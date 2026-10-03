package celeris

import (
	"context"
	"testing"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

// discardRW852 accepts a response and keeps nothing.
type discardRW852 struct{}

func (discardRW852) WriteResponse(*stream.Stream, int, [][2]string, []byte) error { return nil }

// BenchmarkUnmatched852 measures a matched route (whose path celeris#852 does
// not change) and the 404 / 405 answers, on an adapter built the way Start
// builds it, with no global middleware and with three that call Next (what
// an unmatched request now runs, by design), under the sync default and
// under AsyncHandlers (where the unmatched chain is timed like an adaptive
// route until it settles; the loop measures the settled state).
func BenchmarkUnmatched852(b *testing.B) {
	pass := func(c *Context) error { return c.Next() }
	for _, v := range []struct {
		name  string
		mw    int
		async bool
	}{{"middleware=0", 0, false}, {"middleware=3", 3, false}, {"async/middleware=3", 3, true}} {
		s := New(Config{AsyncHandlers: v.async})
		for range v.mw {
			s.Use(pass)
		}
		s.GET("/exists", func(c *Context) error { return c.String(200, "ok") })
		ra := &routerAdapter{server: s}
		ra.buildUnmatchedChains()
		for _, rq := range []struct{ name, method, path string }{
			{"route-200", "GET", "/exists"},
			{"404", "GET", "/missing"},
			{"405", "POST", "/exists"},
		} {
			b.Run(rq.name+"/"+v.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					st, _ := newTestStream(rq.method, rq.path)
					st.ResponseWriter = discardRW852{}
					_ = ra.HandleStream(context.Background(), st)
					st.Release()
				}
			})
		}
	}
}

// BenchmarkRouteAsync852 measures the engines' per-request dispatch question
// (RouteAsync, asked before the handler runs on an inline H1 connection or H2
// stream under AsyncHandlers) for a static route, a parameterised route and
// an unmatched path, with three global middlewares: the case where celeris#852
// makes an unmatched request's answer depend on the unmatched chain.
func BenchmarkRouteAsync852(b *testing.B) {
	pass := func(c *Context) error { return c.Next() }
	s := New(Config{AsyncHandlers: true})
	for range 3 {
		s.Use(pass)
	}
	s.GET("/exists", func(c *Context) error { return nil })
	s.GET("/users/:id", func(c *Context) error { return nil })
	ra := &routerAdapter{server: s}
	ra.buildUnmatchedChains()
	for _, rq := range []struct{ name, method, path string }{
		{"static", "GET", "/exists"},
		{"param", "GET", "/users/42"},
		{"unmatched", "GET", "/missing/path"},
	} {
		b.Run(rq.name, func(b *testing.B) {
			b.ReportAllocs()
			var n int
			for b.Loop() {
				if ra.RouteAsync(rq.method, rq.path) {
					n++
				}
			}
			_ = n
		})
	}
}
