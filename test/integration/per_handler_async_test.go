//go:build linux

package integration

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// registerMixedAsyncRoutes wires a mix of sync / async / override routes
// used to validate per-handler dispatch on the native Linux engines.
func registerMixedAsyncRoutes(srv *celeris.Server) {
	srv.GET("/cpu", func(c *celeris.Context) error { return c.String(http.StatusOK, "cpu") })       // sync (default)
	srv.GET("/db", func(c *celeris.Context) error { return c.String(http.StatusOK, "db") }).Async() // async
	srv.GET("/cached", func(c *celeris.Context) error { return c.String(http.StatusOK, "cached") }).Async(false)
	srv.GET("/users/:id", func(c *celeris.Context) error {
		return c.String(http.StatusOK, "user:%s", c.Param("id"))
	}).Async()
	api := srv.Group("/api").Async()
	api.GET("/products", func(c *celeris.Context) error { return c.String(http.StatusOK, "products") })
	api.GET("/static", func(c *celeris.Context) error { return c.String(http.StatusOK, "static") }).Async(false)
}

var mixedAsyncCases = []struct{ path, want string }{
	{"/cpu", "cpu"},
	{"/db", "db"},
	{"/cached", "cached"},
	{"/users/42", "user:42"},
	{"/api/products", "products"},
	{"/api/static", "static"},
}

func startAsyncServer(t *testing.T, eng celeris.EngineType, async bool) (*celeris.Server, string, string) {
	t.Helper()
	srv := celeris.New(celeris.Config{
		Addr:          "127.0.0.1:0",
		Engine:        eng,
		Protocol:      celeris.Auto, // Auto enables h2c upgrade in WithDefaults
		AsyncHandlers: async,
	})
	registerMixedAsyncRoutes(srv)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.StartWithListener(ln) }()

	deadline := time.Now().Add(5 * time.Second)
	for srv.Addr() == nil && time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("server exited early: %v", err)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if srv.Addr() == nil {
		t.Fatal("server never bound within deadline")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-done
	})
	addr := srv.Addr().String()
	return srv, "http://" + addr, addr
}

func assertRoutes(t *testing.T, client *http.Client, base, proto string) {
	t.Helper()
	for _, tc := range mixedAsyncCases {
		resp, err := client.Get(base + tc.path)
		if err != nil {
			t.Errorf("[%s] GET %s: %v", proto, tc.path, err)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(b) != tc.want {
			t.Errorf("[%s] %s: got (%d,%q), want (200,%q)", proto, tc.path, resp.StatusCode, string(b), tc.want)
		}
	}
}

// TestPerHandlerAsync_Engines verifies mixed sync/async routes return correct
// responses over BOTH HTTP/1.1 and h2c, on BOTH native Linux engines, with
// both server defaults. This is the cross-engine × cross-protocol correctness
// gate for per-handler dispatch (#300); the perf differential is covered by
// the H2 processor unit tests and the probatorium matrix.
func TestPerHandlerAsync_Engines(t *testing.T) {
	engines := []struct {
		name string
		eng  celeris.EngineType
	}{
		{"iouring", celeris.IOUring},
		{"epoll", celeris.Epoll},
	}
	for _, e := range engines {
		for _, async := range []bool{false, true} {
			label := e.name + "-syncDefault"
			if async {
				label = e.name + "-asyncDefault"
			}
			t.Run(label, func(t *testing.T) {
				srv, base, addr := startAsyncServer(t, e.eng, async)
				_ = srv

				// HTTP/1.1
				h1 := &http.Client{Timeout: 5 * time.Second}
				assertRoutes(t, h1, base, "h1")

				// h2c (prior knowledge)
				h2 := h2cClient(addr)
				assertRoutes(t, h2, base, "h2c")
			})
		}
	}
}

// TestPerHandlerAsync_ConcurrentMixed hammers sync + async routes
// concurrently over both protocols on each engine; asserts zero response
// cross-talk (no per-request state bleed across the dispatch boundary).
func TestPerHandlerAsync_ConcurrentMixed(t *testing.T) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{
		{"iouring", celeris.IOUring},
		{"epoll", celeris.Epoll},
	} {
		t.Run(e.name, func(t *testing.T) {
			_, base, addr := startAsyncServer(t, e.eng, false)
			h1 := &http.Client{Timeout: 5 * time.Second}
			h2 := h2cClient(addr)
			var wg sync.WaitGroup
			var bad atomic.Int64
			for i := 0; i < 300; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					tc := mixedAsyncCases[i%len(mixedAsyncCases)]
					client := h1
					if i%2 == 0 {
						client = h2
					}
					resp, err := client.Get(base + tc.path)
					if err != nil {
						bad.Add(1)
						return
					}
					b, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != http.StatusOK || string(b) != tc.want {
						bad.Add(1)
					}
				}(i)
			}
			wg.Wait()
			if n := bad.Load(); n != 0 {
				t.Fatalf("[%s] %d concurrent responses mismatched", e.name, n)
			}
		})
	}
}

