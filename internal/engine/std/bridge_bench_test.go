package std

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

type okStreamHandler struct{}

func (okStreamHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
}

type discardResponseWriter struct{ h http.Header }

func (w *discardResponseWriter) Header() http.Header         { return w.h }
func (w *discardResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardResponseWriter) WriteHeader(int)             {}

// BenchmarkBridgeServeHTTP measures one request through the std bridge, over
// HTTP/1.1 and over HTTP/2 (an h2c stream), whose in-handler count
// celeris#759 adds (an atomic add and a deferred one, HTTP/2 only).
func BenchmarkBridgeServeHTTP(b *testing.B) {
	for _, tc := range []struct {
		name  string
		major int
	}{{"HTTP1", 1}, {"HTTP2", 2}} {
		b.Run(tc.name, func(b *testing.B) {
			e, err := New(resource.Config{Addr: "127.0.0.1:0", Engine: engine.Std, Protocol: engine.HTTP1}, okStreamHandler{})
			if err != nil {
				b.Fatal(err)
			}
			br := &Bridge{engine: e, handler: okStreamHandler{}}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.ProtoMajor = tc.major
			w := &discardResponseWriter{h: http.Header{}}
			b.ReportAllocs()
			for b.Loop() {
				clear(w.h)
				br.ServeHTTP(w, req)
			}
		})
	}
}
