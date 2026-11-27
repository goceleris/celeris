//go:build linux

package iouring

import (
	"errors"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
)

// celeris#1357: a write above the per-write limit names the limit, and still
// matches ErrQueueFull. The write is refused before anything is queued.
func TestDriverWriteAboveLimitNamesTheLimit1357(t *testing.T) {
	e, stop := startTestEngine(t)
	t.Cleanup(stop)
	w, wl := e.workers[0], e.WorkerLoop(0)
	a := registerSettledDriver(t, w, wl)
	defer a.closePeer()

	err := wl.Write(a.fd, make([]byte, 64<<20+1))
	if !errors.Is(err, engine.ErrQueueFull) {
		t.Fatalf("Write of 64 MiB + 1: err = %v, want a match for ErrQueueFull", err)
	}
	var tl *engine.WriteTooLargeError
	if !errors.As(err, &tl) || tl.Limit != 64<<20 || tl.Size != 64<<20+1 {
		t.Fatalf("Write of 64 MiB + 1: err = %#v, want *WriteTooLargeError{Size: 64 MiB + 1, Limit: 64 MiB}", err)
	}
	if !strings.Contains(err.Error(), "exceeds the per-write limit of 64 MiB") {
		t.Fatalf("message %q does not name the limit", err)
	}
}
