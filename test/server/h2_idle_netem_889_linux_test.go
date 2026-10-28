//go:build linux && netem889

package celeris_test

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestH2IdleClockBoundsAClientThatPingsFasterThanItsRTT889: a client that reads
// everything, sends one small GET and then a PING every 200 ms, over a path with
// a 300 ms round trip (netem 150 ms each way on lo: needs root and tc, so it is
// built only with -tags netem889, in a container with NET_ADMIN; a CI runner's
// skip rule does not allow a skip, and it would add nothing there). The server's
// PING ACKs are always in flight, unacknowledged bytes in the kernel's send
// queue, and the count of bytes the conn holds (TIOCOUTQ) is never zero. A conn
// that is not closed because of them never closes, as it did on the head that
// extended the idle period for any output (held 10 s with IdleTimeout 1 s). The
// count of bytes in flight goes up and down with the ACKs, never lower than the
// last extension for long, and the conn is closed once it has not gone down by
// the next expiry: at most one extension per ACK that can be in flight, here
// 2 (300 ms RTT over 200 ms pings), so within 4 idle periods; the bound the cell
// asserts is 6 s.
func TestH2IdleClockBoundsAClientThatPingsFasterThanItsRTT889(t *testing.T) {
	if out, err := exec.Command("tc", "qdisc", "replace", "dev", "lo", "root", "netem", "delay", "150ms").CombinedOutput(); err != nil {
		t.Skipf("tc netem: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tc", "qdisc", "del", "dev", "lo", "root").Run() })
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			c := dialH2raw889(t, tailServer889(t, e.eng), h2dial889{})
			c.request(1, "GET", "/tail889?n=100", true, true)
			t0 := time.Now()
			stop := c.pingEvery889(200 * time.Millisecond)
			defer stop()
			_ = c.conn.SetReadDeadline(t0.Add(6 * time.Second))
			for {
				if _, err := c.fr.ReadFrame(); err != nil {
					if errors.Is(err, os.ErrDeadlineExceeded) {
						t.Fatalf("engine=%s: still open 6 s after the request, a client that pings faster than its RTT holds the conn", e.name)
					}
					t.Logf("TAIL889 ack-lag engine=%s: closed after %v: %v", e.name, time.Since(t0).Round(time.Millisecond), err)
					return
				}
			}
		})
	}
}
