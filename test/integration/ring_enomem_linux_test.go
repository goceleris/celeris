//go:build linux

package integration

import (
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestStartAsyncServerRidesOutRingENOMEM: with the memlock limit squeezed for
// 600 ms the io_uring server's Start fails with the ring-setup ENOMEM, and
// startAsyncServer must retry it until the limit is back instead of failing the
// test on the first error (the flake of celeris#1104: TestPerHandlerAsync_*
// /iouring at 0.01 s with "server exited early: ... cannot allocate memory").
func TestStartAsyncServerRidesOutRingENOMEM(t *testing.T) {
	squeezeMemlock(t, 600*time.Millisecond)
	start := time.Now()
	_, base, _ := startAsyncServer(t, celeris.IOUring, false)
	t.Logf("io_uring server up after %v at %s", time.Since(start).Round(10*time.Millisecond), base)
}
