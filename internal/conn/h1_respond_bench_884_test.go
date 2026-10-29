package conn

import (
	"context"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// respondHandler884 answers every request through the H1 adapter's
// WriteResponse, as celeris.Context does: the path the shutdown contract of
// celeris#884 / #885 (every response after Shutdown carries Connection: close)
// touches at the head build and after the handler.
type respondHandler884 struct {
	status  int
	headers [][2]string
	body    []byte
}

func (h respondHandler884) HandleStream(_ context.Context, s *stream.Stream) error {
	return s.ResponseWriter.WriteResponse(s, h.status, h.headers, h.body)
}

// BenchmarkProcessH1Respond884 is the per-request cost of ProcessH1 with a
// handler that writes a response, on a keep-alive connection that is not
// shutting down: the figure the shutdown flag (a nil test and one atomic load
// per response) must leave alone. This file compiles on main, so the same file
// runs on main and on the branch for the A/B; the arms that wire the flag are in
// h1_respond_flag_bench_884_test.go.
func BenchmarkProcessH1Respond884(b *testing.B) {
	body := []byte(`{"ok":true}`)
	arms := []struct {
		name string
		req  string
		h    respondHandler884
	}{
		{"fused-200-json", "GET /x HTTP/1.1\r\nHost: a\r\n\r\n", respondHandler884{200, [][2]string{{"content-type", "application/json"}, {"content-length", "11"}}, body}},
		{"general-200-3hdr", "GET /x HTTP/1.1\r\nHost: a\r\n\r\n", respondHandler884{200, [][2]string{{"content-type", "text/plain"}, {"x-a", "1"}, {"x-b", "2"}}, body}},
		{"head-200", "HEAD /x HTTP/1.1\r\nHost: a\r\n\r\n", respondHandler884{200, [][2]string{{"content-type", "text/plain"}}, body}},
		{"204", "GET /x HTTP/1.1\r\nHost: a\r\n\r\n", respondHandler884{204, nil, nil}},
	}
	for _, arm := range arms {
		b.Run(arm.name, func(b *testing.B) {
			st := NewH1State()
			w := func([]byte) {}
			ctx := context.Background()
			req := []byte(arm.req)
			b.SetBytes(int64(len(req)))
			b.ReportAllocs()
			for b.Loop() {
				if err := ProcessH1(ctx, req, st, arm.h, w); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