// TestPerHandlerAsync_StickinessMetric verifies the engine-level
// asyncPromoted counter (#300 G3) increments exactly once per fresh
// async-mode H1 connection that touches an async route, and stays
// monotonic across many requests on the same conn (stickiness).
func TestPerHandlerAsync_StickinessMetric(t *testing.T) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{
		{"iouring", celeris.IOUring},
		{"epoll", celeris.Epoll},
	} {
		t.Run(e.name, func(t *testing.T) {
			srv, _, addr := startAsyncServer(t, e.eng, false /* sync default */)
			info := srv.EngineInfo()
			if info == nil {
				t.Skip("engine not running")
			}
			if info.Metrics.AsyncRoutes == 0 {
				t.Fatalf("expected AsyncRoutes>0 for mixed-routes server; got %d", info.Metrics.AsyncRoutes)
			}
			base0 := srv.EngineInfo().Metrics.AsyncPromotedConns

			// One keep-alive client → many requests; promotion should
			// fire ONCE on the first /db hit and not again.
			client := &http.Client{Transport: &http.Transport{
				MaxIdleConnsPerHost: 1,
				DisableKeepAlives:   false,
			}}
			defer client.CloseIdleConnections()

			// First request is sync — must NOT promote.
			if _, err := client.Get("http://" + addr + "/cpu"); err != nil {
				t.Fatalf("get /cpu: %v", err)
			}
			time.Sleep(50 * time.Millisecond)
			if got := srv.EngineInfo().Metrics.AsyncPromotedConns - base0; got != 0 {
				t.Fatalf("sync-only request must NOT promote: delta=%d", got)
			}
			// First async hit on this conn — must promote exactly once.
			if _, err := client.Get("http://" + addr + "/db"); err != nil {
				t.Fatalf("get /db: %v", err)
			}
			time.Sleep(50 * time.Millisecond)
			if got := srv.EngineInfo().Metrics.AsyncPromotedConns - base0; got != 1 {
				t.Fatalf("first async hit must promote exactly once: delta=%d", got)
			}
			// Subsequent requests on the SAME conn (sync or async) must
			// not promote again — sticky.
			for i := 0; i < 5; i++ {
				_, _ = client.Get("http://" + addr + "/db")
				_, _ = client.Get("http://" + addr + "/cpu")
			}
			time.Sleep(100 * time.Millisecond)
			if got := srv.EngineInfo().Metrics.AsyncPromotedConns - base0; got != 1 {
				t.Fatalf("promotion must be sticky: delta=%d after 10 more reqs, want 1", got)
			}
		})
	}
}

// TestPerHandlerAsync_KeepAliveOrdering walks 12 pipelined-but-sequential
// requests with a sync/async/sync/async pattern over ONE keep-alive
// connection and asserts response ordering on iouring + epoll. Catches
// any regression in the inline-flush-then-promote handoff (#300 L1).
func TestPerHandlerAsync_KeepAliveOrdering(t *testing.T) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{
		{"iouring", celeris.IOUring},
		{"epoll", celeris.Epoll},
	} {
		t.Run(e.name, func(t *testing.T) {
			_, _, addr := startAsyncServer(t, e.eng, false)
			tr := &http.Transport{MaxIdleConnsPerHost: 1}
			client := &http.Client{Transport: tr}
			defer client.CloseIdleConnections()

			seq := []string{"/cpu", "/db", "/cached", "/db", "/cpu", "/api/products",
				"/api/static", "/db", "/users/42", "/cpu", "/db", "/api/products"}
			wantByPath := map[string]string{
				"/cpu": "cpu", "/db": "db", "/cached": "cached",
				"/api/products": "products", "/api/static": "static",
				"/users/42": "user:42",
			}
			for i, p := range seq {
				resp, err := client.Get("http://" + addr + p)
				if err != nil {
					t.Fatalf("[%s] req %d %s: %v", e.name, i, p, err)
				}
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("[%s] req %d %s: status=%d", e.name, i, p, resp.StatusCode)
				}
				if got := string(b); got != wantByPath[p] {
					t.Fatalf("[%s] req %d %s: got=%q want=%q", e.name, i, p, got, wantByPath[p])
				}
			}
		})
	}
}
