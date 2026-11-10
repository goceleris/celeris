//go:build linux

package iouring

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/mono/monotest"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#1189. A connection that sends nothing is closed at ReadHeaderTimeout from
// the engine's accept, by a timer of its own armed at the accept (before, by the
// sweep, which came up to 0.86 s late), and, with TCP_DEFER_ACCEPT on (the default),
// the kernel's deferral of a silent connection (about 1 s) is added before the accept.
// See monotest.SilentConnIsCutAtReadHeaderTimeout.
func TestSilentConnIsCutAtReadHeaderTimeout1189(t *testing.T) {
	monotest.SilentConnIsCutAtReadHeaderTimeout(t, startSilent1189)
}

func TestSilentConnIsCutAfterDeferAccept1189(t *testing.T) {
	monotest.SilentConnIsCutAfterDeferAccept(t, startSilent1189)
}

func startSilent1189(t *testing.T, cfg resource.Config, h stream.Handler) string {
	t.Helper()
	r := startEngine806(t, h, func(c *resource.Config) {
		addr := c.Addr
		*c = cfg
		c.Addr = addr
	})
	return r.addr
}

// A connection whose protocol is not decided yet has its header deadline armed in the
// kernel when it is accepted, not left to the sweep (celeris#1189). The sweep of an
// idle worker comes every 0.8 s, so a deadline enforced there is up to 0.8 s late.
// The check is the sweep's own: every sweep that meets a connection that is still
// undecided and has a deadline must find the timer in flight. A sweep that finds
// none means the deadline waits for a sweep.
func TestUndecidedConnHasItsHeaderTimerArmed1189(t *testing.T) {
	var (
		mu               sync.Mutex
		undecided, armed int
		addr             string
	)
	hook := func(w *Worker) {
		mu.Lock()
		defer mu.Unlock()
		// The hook is the package's, not the engine's: workers of an earlier test may
		// still be winding down.
		if addr == "" || w.cfg.Addr != addr {
			return
		}
		for _, fd := range w.liveConns {
			cs := w.conns[fd]
			if cs == nil || cs.detected || cs.detectDeadline == 0 {
				continue
			}
			undecided++
			if cs.headerTimerArmed {
				armed++
			}
		}
	}
	testHookCheckTimeouts.Store(&hook)
	t.Cleanup(func() { testHookCheckTimeouts.Store(nil) })
	r := startEngine806(t, nil, func(c *resource.Config) {
		c.Protocol = engine.Auto
		c.ReadHeaderTimeout = 10 * time.Second
	})
	mu.Lock()
	addr = r.addr
	mu.Unlock()
	c, err := net.DialTimeout("tcp", r.addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	// A sweep is at most about a second away; wait for three looks at the connection.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := undecided
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if undecided == 0 {
		t.Fatal("no sweep met the undecided connection")
	}
	if armed != undecided {
		t.Errorf("%d of %d sweeps that met the undecided connection found its header timer in flight, want all (the deadline is enforced by the sweep)", armed, undecided)
	}
}
