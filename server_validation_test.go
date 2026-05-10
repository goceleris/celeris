//go:build validation

package celeris

import (
	"context"
	"testing"

	"github.com/goceleris/celeris/validation"
)

// TestServerPanicBumpsValidationCounter is the integration version of
// the synthetic panic check. It exercises the same safety net as
// TestServerPanicRecovery, then verifies the cross-package wiring
// into validation.PanicCount.
func TestServerPanicBumpsValidationCounter(t *testing.T) {
	before := validation.PanicCount.Load()

	s := New(Config{})
	s.GET("/panic", func(_ *Context) error {
		panic("handler panic — validation soak fixture")
	})
	adapter := &routerAdapter{server: s}

	st, rw := newTestStream("GET", "/panic")
	if err := adapter.HandleStream(context.Background(), st); err != nil {
		t.Fatalf("HandleStream: %v", err)
	}
	if rw.status != 500 {
		t.Fatalf("status: got %d, want 500", rw.status)
	}
	st.Release()

	after := validation.PanicCount.Load()
	// Use >=, not equality: the counter is process-global and other
	// concurrent tests may also bump it. We only need to observe at
	// least one increment caused by this test.
	if after < before+1 {
		t.Fatalf("validation.PanicCount: got %d, want >=%d (before=%d)", after, before+1, before)
	}
}
