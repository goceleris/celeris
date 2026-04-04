package testutil

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// RunMiddleware creates a test context with GET / and runs the middleware.
// c.Next() returns nil (no-op handler) for isolated middleware testing.
func RunMiddleware(t *testing.T, mw celeris.HandlerFunc, opts ...celeristest.Option) (*celeristest.ResponseRecorder, error) {
	t.Helper()
	return RunMiddlewareWithMethod(t, mw, "GET", "/", opts...)
}

// RunMiddlewareWithMethod creates a test context with the given method and path,
// then runs the middleware.
func RunMiddlewareWithMethod(t *testing.T, mw celeris.HandlerFunc, method, path string, opts ...celeristest.Option) (*celeristest.ResponseRecorder, error) {
	t.Helper()
	ctx, rec := celeristest.NewContextT(t, method, path, opts...)
	err := mw(ctx)
	return rec, err
}

// RunChain creates a test context with the given handler chain and executes it.
// The first handler in the chain is called, and each c.Next() progresses through
// the remaining handlers.
func RunChain(t *testing.T, chain []celeris.HandlerFunc, method, path string, opts ...celeristest.Option) (*celeristest.ResponseRecorder, error) {
	t.Helper()
	if len(chain) == 0 {
		t.Fatal("RunChain: chain must not be empty")
	}
	allOpts := make([]celeristest.Option, 0, len(opts)+1)
	allOpts = append(allOpts, celeristest.WithHandlers(chain...))
	allOpts = append(allOpts, opts...)
	ctx, rec := celeristest.NewContextT(t, method, path, allOpts...)
	err := ctx.Next()
	return rec, err
}
