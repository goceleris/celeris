// Package benchcmp_db compares realistic end-to-end API workloads
// (middleware chain + real DB I/O) across celeris, fiber, echo, chi, and
// net/http stdlib. Every benchmark hits real Redis and/or Postgres, so
// numbers reflect the full stack — not just the router's dispatch cost.
//
// Each framework's server is spun up on a unique loopback port; the
// benchmark drives HTTP requests through the local kernel stack via
// net/http clients so all frameworks pay the same TCP roundtrip cost.
// The only variance should come from framework overhead + driver
// overhead.
//
// Env vars (with sensible defaults):
//
//	CELERIS_REDIS_ADDR   defaults to "127.0.0.1:6379"
//	CELERIS_PG_DSN       defaults to "postgres://celeris:celeris@127.0.0.1:5432/celeristest?sslmode=disable"
//
// If either is unset AND the default doesn't respond, the affected
// benchmark calls b.Skip. No mocks — the whole point is realistic
// numbers.
package benchcmp_db

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// quietLogger discards everything — benchmark output parsers can't
// cope with celeris's startup WARN/INFO lines interleaved with Go's
// bench result lines. Every test server uses this.
var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// liveRedisAddr returns CELERIS_REDIS_ADDR or the default. The caller
// is responsible for skipping when the address does not respond.
func liveRedisAddr() string {
	addr := os.Getenv("CELERIS_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	return addr
}

// livePGDSN returns CELERIS_PG_DSN or the default.
func livePGDSN() string {
	dsn := os.Getenv("CELERIS_PG_DSN")
	if dsn == "" {
		dsn = "postgres://celeris:celeris@127.0.0.1:5432/celeristest?sslmode=disable"
	}
	return dsn
}

// liveMemcachedAddr returns CELERIS_MEMCACHED_ADDR or the default.
func liveMemcachedAddr() string {
	addr := os.Getenv("CELERIS_MEMCACHED_ADDR")
	if addr == "" {
		addr = "127.0.0.1:11211"
	}
	return addr
}

// tcpReachable returns true if a TCP connection to addr can be
// established within 500ms.
func tcpReachable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// skipIfNoRedis calls b.Skip when Redis is unreachable at the
// configured address.
func skipIfNoRedis(b testing.TB) string {
	b.Helper()
	addr := liveRedisAddr()
	if !tcpReachable(addr) {
		b.Skipf("skipping: Redis at %s not reachable", addr)
	}
	return addr
}

// freePort asks the kernel for an available loopback port, returns
// it, and frees the listener — the caller binds the same port via
// the framework server immediately after.
func freePort(b testing.TB) string {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// sharedClient pools clients by address so parallel benchmarks reuse
// keep-alive connections (same policy every framework sees).
var (
	clientMu sync.Mutex
	clients  = make(map[string]*http.Client)
)

func httpClient(addr string) *http.Client {
	clientMu.Lock()
	defer clientMu.Unlock()
	if c, ok := clients[addr]; ok {
		return c
	}
	tr := &http.Transport{
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   128,
		MaxConnsPerHost:       128,
		IdleConnTimeout:       60 * time.Second,
		DisableCompression:    true,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	clients[addr] = c
	return c
}

// waitReady retries GET / until it returns a non-error response or
// the deadline elapses.
func waitReady(b testing.TB, addr string) {
	b.Helper()
	client := httpClient("ready-" + addr)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/health")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.Fatalf("server at %s never responded", addr)
}

// driveClients issues b.N GET requests to URL via goroutines (pooled
// connections) and returns when all complete. Each request body is
// drained — so bench numbers include response reads.
func driveClients(b *testing.B, url string) {
	b.Helper()
	client := httpClient(url)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()

	var total atomic.Int64
	var wg sync.WaitGroup
	work := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range work {
				req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
				resp, err := client.Do(req)
				if err != nil {
					b.Logf("req err: %v", err)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				total.Add(1)
			}
		}()
	}
	for i := 0; i < b.N; i++ {
		work <- struct{}{}
	}
	close(work)
	wg.Wait()
	b.StopTimer()
	if total.Load() != int64(b.N) {
		b.Logf("drove %d requests, expected %d", total.Load(), b.N)
	}
}

// paddedJSON is the response payload shape every scenario returns, so
// response sizes are comparable across frameworks.
type paddedJSON struct {
	OK     bool   `json:"ok"`
	User   string `json:"user"`
	Role   string `json:"role"`
	Cached bool   `json:"cached"`
}

func (p paddedJSON) String() string {
	return fmt.Sprintf(`{"ok":%v,"user":%q,"role":%q,"cached":%v}`, p.OK, p.User, p.Role, p.Cached)
}
