//go:build linux

package adaptive

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// The tests in this file reach into the fix for celeris#616 (the Listen hook
// and the hold itself), so unlike port_hold_616_linux_test.go they do not
// build against the code before it.

// TestAdaptivePortHeldThroughListenStart616 takes the port at the last moment
// the hold has to cover: inside Listen, just before the start engine begins
// to bind. A hold given back any earlier, for instance at the top of Listen,
// would hand the port to the thief here, and the start engine would then fail
// to bind it.
func TestAdaptivePortHeldThroughListenStart616(t *testing.T) {
	for _, se := range startEngines {
		for _, th := range portThieves {
			t.Run(se.env+"/"+th.name, func(t *testing.T) {
				e, addr, port := newGapEngine(t, se.env, se.want)

				var hookRan bool
				var hookAddr string
				var stealErr error
				listenGapHook = func(a string) {
					hookRan, hookAddr = true, a
					c, err := th.steal(a)
					if err == nil {
						_ = c.Close()
					}
					stealErr = err
				}
				defer func() { listenGapHook = nil }()

				stop, lerr := startAndServe(t, e)
				listenGapHook = nil
				if !hookRan {
					t.Fatal("the Listen hook never ran: the steal inside Listen was not attempted")
				}
				if hookAddr != addr {
					t.Fatalf("the Listen hook saw %q, want the address New picked, %q", hookAddr, addr)
				}
				t.Logf("celeris616 in-Listen steal: start=%s thief=%s addr=%s steal_err=%v listen_err=%v", se.env, th.name, addr, stealErr, lerr)
				if stealErr == nil {
					t.Errorf("%s bound %s inside Listen before the start engine did: the hold was given back too early", th.name, addr)
				} else if !errors.Is(stealErr, syscall.EADDRINUSE) {
					t.Errorf("the steal inside Listen failed with %v, not EADDRINUSE", stealErr)
				}
				if lerr != nil {
					t.Fatalf("the engine did not start: %v", lerr)
				}
				defer stop()
				if got := e.Addr().(*net.TCPAddr).Port; got != port {
					t.Errorf("engine serves on port %d, want the port New picked, %d (%s)", got, port, addr)
				}
			})
		}
	}
}

// TestAdaptivePortHoldReleasedOnceBound616: the hold is given back as soon as
// the start engine has published its address, not kept for the engine's life.
func TestAdaptivePortHoldReleasedOnceBound616(t *testing.T) {
	for _, se := range startEngines {
		t.Run(se.env, func(t *testing.T) {
			e, _, _ := newGapEngine(t, se.env, se.want)
			if e.portHold.Load() == nil {
				_ = e.Shutdown(context.Background())
				t.Fatal("New returned without holding the port")
			}
			stop, err := startAndServe(t, e)
			if err != nil {
				_ = e.Shutdown(context.Background())
				t.Fatalf("Listen: %v", err)
			}
			defer stop()
			if e.portHold.Load() != nil {
				t.Error("the start engine has published its address but the port hold is still open")
			}
		})
	}
}

// TestAdaptivePortHoldKind616 pins which hold New takes. On a free port it is
// the exclusive one (SO_REUSEPORT without SO_REUSEADDR), which refuses even a
// Go listener. Beside the TIME_WAIT sockets of an ordinary Go server the
// kernel refuses that one, and New must take the shared hold (SO_REUSEADDR as
// well) rather than none: TestAdaptivePortHeldBesideTimeWait616 shows what
// the shared hold still refuses.
func TestAdaptivePortHoldKind616(t *testing.T) {
	opts := func(t *testing.T, e *Engine) (reuseAddr, reusePort int) {
		t.Helper()
		f := e.portHold.Load()
		if f == nil {
			t.Fatal("New returned without holding the port")
		}
		fd := int(f.Fd())
		ra, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR)
		if err != nil {
			t.Fatalf("getsockopt SO_REUSEADDR: %v", err)
		}
		rp, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT)
		if err != nil {
			t.Fatalf("getsockopt SO_REUSEPORT: %v", err)
		}
		return ra, rp
	}
	t.Setenv("CELERIS_ADAPTIVE_START", "epoll")
	t.Run("free-port", func(t *testing.T) {
		e, err := New(resource.Config{Addr: ":0", Protocol: engine.HTTP1, Resources: resource.Resources{Workers: 2}}, respHandler{}, nil)
		if err != nil {
			t.Fatalf("adaptive.New: %v", err)
		}
		defer func() { _ = e.Shutdown(context.Background()) }()
		ra, rp := opts(t, e)
		t.Logf("celeris616 hold kind: state=free-port addr=%s SO_REUSEADDR=%d SO_REUSEPORT=%d", e.cfg.Addr, ra, rp)
		if ra != 0 || rp == 0 {
			t.Errorf("the hold on a free port has SO_REUSEADDR=%d SO_REUSEPORT=%d, want 0 and set: the exclusive hold", ra, rp)
		}
	})
	t.Run("time-wait", func(t *testing.T) {
		port := leaveTimeWait(t)
		e, err := New(resource.Config{Addr: ":" + strconv.Itoa(port), Protocol: engine.HTTP1, Resources: resource.Resources{Workers: 2}}, respHandler{}, nil)
		if err != nil {
			t.Fatalf("adaptive.New: %v", err)
		}
		defer func() { _ = e.Shutdown(context.Background()) }()
		ra, rp := opts(t, e)
		t.Logf("celeris616 hold kind: state=time-wait addr=%s SO_REUSEADDR=%d SO_REUSEPORT=%d", e.cfg.Addr, ra, rp)
		if ra == 0 || rp == 0 {
			t.Errorf("the hold beside TIME_WAIT sockets has SO_REUSEADDR=%d SO_REUSEPORT=%d, want both set: the shared hold", ra, rp)
		}
	})
}
