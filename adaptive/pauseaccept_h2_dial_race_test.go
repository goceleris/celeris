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
	"github.com/goceleris/celeris/resource"
)

// TestAdaptivePauseAccept_H2DialNoRSTRace is a second angle on the
// H2-dial-RST race that TestAdaptiveH2DialNoRSTRace guards, run with a
// larger (4) worker pool so the standby engine has more listen FDs to
// evict from the SO_REUSEPORT group before Addr() is published. The
// race is the same: if PauseAccept on the standby has not synchronously
// removed every listen FD from the routing pool by the time adaptive
// exposes Addr, a burst of dials gets split across active and standby,
// and the standby's FD close RSTs the conns that landed on it — fatal
// for H2 prior-knowledge handshakes mid-flush.
//
// Iterations bound at 3: the race only fires on engine spin-up, so we
// just need an additional burst against a wider pool in addition to the
// main test's larger budget.
func TestAdaptivePauseAccept_H2DialNoRSTRace(t *testing.T) {
	const iterations = 3
	for i := 0; i < iterations; i++ {
		runPauseAcceptH2Once(t, i)
	}
}

func runPauseAcceptH2Once(t *testing.T, iter int) {
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
			Workers: 4,
		},
	}
	e, err := New(cfg, &h2PrefaceHandler{}, nil)
	if err != nil {
		t.Skipf("iter %d: adaptive engine unavailable: %v", iter, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(30 * time.Second):
		}
	}()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && e.Addr() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatalf("iter %d: engine did not bind in time", iter)
	}
	target := e.Addr().String()

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
		switch {
		case errors.Is(firstErr, net.ErrClosed):
			// fine — test cleanup race, not the bug
		default:
			t.Fatalf("iter %d: %d/%d conns failed; first: %v", iter, count, conns, firstErr)
		}
	}
}
