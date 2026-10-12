//go:build linux

package iouring

import (
	"io"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#811: the fix parks a worker that used to spin, so how late is
// WriteTimeout enforced for a conn whose client does not read? The sweep
// (checkTimeouts) runs on an iteration gate: every 32 iterations with
// ReadHeaderTimeout > 0 or a detached conn, every 1024 otherwise, and an idle
// iteration waits up to 25 / 50 / 100 ms (baseTimeout), so the sweeps are
// 0.8 s, 1.6 s or about 100 s apart on an idle worker. A worker that used to
// spin reached the gate every few microseconds, so main enforced WriteTimeout
// on a stalled async conn within about a second whatever ReadHeaderTimeout
// was; parked, it would reach it 100 s later with ReadHeaderTimeout off, so
// the give-up sets sendWatch and the loop sweeps about every 1 to 2 s
// (checked on every 8th iteration) while a SEND is outstanding (sweepWatchDue).
//
// A worker that waits for a SEND's completion with no timeout (waitUntimed,
// mode 3a: the last thing it submitted is a remainder SEND to a client that
// stopped reading) runs no iteration, so no sweep: not bounded by anything
// here, nor on main. The partial_read arms measure it (E14_PROBE=1).
//
// Each arm: one conn (pipelined: busy811Conns, the count that kept the old
// worker spinning on the default configuration), a 3 MiB response nobody
// reads (pipelined: and a second request behind it, which is what kept the
// old dirty list non-empty), WriteTimeout 1 s, ReadTimeout and IdleTimeout
// off. It logs the time from the request to the conn's teardown
// (ActiveConnections back to 0) and the CPU over it.
//
// The asserted arms are the pipelined async ones, the shape that used to spin,
// on the default configuration (ReadHeaderTimeout 10 s) and with it off: the
// conn must be gone in a bound far under what the gate alone gives (about
// 100 s with ReadHeaderTimeout off). The other arms (async single, sync, and
// the partial_read ones) are bounds that main has as well, measured with
// E14_PROBE=1 and not asserted: they wait up to E14_PROBE_WAIT seconds
// (default 130).
func TestIouringWriteTimeoutBoundOfAStalledSend811(t *testing.T) {
	// WriteTimeout 1 s + closing drain 5 s + connState release hold 5 s +
	// a sweep every 1 s or less: about 8 to 10 s.
	const asserted = 20 * time.Second
	wait := 130 * time.Second
	if v, err := strconv.Atoi(os.Getenv("E14_PROBE_WAIT")); err == nil && v > 0 {
		wait = time.Duration(v) * time.Second
	}
	for _, tc := range []struct {
		name      string
		async     bool
		pipelined bool
		partial   int           // bytes the client reads, then stops: the first SEND completes short, a remainder SEND is the worker's last submission
		rht       time.Duration // 0: the 10 s default; -1: off
		assert    bool
	}{
		{"async_pipelined/rht_default", true, true, 0, 0, true},
		{"async_pipelined/rht_off", true, true, 0, -1, true},
		{"async_single/rht_default", true, false, 0, 0, false},
		{"async_single/rht_off", true, false, 0, -1, false},
		{"sync/rht_default", false, false, 0, 0, false},
		{"sync/rht_off", false, false, 0, -1, false},
		{"partial_read/async/rht_off", true, false, 256 << 10, -1, false},
		{"partial_read/sync/rht_off", false, false, 256 << 10, -1, false},
	} {
		if !tc.assert && os.Getenv("E14_PROBE") == "" {
			continue // a probe arm exists only under E14_PROBE=1 (a skipped subtest would be an unaccounted SKIP in CI)
		}
		t.Run(tc.name, func(t *testing.T) {
			h := newBusyHandler811()
			e, addr := startFDLEngine(t, h, func(c *resource.Config) {
				c.AsyncHandlers = tc.async
				c.Resources.SocketSend = busy811SndBuf
				c.WriteTimeout = time.Second
				c.ReadTimeout = -1
				c.IdleTimeout = -1
				c.ReadHeaderTimeout = tc.rht
			})
			conns := 1
			if tc.pipelined {
				conns = busy811Conns
			}
			cls := make([]*busyClient811, conns)
			c0, t0 := cpu811(t), time.Now()
			for i := range cls {
				cls[i] = dialBusy811(t, addr)
				cls[i].send(t, "/big")
			}
			if tc.pipelined {
				time.Sleep(300 * time.Millisecond) // every first SEND is in flight
				for _, cl := range cls {
					cl.send(t, "/small")
				}
			}
			if tc.partial > 0 {
				_ = cls[0].c.SetReadDeadline(time.Now().Add(10 * time.Second))
				if _, err := io.CopyN(io.Discard, cls[0].br, int64(tc.partial)); err != nil {
					t.Fatalf("reading the first %d bytes of the response: %v", tc.partial, err)
				}
			}
			var gone time.Duration
			var cpu5 time.Duration
			for time.Since(t0) < wait {
				time.Sleep(20 * time.Millisecond)
				if cpu5 == 0 && time.Since(t0) >= 5*time.Second {
					cpu5 = cpu811(t) - c0 // a spin that ends at WriteTimeout (1 s) has spent its CPU by now
				}
				if e.Metrics().ActiveConnections == 0 {
					gone = time.Since(t0)
					break
				}
			}
			t.Logf("celeris811 WRITETIMEOUT arm=%s conns=%d write_timeout_ms=1000 gone_ms=%d (0: still open after %d ms) cpu_ms_at_5s=%.1f cpu_frac=%.3f",
				tc.name, conns, gone.Milliseconds(), wait.Milliseconds(), float64(cpu5)/1e6, float64(cpu811(t)-c0)/float64(time.Since(t0)))
			if tc.assert && (gone == 0 || gone > asserted) {
				t.Errorf("celeris#811: %d conns whose clients do not read, WriteTimeout 1 s, were not all torn down within %v "+
					"(gone_ms=%d, 0 = still open): the parked worker's sweep must still run in time", conns, asserted, gone.Milliseconds())
			}
		})
	}
}
