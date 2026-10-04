//go:build linux

package cache_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/cache"
)

// TestCachePanickingFillDoesNotWedgeTheServer921 is celeris#921 on every
// engine. cache.New()'s coalesced fill panics once, on the key's first
// handler run; celeris recovers the panic and answers that request with a
// 500. The panic left the key's coalesced call in place, so every later
// request for the key joined it and waited forever, and on epoll, io_uring
// and adaptive each such request held its event loop: requests that share
// nothing with the key went unanswered, and Start did not return after
// Shutdown. Now 8 requests for the key, then 8 for /ping, each on a fresh
// HTTP/1.1 connection, must all be answered with 200, and Start must return
// (startServer913's cleanup checks that).
func TestCachePanickingFillDoesNotWedgeTheServer921(t *testing.T) {
	for _, e := range engines913 {
		t.Run(e.name, func(t *testing.T) {
			var runs atomic.Int32
			addr := startServer913(t, e.eng, 0, func(s *celeris.Server) {
				s.GET("/k", cache.New(), func(c *celeris.Context) error {
					if runs.Add(1) == 1 {
						panic("the fill panicked")
					}
					return c.String(200, "v")
				})
			})
			var wg sync.WaitGroup
			first := make([]answer913, 1)
			fetchAll913(&wg, first, "http://"+addr+"/k", 2*time.Second, "x-cache")
			wg.Wait()
			if a := first[0]; a.err != nil || a.status != 500 {
				t.Errorf("the request whose handler panicked: status %d err %v, want 500", a.status, a.err)
			}
			key := make([]answer913, 8)
			others := make([]answer913, 8)
			fetchAll913(&wg, key, "http://"+addr+"/k", 2*time.Second, "x-cache")
			time.Sleep(100 * time.Millisecond)
			fetchAll913(&wg, others, "http://"+addr+"/ping", 2*time.Second, "")
			wg.Wait()
			check := func(what string, as []answer913, body string) {
				t.Helper()
				ok := 0
				for i, a := range as {
					switch {
					case a.err != nil:
						t.Errorf("%s %d: no response after %v: %v", what, i, a.took, a.err)
					case a.status != 200 || string(a.body) != body:
						t.Errorf("%s %d: %d %q, want 200 %q", what, i, a.status, a.body, body)
					default:
						ok++
					}
				}
				t.Logf("%s: %d of %d answered", what, ok, len(as))
			}
			check("requests for the key after the panic", key, "v")
			check("requests for /ping", others, "ok")
			t.Logf("handler runs %d", runs.Load())
		})
	}
}
