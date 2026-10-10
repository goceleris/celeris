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
// celeris#759 adds (an atomic add and a deferred one, HTTP/2 only). The
// Cancelable cases give the request a cancelable context, as net/http does;
// HTTP/2 binds the stream's context to it (celeris#949), HTTP/1 does not.
func BenchmarkBridgeServeHTTP(b *testing.B) {
	for _, tc := range []struct {
		name       string
		major      int
		cancelable bool
	}{{"HTTP1", 1, false}, {"HTTP2", 2, false}, {"HTTP1Cancelable", 1, true}, {"HTTP2Cancelable", 2, true}} {
		b.Run(tc.name, func(b *testing.B) {
			e, err := New(resource.Config{Addr: "127.0.0.1:0", Engine: engine.Std, Protocol: engine.HTTP1}, okStreamHandler{})
			if err != nil {
				b.Fatal(err)
			}
			br := &Bridge{engine: e, handler: okStreamHandler{}}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.ProtoMajor = tc.major
			if tc.cancelable {
				// A request served by net/http has a cancelable context
				// (its connection's, then its own): the binding of the
				// stream's context to it (celeris#949) registers with it.
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				req = req.WithContext(ctx)
			}
			w := &discardResponseWriter{h: http.Header{}}
			b.ReportAllocs()
			for b.Loop() {
				clear(w.h)
				br.ServeHTTP(w, req)
			}
		})
	}
}

// BenchmarkH2CFrontServeHTTP measures one HTTP/1.1 request through the whole
// handler chain of an H2C engine: the h2c front end's checks (is this the
// preface? an upgrade?) and then the bridge. Every request on an H2C listener
// pays them (celeris#878 replaced x/net's h2c handler with h2cHandler).
func BenchmarkH2CFrontServeHTTP(b *testing.B) {
	e, err := New(resource.Config{Addr: "127.0.0.1:0", Engine: engine.Std, Protocol: engine.H2C}, okStreamHandler{})
	if err != nil {
		b.Fatal(err)
	}
	h := e.server.Handler
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := &discardResponseWriter{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		h.ServeHTTP(w, req)
	}
}
