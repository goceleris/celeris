package ratelimit

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// TestSkipFailedRefundsAPanic921: celeris#921's family. With
// SkipFailedRequests, the token of a request whose handler returns a status
// >= 400 is refunded after c.Next, but one whose handler panicked (which the
// recovery middleware answers 500) was not: the refund ran only on a normal
// return, so each panic spent a token and a panicking route ran its clients
// into 429. A panic is now refunded as a failed request, and the panic
// continues. The returned-500 arm is the control. Both limiters, the
// in-memory one and a Store with Undo.
func TestSkipFailedRefundsAPanic921(t *testing.T) {
	for _, backend := range []string{"memory", "store"} {
		for _, mode := range []string{"returns-500", "panics"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				cfg := Config{RPS: 0.0001, Burst: 2, SkipFailedRequests: true, KeyFunc: func(*celeris.Context) string { return "k" }}
				if backend == "store" {
					cfg.Store = newMockStore(2)
				}
				mw := New(cfg)
				failing := func(c *celeris.Context) error {
					if mode == "panics" {
						panic("boom")
					}
					return celeris.NewHTTPError(500, "down")
				}
				for i := range 3 {
					c, _ := celeristest.NewContext("GET", "/", celeristest.WithHandlers(mw, failing))
					var p any
					func() { defer func() { p = recover() }(); _ = c.Next() }()
					celeristest.ReleaseContext(c)
					if mode == "panics" && p != "boom" {
						t.Fatalf("failed request %d: recovered %v, want the handler's own panic", i, p)
					}
				}
				c, rec := celeristest.NewContext("GET", "/", celeristest.WithHandlers(mw, func(c *celeris.Context) error { return c.String(200, "ok") }))
				defer celeristest.ReleaseContext(c)
				if err := c.Next(); err != nil || rec.StatusCode != 200 {
					t.Errorf("after 3 failed requests (burst 2, SkipFailedRequests) a healthy request got %d, err %v; want 200", rec.StatusCode, err)
				}
			})
		}
	}
}
