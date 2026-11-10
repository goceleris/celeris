package celeris_test

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/internal/conn"
)

// TestErrStreamRefusedIsTheEnginesSentinel884 pins celeris#884 item 77: the
// error a StreamWriter write returns at the engine's 64 MiB limit is
// conn.ErrWriteRefused itself, exported as celeris.ErrStreamRefused, so a
// caller outside the module can match it with errors.Is (or ==) and the
// engines' own errors.Is checks match the same value. It is not a gone peer:
// it does not wrap net.ErrClosed, which conn.ErrConnClosed does.
func TestErrStreamRefusedIsTheEnginesSentinel884(t *testing.T) {
	if celeris.ErrStreamRefused != conn.ErrWriteRefused {
		t.Fatalf("celeris.ErrStreamRefused = %p, want the same value as conn.ErrWriteRefused (%p), not a wrapper", celeris.ErrStreamRefused, conn.ErrWriteRefused)
	}
	if !errors.Is(conn.ErrWriteRefused, celeris.ErrStreamRefused) || !errors.Is(celeris.ErrStreamRefused, conn.ErrWriteRefused) {
		t.Fatal("errors.Is does not match across the two names")
	}
	wrapped := fmt.Errorf("handler: %w", conn.ErrWriteRefused)
	if !errors.Is(wrapped, celeris.ErrStreamRefused) {
		t.Fatal("a wrapped engine refusal does not match celeris.ErrStreamRefused")
	}
	if errors.Is(celeris.ErrStreamRefused, net.ErrClosed) {
		t.Fatal("ErrStreamRefused wraps net.ErrClosed: a refused response cannot be told from a gone peer")
	}
	if !errors.Is(conn.ErrConnClosed, net.ErrClosed) || errors.Is(conn.ErrConnClosed, celeris.ErrStreamRefused) {
		t.Fatal("conn.ErrConnClosed must wrap net.ErrClosed and must not match ErrStreamRefused")
	}
}
