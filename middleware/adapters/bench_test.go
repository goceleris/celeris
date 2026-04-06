package adapters

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

func BenchmarkWrapMiddleware(b *testing.B) {
	stdMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
		})
	}
	handler := func(c *celeris.Context) error { return c.NoContent(200) }
	wrapped := WrapMiddleware(stdMW)

	chain := []celeris.HandlerFunc{wrapped, handler}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, _ := celeristest.NewContext("GET", "/",
			celeristest.WithHandlers(chain...),
		)
		_ = ctx.Next()
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkToStdlib(b *testing.B) {
	h := func(c *celeris.Context) error { return c.NoContent(200) }
	stdHandler := ToStdlib(h)

	req := httptest.NewRequest("GET", "/", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		w := httptest.NewRecorder()
		stdHandler.ServeHTTP(w, req)
	}
}
