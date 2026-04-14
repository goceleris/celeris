package adapters_test

import (
	"net/http"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/adapters"
)

// TestWrapMiddleware_PanicPropagation verifies that a panic raised by
// the celeris chain inside the wrapped stdlib middleware is re-raised
// by WrapMiddleware after wrapped.ServeHTTP returns, so celeris
// middleware/recovery can catch it. Without M12's fix, a stdlib
// middleware with its own recover() would swallow the panic.
func TestWrapMiddleware_PanicPropagation(t *testing.T) {
	// Stdlib middleware that wraps inner with its own recover() — would
	// swallow panics if WrapMiddleware did not pre-catch them.
	swallowingMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { _ = recover() }()
			next.ServeHTTP(w, r)
		})
	}
	mw := adapters.WrapMiddleware(swallowingMW)
	panicker := func(c *celeris.Context) error {
		panic("boom from celeris handler")
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic to propagate to celeris-side caller; got swallowed by stdlib middleware's recover")
		}
		if r != "boom from celeris handler" {
			t.Errorf("panic value = %v, want \"boom from celeris handler\"", r)
		}
	}()

	ctx, _ := celeristest.NewContext("GET", "/",
		celeristest.WithHandlers(mw, panicker),
	)
	_ = ctx.Next()
	celeristest.ReleaseContext(ctx)
}

// (Header propagation is covered by the existing
// TestWrapMiddlewareHeaderPropagation in adapters_test.go.)
