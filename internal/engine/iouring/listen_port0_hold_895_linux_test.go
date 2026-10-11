//go:build linux

package iouring

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#895: the port Listen decides for a port 0 is held from the moment
// it is decided until every worker is listening on it, so no other socket can
// take it in between, and is let go on every path out of Listen.

func newPort0Engine895(t *testing.T, workers int) *Engine {
	t.Helper()
	e, err := New(resource.Config{
		Addr:      "127.0.0.1:0",
		Engine:    engine.IOUring,
		Protocol:  engine.HTTP1,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Resources: resource.Resources{Workers: workers},
	}, respondingHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// TestPort0IsHeldBetweenBeingChosenAndBeingBound895: while the hold is held
// (the hook runs before any worker is created) a plain net.Listen on the chosen
// address is refused, so nothing but the engine's own sockets can take it; and
// once Listen is serving, the hold is gone and the workers hold the port alone.
func TestPort0IsHeldBetweenBeingChosenAndBeingBound895(t *testing.T) {
	var hookErr atomic.Value
	var chosen atomic.Value
	portHoldHook = func(addr string) {
		chosen.Store(addr)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			hookErr.Store("a plain net.Listen on " + addr + " succeeded while the port was held")
			return
		}
		if !errors.Is(err, unix.EADDRINUSE) {
			hookErr.Store("a plain net.Listen on " + addr + " failed with " + err.Error() + ", want EADDRINUSE")
		}
	}
	t.Cleanup(func() { portHoldHook = nil })

	r := start895(t, "127.0.0.1:0", 2, 1, false)
	r.e.mu.Lock()
	workers := len(r.e.workers)
	r.e.mu.Unlock()
	if msg, _ := hookErr.Load().(string); msg != "" {
		t.Fatal(msg)
	}
	addr, _ := chosen.Load().(string)
	if addr == "" {
		t.Fatal("the hook never ran: Listen did not resolve the port")
	}
	if got := r.e.Addr().String(); got != addr {
		t.Fatalf("Addr() = %s, but the port chosen before the workers bound was %s", got, addr)
	}
	port := portOf895(t, r.e.Addr())
	if n := census895(t).bound[port]; n != workers {
		t.Fatalf("%d sockets bound to port %d with the engine serving, want %d (one per worker, no hold)", n, port, workers)
	}
}

// TestPort0HoldIsReleasedWhenListenFails895: a Listen that fails after the
// port was chosen gives it back.
func TestPort0HoldIsReleasedWhenListenFails895(t *testing.T) {
	orig := listenAddrOf
	listenAddrOf = func(int) net.Addr { return nil }
	t.Cleanup(func() { listenAddrOf = orig })
	var chosen atomic.Value
	portHoldHook = func(addr string) { chosen.Store(addr) }
	t.Cleanup(func() { portHoldHook = nil })

	var err error
	_, _, _ = retryRingENOMEM(func() error {
		e := newPort0Engine895(t, 2)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- e.Listen(ctx) }()
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Listen kept running with no address to publish")
		}
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "no worker could report") {
		t.Fatalf("Listen = %v, want the no-address error", err)
	}
	addr, _ := chosen.Load().(string)
	if addr == "" {
		t.Fatal("the hook never ran")
	}
	port := portOfString895(t, addr)
	if n := census895(t).bound[port]; n != 0 {
		t.Fatalf("%d sockets still bound to port %d after Listen failed, want 0", n, port)
	}
	// bindWithin656, not an immediate bind: the sockets of a worker that
	// was cancelled are let go of asynchronously on a main without the
	// celeris#896 fix, which this test is not about.
	if err := bindWithin656(addr, 3*time.Second); err != nil {
		t.Fatalf("net.Listen on %s after the failed Listen: %v", addr, err)
	}
}

// TestPort0ListenErrorWhenTheHoldCannotBeBound895: an address this host does
// not own is an error from Listen, not workers that each fail or bind alone.
func TestPort0ListenErrorWhenTheHoldCannotBeBound895(t *testing.T) {
	e, err := New(resource.Config{
		Addr:      "203.0.113.77:0",
		Engine:    engine.IOUring,
		Protocol:  engine.HTTP1,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Resources: resource.Resources{Workers: 2},
	}, respondingHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = retryRingENOMEM(func() error { return e.Listen(t.Context()) })
	if err == nil || !errors.Is(err, unix.EADDRNOTAVAIL) {
		t.Fatalf("Listen on an address this host does not own = %v, want EADDRNOTAVAIL", err)
	}
	t.Logf("Listen: %v", err)
}

func portOfString895(t *testing.T, addr string) int {
	t.Helper()
	return portOf895(t, mustTCPAddr895(t, addr))
}

func mustTCPAddr895(t *testing.T, addr string) net.Addr {
	t.Helper()
	a, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		t.Fatalf("resolve %q: %v", addr, err)
	}
	return a
}
