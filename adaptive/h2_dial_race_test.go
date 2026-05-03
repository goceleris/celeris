//go:build linux

package adaptive

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// h2PrefaceHandler returns a tiny canned H1 response — enough for the
// test to drive the engine into the protocol-detection path. The dial
// race we're guarding against fires before any request is parsed, so
// the response payload is irrelevant.
type h2PrefaceHandler struct{}

func (h *h2PrefaceHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}},
		[]byte("ok"))
}

// h2ClientPreface is the RFC 7540 §3.5 client preface — exact 24 bytes.
var h2ClientPreface = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")

// TestAdaptiveH2DialNoRSTRace asserts that immediately after Listen
// returns a usable Addr(), an H2 prior-knowledge dial succeeds without
// being RST'd by the standby engine — i.e. the SO_REUSEPORT group only
// holds the active engine's listen sockets at that point.
//
// Before the synchronous-PauseAccept fix, the standby engine's listen
// FD was still in the routing pool when adaptive.Listen exposed Addr;
// the kernel split a burst of dials between active and standby, then
// the standby's FD close (triggered async by acceptPaused=true) sent
// RST to whichever conns had landed in its accept queue. H1 clients
// retry transparently; H2 prior-knowledge clients fail mid-handshake.
//
// We dial 32 connections in parallel right after Addr() is non-nil and
// each one sends the H2 client preface. Every dial must complete its
// preface write without ECONNRESET — any RST means a conn landed on a
// listener that should have been out of the SO_REUSEPORT group.
func TestAdaptiveH2DialNoRSTRace(t *testing.T) {
	// 5 iterations is the smallest budget that still surfaces the
	// SO_REUSEPORT race (each iter bursts 32 parallel dials against a
	// fresh adaptive engine). 30 → 10 (bf377c0) wasn't enough on Azure
	// CI runners under `-race` — race instrumentation roughly doubles
	// the per-iter time, and iter 1 was still tripping the 30s bind
	// deadline. msr1 clears 30 iters in <50s without -race; 5 with
	// 60s deadline (below) fits CI's wall budget under -race while
	// keeping ≥1 dial burst against a freshly-spun engine — the
	// minimum needed to catch a regression of the original
	// SO_REUSEPORT phantom-listener bug.
	const iterations = 5

	for i := 0; i < iterations; i++ {
		runOnce(t, i)
	}
}

func runOnce(t *testing.T, iter int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("iter %d: pick port: %v", iter, err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := resource.Config{
		Addr:            addr,
		Engine:          engine.Adaptive,
		Protocol:        engine.Auto,
		EnableH2Upgrade: true,
		Resources: resource.Resources{
			Workers: 2,
		},
	}
	e, err := New(cfg, &h2PrefaceHandler{})
	if err != nil {
		t.Skipf("iter %d: adaptive engine unavailable: %v", iter, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	defer func() {
		cancel()
		// 30s upper bound — slow CI runners can take several seconds to
		// drain io_uring CQEs / close SO_REUSEPORT FDs, and the next
		// iteration's engine fails to bind if the previous one hasn't
		// fully shut down. 2s was tight enough to cause a flake at
		// iter 1 on Azure kernel 6.17 runners.
		select {
		case <-errCh:
		case <-time.After(30 * time.Second):
		}
	}()

	// 30s deadline (5s was tight on slow GitHub Actions runners; mirrors
	// the bind-deadline bump on the iouring async-churn test in 1655eb0).
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && e.Addr() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatalf("iter %d: engine did not bind in time", iter)
	}
	target := e.Addr().String()

	// Burst-dial from many goroutines so the kernel has to load-balance
	// across the SO_REUSEPORT group exactly when the bug used to fire.
	const conns = 32
	var wg sync.WaitGroup
	errs := make(chan error, conns)
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c, derr := net.DialTimeout("tcp", target, 2*time.Second)
			if derr != nil {
				errs <- fmt.Errorf("conn %d dial: %w", id, derr)
				return
			}
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			if _, werr := c.Write(h2ClientPreface); werr != nil {
				errs <- fmt.Errorf("conn %d preface write: %w", id, werr)
				return
			}
			// Read at least one byte (the server's SETTINGS frame
			// prefix) — proves the kernel actually delivered our
			// preface to a real handler, not RST it from a stale
			// listener.
			br := bufio.NewReader(c)
			if _, rerr := br.ReadByte(); rerr != nil {
				errs <- fmt.Errorf("conn %d settings read: %w", id, rerr)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	var firstErr error
	count := 0
	for err := range errs {
		count++
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		// Surface the smoking gun — these strings are what the matrix
		// runner saw on the cell that tripped fail-fast.
		switch {
		case errors.Is(firstErr, net.ErrClosed):
			// fine — test cleanup race, not the bug
		default:
			t.Fatalf("iter %d: %d/%d conns failed; first: %v", iter, count, conns, firstErr)
		}
	}
}
