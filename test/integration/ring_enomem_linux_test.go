//go:build linux

package integration

import (
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/iouring"
	"github.com/goceleris/celeris/internal/resource"
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

// TestStartEngineRetryRidesOutRingENOMEM is the same for an engine built and
// started directly (the slowloris tests' io_uring arm): New reads a probe ring
// that hit the ENOMEM as "io_uring not available", Listen's ring setup fails
// with the ENOMEM itself, and both are retried until the limit is back.
func TestStartEngineRetryRidesOutRingENOMEM(t *testing.T) {
	squeezeMemlock(t, 600*time.Millisecond)
	start := time.Now()
	e := startEngineRetry(t, func() (engine.Engine, error) {
		return iouring.New(resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, Resources: resource.Resources{Workers: 2}}, &echoHandler{})
	}, func(err error) { t.Fatalf("io_uring.New: %v", err) })
	t.Logf("io_uring engine up after %v at %s", time.Since(start).Round(10*time.Millisecond), e.(addrGetter).Addr())
}
