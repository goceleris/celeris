//go:build linux

package epoll

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	celerisengine "github.com/goceleris/celeris/internal/engine"
)

// celeris#1357: a write above the per-write limit names the limit, and still
// matches ErrQueueFull. The write is refused before anything is queued.
func TestDriverWriteAboveLimitNamesTheLimit1357(t *testing.T) {
	eng, stop := newTestEngine(t)
	t.Cleanup(stop)
	local, peer := socketpairNonblocking(t)
	t.Cleanup(func() { _ = unix.Close(local); _ = unix.Close(peer) })
	wl := eng.WorkerLoop(0)
	if err := wl.RegisterConn(local, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	t.Cleanup(func() { _ = wl.UnregisterConn(local) })

	err := wl.Write(local, make([]byte, 64<<20+1))
	if !errors.Is(err, celerisengine.ErrQueueFull) {
		t.Fatalf("Write of 64 MiB + 1: err = %v, want a match for ErrQueueFull", err)
	}
	var tl *celerisengine.WriteTooLargeError
	if !errors.As(err, &tl) || tl.Limit != 64<<20 || tl.Size != 64<<20+1 {
		t.Fatalf("Write of 64 MiB + 1: err = %#v, want *WriteTooLargeError{Size: 64 MiB + 1, Limit: 64 MiB}", err)
	}
	if !strings.Contains(err.Error(), "exceeds the per-write limit of 64 MiB") {
		t.Fatalf("message %q does not name the limit", err)
	}
}
