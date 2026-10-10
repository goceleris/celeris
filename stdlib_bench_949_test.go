package celeris

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type toHandlerBenchRW struct{ h http.Header }

func (w *toHandlerBenchRW) Header() http.Header         { return w.h }
func (w *toHandlerBenchRW) Write(p []byte) (int, error) { return len(p), nil }
func (w *toHandlerBenchRW) WriteHeader(int)             {}

// BenchmarkToHandler measures one request through ToHandler, whose
// c.Context() ends with r.Context() (celeris#949) for HTTP/1 and HTTP/2
// alike. Background: a request context that can never end (httptest's).
// Cancelable: one cancelable context for every iteration. Fresh: a new
// cancelable context per request, as net/http gives every request (the first
// registration with a parent allocates its children map). PreCancelled: the
// context is over before the handler starts.
func BenchmarkToHandler(b *testing.B) {
	h := ToHandler(func(c *Context) error { return c.NoContent(200) })
	base := httptest.NewRequest(http.MethodGet, "/", nil)
	w := &toHandlerBenchRW{h: http.Header{}}
	b.Run("Background", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			clear(w.h)
			h.ServeHTTP(w, base)
		}
	})
	b.Run("Cancelable", func(b *testing.B) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := base.WithContext(ctx)
		b.ReportAllocs()
		for b.Loop() {
			clear(w.h)
			h.ServeHTTP(w, req)
		}
	})
	for _, pre := range []bool{false, true} {
		name := "Fresh"
		if pre {
			name = "PreCancelled"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ctx, cancel := context.WithCancel(context.Background())
				if pre {
					cancel()
				}
				clear(w.h)
				h.ServeHTTP(w, base.WithContext(ctx))
				cancel()
			}
		})
	}
}
