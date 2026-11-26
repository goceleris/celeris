//go:build postgres

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// startCancelHoldingProxy listens on a loopback port and pipes every session to
// the server of dsn, except a CancelRequest (the first message of a connection,
// 16 bytes, code 80877102): that socket is held open and not forwarded, which is
// what a postmaster that is slow to act on a cancel looks like. The driver's
// cancel exchange of an ended context then cannot be confirmed within its
// second, and the conn is failed. It returns dsn pointed at the proxy.
func startCancelHoldingProxy(t *testing.T, dsn string) string {
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
	done := make(chan struct{})
	var wg sync.WaitGroup
	t.Cleanup(func() {
		close(done)
		_ = ln.Close()
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
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = c.Close() }()
				var head [8]byte
				if _, err := io.ReadFull(c, head[:]); err != nil {
					return
				}
				if head[3] == 16 && head[4] == 0x04 && head[5] == 0xd2 && head[6] == 0x16 && head[7] == 0x2e {
					<-done // the cancel request: held, never forwarded
					return
				}
				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer func() { _ = up.Close() }()
				if _, err := up.Write(head[:]); err != nil {
					return
				}
				go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	u.Host = ln.Addr().String()
	return u.String()
}

// The same call as TestPrepareEndedWriteIsNeverSent, with a cancel the server
// never confirms: the conn of the ended call is failed (closed) and discarded.
// The call used to run its fallback on the closed conn and return ErrClosed
// ("connection closed") in place of the context's error. It must return the
// context's error (or 57014), leave no row, and the pool must work afterwards.
func TestPrepareEndedWithAnUnconfirmedCancelReportsTheContext(t *testing.T) {
	dsn := dsnFromEnv(t)
	calls := 4
	if testing.Short() {
		calls = 2
	}
	bg := context.Background()
	for _, mode := range evictModes {
		for _, end := range []string{"deadline", "cancel"} {
			t.Run(mode.name+"/"+end, func(t *testing.T) {
				t.Parallel()
				admin := openDB(t) // one per subtest: each holds a conn while it keeps a lock
				tab := uniqueTableName(t, "prep_unc")
				createTable(t, admin, tab, "id serial, m bigint")
				pool := mode.open(t, startCancelHoldingProxy(t, dsn), 1)
				defer pool.Close()
				if err := pool.Exec(bg, "INSERT INTO "+tab+"(m) VALUES ($1)", int64(-1)); err != nil {
					t.Fatalf("warm-up: %v", err)
				}
				for i := 0; i < calls; i++ {
					tx, err := admin.BeginTx(bg, nil)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(bg, "LOCK TABLE "+tab+" IN ACCESS EXCLUSIVE MODE"); err != nil {
						_ = tx.Rollback()
						t.Fatal(err)
					}
					var ctx context.Context
					var cancel context.CancelFunc
					want := context.Canceled
					if end == "deadline" {
						ctx, cancel = context.WithTimeout(bg, 20*time.Millisecond)
						want = context.DeadlineExceeded
					} else {
						ctx, cancel = context.WithCancel(bg)
						time.AfterFunc(20*time.Millisecond, cancel)
					}
					// The lock is released after the cancel exchange gave up (1 s): the
					// Parse waits for it, so the context ends during the prepare.
					go func() {
						<-ctx.Done()
						time.Sleep(1500 * time.Millisecond)
						_ = tx.Rollback()
					}()
					err = pool.Exec(ctx, fmt.Sprintf("INSERT INTO %s(m) VALUES ($1) -- %d", tab, i), int64(i))
					cancel()
					var pe *postgres.PGError
					switch {
					case err == nil:
						t.Fatalf("call %d returned no error: its Parse was waiting for a lock that was held past its context", i)
					case errors.Is(err, postgres.ErrClosed):
						t.Fatalf("call %d: %v: ErrClosed in place of the context's error", i, err)
					case errors.Is(err, want), errors.As(err, &pe) && pe.Code == "57014":
					default:
						t.Fatalf("call %d: error %v (%T): not the context's (%v)", i, err, err, want)
					}
				}
				// the lock is gone and the pool works
				time.Sleep(1700 * time.Millisecond)
				ctx, cancel := context.WithTimeout(bg, 30*time.Second)
				defer cancel()
				var n int64
				if err := admin.QueryRowContext(ctx, "SELECT count(*) FROM "+tab+" WHERE m >= 0").Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 0 {
					t.Fatalf("%d rows for calls that returned an error: the statement was sent after its context ended", n)
				}
				var m int64
				if err := queryOne(ctx, pool, "SELECT $1::int8", []any{int64(7)}, &m); err != nil || m != 7 {
					t.Fatalf("the pool after the run: m=%d err=%v", m, err)
				}
			})
		}
	}
}
