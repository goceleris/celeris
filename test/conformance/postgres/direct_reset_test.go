//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"
)

// A pool whose connections were all cut by the server side runs on fresh
// connections after at most one failed call per dead connection (celeris#1463).
//
// A direct-mode Pool kept a connection after a write into its reset socket
// failed and handed the same dead socket out again until the 30 s health check
// closed it. The two tests cut the connections of a real postgres:16 in the two
// ways a Go test can:
//
//   - TestPoolRunsOnFreshConnsAfterAResetProxy: the pool talks to the server
//     through a TCP proxy that resets (RST, SetLinger(0)) or closes (FIN) both
//     sockets of every session on demand: what a server host that dies or a
//     firewall that resets looks like to the client.
//   - TestPoolRunsOnFreshConnsAfterPgTerminateBackend: the server ends the
//     sessions itself (pg_terminate_backend): a FATAL error and a close.
//
// A restart of the server (docker restart / kill) is the manual form of the
// first; it cannot run inside a go test.

// resetProxy pipes every session to the server of dsn and cuts them on demand.
type resetProxy struct {
	dsn string // dsn pointed at the proxy

	mu    sync.Mutex
	pairs [][2]net.Conn // client side, server side
}

func startResetProxy(t *testing.T, dsn string) *resetProxy {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	target := u.Host
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &resetProxy{}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		p.cut(false)
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.pairs = append(p.pairs, [2]net.Conn{c, up})
			p.mu.Unlock()
			wg.Add(2)
			go func() { defer wg.Done(); _, _ = io.Copy(up, c); _ = up.Close() }()
			go func() { defer wg.Done(); _, _ = io.Copy(c, up); _ = c.Close() }()
		}
	}()
	u.Host = ln.Addr().String()
	p.dsn = u.String()
	return p
}

// cut closes every session: the client side with a RST when rst is set, with a
// FIN otherwise. It returns the number of sessions it cut.
func (p *resetProxy) cut(rst bool) int {
	p.mu.Lock()
	pairs := p.pairs
	p.pairs = nil
	p.mu.Unlock()
	for _, pr := range pairs {
		if tc, ok := pr[0].(*net.TCPConn); ok && rst {
			_ = tc.SetLinger(0)
		}
		_ = pr[0].Close()
		_ = pr[1].Close()
	}
	return len(pairs)
}

// poisonRun runs calls on db after the pool's conns were cut: each of the n dead
// conns may fail one call, then every call succeeds.
func poisonRun(t *testing.T, db stressDB, n int) {
	t.Helper()
	const calls = 12
	var failed []error
	lastFail := -1
	for i := 0; i < calls; i++ {
		time.Sleep(20 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var err error
		if i%2 == 0 {
			err = db.Exec(ctx, "SELECT 1")
		} else {
			var m int64
			err = queryOne(ctx, db, "SELECT $1::int8", []any{int64(i)}, &m)
		}
		cancel()
		if err != nil {
			failed = append(failed, err)
			lastFail = i
		}
	}
	if len(failed) > n {
		t.Fatalf("%d of %d calls failed on a pool of %d cut conns (at most one per conn): %v", len(failed), calls, n, failed)
	}
	if lastFail >= n {
		t.Fatalf("a call failed at position %d: the pool kept running on a dead conn after the first %d calls: %v", lastFail+1, n, failed)
	}
	t.Logf("%d of %d calls failed on %d cut conns: %v", len(failed), calls, n, failed)
}

// poisonWarmPool opens n conns of db with n concurrent one-second-ish queries.
func poisonWarmPool(t *testing.T, db stressDB, n int) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var m int64
			errs <- queryOne(ctx, db, "SELECT pg_sleep(0.2)::text IS NOT NULL AS s, $1::int8", []any{int64(1)}, new(bool), &m)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("warm-up: %v", err)
		}
	}
}

func TestPoolRunsOnFreshConnsAfterAResetProxy(t *testing.T) {
	dsn := dsnFromEnv(t)
	const n = 3
	for _, mode := range evictModes {
		for _, how := range []string{"rst", "fin"} {
			t.Run(mode.name+"/"+how, func(t *testing.T) {
				p := startResetProxy(t, dsn)
				db := mode.open(t, p.dsn, n)
				defer db.Close()
				poisonWarmPool(t, db, n)
				if got := p.cut(how == "rst"); got < n {
					t.Fatalf("cut %d sessions, want at least %d", got, n)
				}
				poisonRun(t, db, n)
			})
		}
	}
}

func TestPoolRunsOnFreshConnsAfterPgTerminateBackend(t *testing.T) {
	dsn := dsnFromEnv(t)
	const n = 3
	admin := openDB(t)
	for _, mode := range evictModes {
		t.Run(mode.name, func(t *testing.T) {
			app := fmt.Sprintf("celeris_poison_%s_%d", mode.name, time.Now().UnixNano())
			db := mode.open(t, withParams(dsn, "application_name="+app), n)
			defer db.Close()
			poisonWarmPool(t, db, n)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if got := terminateBackends(ctx, t, admin, app); got < n {
				t.Fatalf("terminated %d backends, want at least %d", got, n)
			}
			poisonRun(t, db, n)
		})
	}
}

// terminateBackends ends every backend whose application_name is app.
func terminateBackends(ctx context.Context, t *testing.T, admin *sql.DB, app string) int {
	t.Helper()
	var n int
	if err := admin.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = $1) s", app).Scan(&n); err != nil {
		t.Fatalf("pg_terminate_backend: %v", err)
	}
	return n
}
