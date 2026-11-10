package monotest

import (
	"net"
	"testing"
	"time"
)

// The bound of ReadHeaderTimeout on a connection that sends nothing (celeris#1189).
// The scenarios here are not about the clock: they pin how late the deadline is
// enforced, on the real clock, for both native engines.
//
// The engines enforce the deadline from the accept. With TCP_DEFER_ACCEPT (the
// default) the accept of a silent connection is the kernel's promotion of it, one
// SYN-ACK retransmission timeout (1 s) after the SYN; measured 1.00 to 1.07 s
// (docker arm64, both engines), and deferlinger.DefaultLinger budgets 1.5 s for it.
const (
	silentTimeout = time.Second

	// silentSlack is what a silent connection may exceed its deadline by once it
	// has been accepted. epoll sweeps every 25 ms and io_uring arms a timer of its
	// own, so a connection is closed within 8 to 25 ms of its deadline (measured,
	// docker arm64 with 2 CPUs, 20 runs per engine); 150 ms is six times the larger
	// and is not tuned to the machine. It does not tell the io_uring sweep from the
	// timer on every run: the sweep that enforced the deadline before celeris#1189
	// came 0.1 to 0.9 s late, depending on where its phase fell after the accept.
	// The regression test for that is the io_uring package's
	// TestUndecidedConnHasItsHeaderTimerArmed1189, which does not depend on timing.
	silentSlack = 150 * time.Millisecond

	// settle is how long the engine idles before the connection, for its workers'
	// idle backoff (1 ms, then 5 ms, then 25 ms waits) to have run its course.
	settle = 700 * time.Millisecond

	// deferWindow is what a silent connection is allowed to wait in the kernel, with
	// TCP_DEFER_ACCEPT on, before the engine accepts it: the 1.5 s that
	// deferlinger.DefaultLinger budgets for it (1.07 s was the most measured).
	deferWindow = 1500 * time.Millisecond
)

// SilentConnIsCutAtReadHeaderTimeout starts an engine with ReadHeaderTimeout 1 s and
// TCP_DEFER_ACCEPT off, so the deadline counts from the connect, opens a connection
// that sends nothing and another that sends one byte (the protocol is not decided
// yet) and then nothing, and asserts each is closed no sooner than 900 ms and no later
// than 1.15 s after its connect.
func SilentConnIsCutAtReadHeaderTimeout(t *testing.T, start Start) {
	cfg := Config()
	cfg.ReadHeaderTimeout = silentTimeout
	cfg.DisableDeferAccept = true
	addr := start(t, cfg, handler{})
	// The engine is left idle first, as a server is when a silent connection comes in
	// for the first time: its workers wait in their longest idle wait, which is the
	// state in which a sweep is furthest away from the connection.
	time.Sleep(settle)
	for _, c := range []struct{ name, first string }{{"silent", ""}, {"one-byte", "G"}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cutWithin(t, addr, c.first, silentTimeout-100*time.Millisecond, silentTimeout+silentSlack)
		})
	}
}

// SilentConnIsCutAfterDeferAccept is the same with TCP_DEFER_ACCEPT on, the default:
// a connection that sends nothing is accepted by the engine about a second after its
// SYN and is closed ReadHeaderTimeout after that, so no later than the deferral's
// window (1.5 s), the timeout and the slack after its connect. This is the bound the
// ReadHeaderTimeout documentation states.
func SilentConnIsCutAfterDeferAccept(t *testing.T, start Start) {
	cfg := Config()
	cfg.ReadHeaderTimeout = silentTimeout
	addr := start(t, cfg, handler{})
	cutWithin(t, addr, "", silentTimeout-100*time.Millisecond, silentTimeout+deferWindow+silentSlack)
}

// cutWithin connects to addr, sends first (nothing when it is empty), and asserts the
// server closes the connection within [lo, hi] of the connect.
func cutWithin(t *testing.T, addr, first string, lo, hi time.Duration) {
	t.Helper()
	t0 := time.Now()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	if first != "" {
		if _, err := c.Write([]byte(first)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	d, closed := closedAfter(c, t0, hi+time.Second)
	switch {
	case !closed:
		t.Errorf("still open %v after the connect, want it closed by %v", d.Round(time.Millisecond), hi)
	case d < lo:
		t.Errorf("closed after %v, want no sooner than %v", d.Round(time.Millisecond), lo)
	case d > hi:
		t.Errorf("closed after %v, want by %v (ReadHeaderTimeout %v)", d.Round(time.Millisecond), hi, silentTimeout)
	}
}
