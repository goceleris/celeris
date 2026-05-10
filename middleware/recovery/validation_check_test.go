//go:build validation

package recovery_test

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/internal/testutil"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/validation"
)

// TestPanicBumpsValidationCounter verifies the recovery middleware
// installs the validation.RecordPanic hook on the recover path: a
// real panic from a celeris handler under the middleware bumps
// PanicCount by at least one.
func TestPanicBumpsValidationCounter(t *testing.T) {
	before := validation.PanicCount.Load()

	mw := recovery.New()
	handler := func(_ *celeris.Context) error { panic("recovery validation hook") }
	chain := []celeris.HandlerFunc{mw, handler}
	rec, err := testutil.RunChain(t, chain, "GET", "/panic")
	// The default ErrorHandler writes a 500 response and returns nil;
	// the panic is fully consumed by the middleware. Either nil or a
	// non-nil error is acceptable — the only invariant we care about
	// here is the status code and the counter bump.
	_ = err
	if rec.StatusCode != 500 {
		t.Fatalf("status: got %d, want 500", rec.StatusCode)
	}

	after := validation.PanicCount.Load()
	if after < before+1 {
		t.Fatalf("validation.PanicCount: got %d, want >=%d (before=%d)", after, before+1, before)
	}
}

// TestNoPanicLeavesCounterAlone confirms the healthy path doesn't
// touch the counter — bumping on every request would defeat the
// "panic escaped recovery" predicate.
func TestNoPanicLeavesCounterAlone(t *testing.T) {
	before := validation.PanicCount.Load()

	mw := recovery.New()
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	chain := []celeris.HandlerFunc{mw, handler}
	if _, err := testutil.RunChain(t, chain, "GET", "/ok"); err != nil {
		t.Fatalf("RunChain: %v", err)
	}

	after := validation.PanicCount.Load()
	if after != before {
		t.Fatalf("validation.PanicCount: got %d, want %d (no panic should not bump)", after, before)
	}
}
