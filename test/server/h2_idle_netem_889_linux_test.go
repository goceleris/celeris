//go:build linux && netem889

package celeris_test

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// TestH2IdleClockBoundsAClientThatPingsFasterThanItsRTT889: a client that reads
// everything, sends one small GET and then a PING every 200 ms, over a path with
// a 300 ms round trip (netem 150 ms each way on lo: needs root and tc, so it is
// built only with -tags netem889, in a container with NET_ADMIN; nothing in CI
// runs it, so it is not CI evidence, and a CI runner's skip rule does not allow a
// skip). The server's PING ACKs are always in flight, unacknowledged bytes in the
// kernel's send queue, so the conn always holds output. Extending its idle period
// for that output for ever is what the progress cap exists to prevent, and the
// cap judges the bytes the peer acknowledged beyond the control frames the
// server sent it: this peer acknowledges about what it is sent, so it is
// extended only while its in-flight count shrinks (at most once per ACK that can
// be in flight, here about 2: 300 ms RTT over 200 ms pings) and is closed within
// a few idle periods, measured 2.4 to 3.6 s with IdleTimeout 1 s; the cell
// asserts 6 s. Its window is open, so it must also get the GOAWAY(NO_ERROR) the
// idle close sends, before the close, on every engine (io_uring reaped the
// refused conn in the sweep that staged it, before the SEND was submitted: bare
// EOF, 0 of 3).
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
				f, err := c.fr.ReadFrame()
				if g, ok := f.(*http2.GoAwayFrame); ok {
					c.gaSeen, c.gaCode = true, g.ErrCode
				}
				if err != nil {
					if errors.Is(err, os.ErrDeadlineExceeded) {
						t.Fatalf("engine=%s: still open 6 s after the request, a client that pings faster than its RTT holds the conn", e.name)
					}
					t.Logf("TAIL889 ack-lag engine=%s: closed after %v: %v, GOAWAY seen=%v code=%v", e.name, time.Since(t0).Round(time.Millisecond), err, c.gaSeen, c.gaCode)
					c.wantIdleGoAway889(t)
					return
				}
			}
		})
	}
}
