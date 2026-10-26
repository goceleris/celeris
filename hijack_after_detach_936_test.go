package celeris

import (
	"errors"
	"testing"
)

// celeris#936 (the residual of #1105): nothing refused Context.Hijack after
// Context.Detach. After Detach the engine's dispatch goroutine no longer holds
// the connection's lock across the handler, so the hijack's close of the
// descriptor runs against flushes and re-arms that rely on that lock. A
// detached stream is the middleware's (a WebSocket or SSE writer's) and a
// hijacked connection is the caller's: it cannot be both, so the second
// takeover is refused and the connection stays as Detach left it.

// TestHijackAfterDetachIsRefused: Detach, then Hijack, on a writer that could
// hijack. The engine must not be asked, and no connection comes back.
func TestHijackAfterDetachIsRefused(t *testing.T) {
	c, w, _ := newHijackDetachContext(t)
	done := c.Detach()
	defer done()

	conn, err := c.Hijack()
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, ErrHijackAfterDetach) {
		t.Errorf("Hijack after Detach = (conn %v, %v), want ErrHijackAfterDetach", conn != nil, err)
	}
	if got := w.hijacks.Load(); got != 0 {
		t.Errorf("the engine's Hijack ran %d times after Detach, want 0", got)
	}
	if c.hijacked {
		t.Error("the Context is marked hijacked after a refused Hijack")
	}
}

// TestHijackWithoutDetachStillSucceeds is the control: the same Context and
// writer, no Detach.
func TestHijackWithoutDetachStillSucceeds(t *testing.T) {
	c, w, _ := newHijackDetachContext(t)
	conn, err := c.Hijack()
	if err != nil {
		t.Fatalf("Hijack: %v", err)
	}
	_ = conn.Close()
	if got := w.hijacks.Load(); got != 1 {
		t.Errorf("the engine's Hijack ran %d times, want 1", got)
	}
}
