//go:build linux

package adaptive

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
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

// TestAdaptivePortHeldUntilListenReturns616: the hold lives until Listen
// returns, not until the start engine has published its address (celeris#886
// item 19). Adaptive is not an engine.AcceptController, so a user cannot pause
// it; what pauses its active sub-engine is a switch, and under
// DisableDeferAccept the outgoing engine's listen sockets close at once, so
// for the switch gap the port has no listening socket at all. The test pauses
// the active sub-engine directly to open that gap and holds it open:
//
//   - once no listener is left on the port, both thieves (a Go listener and a
//     bare socket) must be refused with EADDRINUSE: only the hold keeps them
//     out. Given back at the publication of Addr, as before, the hold would be
//     gone here and a thief would bind;
//   - after the resume the sub-engine's workers listen again beside the hold
//     (a never-listening SO_REUSEPORT socket takes no connection and does not
//     refuse a sibling), and ErrorListenerRecreate stays 0;
//   - once Listen has returned, and before Shutdown (which gives the hold back
//     on its own), the port is free again: the deferred release in Listen ran.
func TestAdaptivePortHeldUntilListenReturns616(t *testing.T) {
	for _, se := range startEngines {
		t.Run(se.env, func(t *testing.T) {
			e, addr, port := newGapEngineWith(t, se.env, se.want, func(c *resource.Config) { c.DisableDeferAccept = true })
			cancel, done, err := startListen(e)
			if err != nil {
				_ = e.Shutdown(context.Background())
				t.Fatalf("Listen: %v", err)
			}
			stopped := false
			stopListen := func() error {
				cancel()
				if stopped {
					return nil
				}
				stopped = true
				return <-done
			}
			defer func() {
				_ = stopListen()
				_ = e.Shutdown(context.Background())
			}()

			if e.portHold.Load() == nil {
				// Not fatal: the thieves below show what the missing hold means.
				t.Error("the port hold was given back before Listen returned: the start engine has published its address and the hold is gone")
			}
			active := *e.active.Load()
			ac, ok := active.(engine.AcceptController)
			if !ok {
				t.Fatalf("the %s sub-engine is not an AcceptController", se.env)
			}
			workers := active.Metrics().Workers
			if err := ac.PauseAccept(); err != nil {
				t.Fatalf("PauseAccept: %v", err)
			}
			// The pause closes the sub-engine's listen sockets on the workers'
			// own threads (epoll) or by cancelling their accepts (io_uring), so
			// wait until nothing listens on the port before the thieves try.
			if !waitRefused616(addr, 10*time.Second) {
				t.Fatalf("the paused %s sub-engine still accepts on %s after 10s", se.env, addr)
			}
			if n := listeningPorts895(t)[port]; n != 0 {
				t.Fatalf("%d listen sockets still on port %d after the pause", n, port)
			}
			var refused []string
			for _, th := range portThieves {
				c, err := th.steal(addr)
				if err == nil {
					_ = c.Close()
					t.Errorf("%s bound %s while the active sub-engine was paused: nothing held the port", th.name, addr)
					continue
				}
				if !errors.Is(err, syscall.EADDRINUSE) {
					t.Errorf("%s on %s while paused: %v, want EADDRINUSE", th.name, addr, err)
					continue
				}
				refused = append(refused, th.name)
			}

			if err := ac.ResumeAccept(); err != nil {
				t.Fatalf("ResumeAccept: %v", err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for listeningPorts895(t)[port] < workers && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			listening := listeningPorts895(t)[port]
			if listening < workers {
				t.Errorf("%d of %d workers listen on %s 10s after the resume", listening, workers, addr)
			}
			if c, err := net.DialTimeout("tcp", addr, 2*time.Second); err != nil {
				t.Errorf("dial %s after the resume: %v", addr, err)
			} else {
				_ = c.Close()
			}
			recreate := e.Metrics().ErrorListenerRecreate
			if recreate != 0 {
				t.Errorf("ErrorListenerRecreate = %d after the resume, want 0", recreate)
			}

			// Listen returns; the deferred release is the only thing that can
			// have given the hold back, since Shutdown has not run.
			if err := stopListen(); err != nil {
				t.Errorf("Listen returned %v after a clean stop", err)
			}
			var freed []string
			var slowest time.Duration
			for _, th := range portThieves {
				took, err := stealWithin(th, addr, 2*time.Second)
				if err != nil {
					t.Errorf("%s cannot bind %s after Listen returned and before Shutdown: %v: the hold outlived Listen", th.name, addr, err)
					continue
				}
				freed = append(freed, th.name)
				slowest = max(slowest, took)
			}
			t.Logf("celeris616 hold until Listen returns: start=%s paused_refused=%v workers=%d listening_after_resume=%d error_listener_recreate=%d freed_after_listen=%v in=%v", se.env, refused, workers, listening, recreate, freed, slowest)
		})
	}
}

// waitRefused616 polls until a dial to addr is refused, which is when no
// socket listens on it, or limit passes.
func waitRefused616(addr string, limit time.Duration) bool {
	for deadline := time.Now().Add(limit); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
			continue
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			return true
		}
	}
	return false
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
