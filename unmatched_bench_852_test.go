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
// an unmatched request now runs, by design).
func BenchmarkUnmatched852(b *testing.B) {
	pass := func(c *Context) error { return c.Next() }
	for _, mw := range []int{0, 3} {
		s := New(Config{})
		for range mw {
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
			name := rq.name + "/middleware=" + map[int]string{0: "0", 3: "3"}[mw]
			b.Run(name, func(b *testing.B) {
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
