package conn

import (
	"context"
	"sync/atomic"
	"testing"
)

// BenchmarkProcessH1RespondFlag884 is BenchmarkProcessH1Respond884 with the
// engines' shutdown flag wired and down (H1State.Shutdown): the per-response
// cost of the nil test and the atomic load the contract of celeris#884 / #885
// added, against the same arms without the flag.
func BenchmarkProcessH1RespondFlag884(b *testing.B) {
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
			var down atomic.Bool
			st.Shutdown = &down
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
