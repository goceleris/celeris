//go:build linux

package epoll

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

	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#895: the port Listen decides for a port 0 is held from the moment
// it is decided until Listen returns, on every path out of it: a socket that
// never listens, so no other socket can take the port while the loops start,
// while accepting is paused (the loops then have no listen socket and bind the
// port again on a resume), or at any time in between. The loops' own sockets
// join the port's SO_REUSEPORT group beside it, and it is closed last, after
// they have been closed, so the port is free the instant Listen returns
// (celeris#896).

func newPort0Engine895(t *testing.T, loops int) *Engine {
	t.Helper()
	e, err := New(resource.Config{
		Addr:      "127.0.0.1:0",
		Engine:    celerisengine.Epoll,
		Protocol:  celerisengine.HTTP1,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Resources: resource.Resources{Workers: loops},
	}, respondingHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// TestPort0IsHeldBetweenBeingChosenAndBeingBound895: while the hold is held
// (the hook runs before any loop is created) a plain net.Listen on the chosen
// address is refused, so nothing but the engine's own sockets can take it; and
// while Listen is serving the hold stays beside the loops' sockets (it keeps
// the port through a pause, TestPort0PausedPortCannotBeTaken895), and Listen
// gives it back as it returns.
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

	const loops = 3
	r := start895(t, "127.0.0.1:0", loops, false)
	if msg, _ := hookErr.Load().(string); msg != "" {
		t.Fatal(msg)
	}
	addr, _ := chosen.Load().(string)
	if addr == "" {
		t.Fatal("the hook never ran: Listen did not resolve the port")
	}
	if got := r.e.Addr().String(); got != addr {
		t.Fatalf("Addr() = %s, but the port chosen before the loops bound was %s", got, addr)
	}
	port := portOf895(t, r.e.Addr())
	c := census895(t)
	if n := c.listening[port]; n != loops {
		t.Fatalf("%d sockets listen on port %d with the engine serving, want %d (one per loop)", n, port, loops)
	}
	if n := c.bound[port]; n != loops+1 {
		t.Fatalf("%d sockets bound to port %d with the engine serving, want %d (one per loop and the hold)", n, port, loops+1)
	}
	if err := r.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if n := census895(t).bound[port]; n != 0 {
		t.Fatalf("%d sockets still bound to port %d after Listen returned, want 0 (the hold included)", n, port)
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

	e := newPort0Engine895(t, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no loop could report") {
			t.Fatalf("Listen = %v, want the no-address error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Listen kept running with no address to publish")
	}
	addr, _ := chosen.Load().(string)
	if addr == "" {
		t.Fatal("the hook never ran")
	}
	port := portOfString895(t, addr)
	if n := census895(t).bound[port]; n != 0 {
		t.Fatalf("%d sockets still bound to port %d after Listen failed, want 0", n, port)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("net.Listen on %s after the failed Listen: %v", addr, err)
	}
	_ = ln.Close()
}

// TestPort0ListenErrorWhenTheHoldCannotBeBound895: an address this host does
// not own is an error from Listen, not loops that each fail or bind alone.
func TestPort0ListenErrorWhenTheHoldCannotBeBound895(t *testing.T) {
	e, err := New(resource.Config{
		Addr:      "203.0.113.77:0",
		Engine:    celerisengine.Epoll,
		Protocol:  celerisengine.HTTP1,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Resources: resource.Resources{Workers: 2},
	}, respondingHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = e.Listen(t.Context())
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
