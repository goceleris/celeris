//go:build linux

package iouring

import (
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
// 0.8 s, 1.6 s or about 100 s apart on an idle worker, and unbounded while
// the loop waits without a timeout for a SEND's completion (waitUntimed).
// Those bounds are main's, for every conn whose SEND is outstanding and which
// is not on the dirty list, and the fix does not change them. What the fix
// changes is which workers reach them: a worker that used to spin reached the
// gate every few microseconds.
//
// Each arm: one conn (pipelined: busy811Conns, the count that kept the old
// worker spinning on the default configuration), a 3 MiB response nobody
// reads (pipelined: and a second request behind it, which is what kept the
// old dirty list non-empty), WriteTimeout 1 s, ReadTimeout and IdleTimeout
// off. It logs the time from the request to the conn's teardown
// (ActiveConnections back to 0) and the CPU over it.
//
// The asserted arm is the default configuration (ReadHeaderTimeout 10 s,
// gate 32): the conn must be gone in a bound far under the 25 s a gate of
// 1024 would give. The other arms (ReadHeaderTimeout off, async single, sync)
// are the pre-existing bounds above, measured with E14_PROBE=1 and not
// asserted: they wait up to E14_PROBE_WAIT seconds (default 130).
func TestIouringWriteTimeoutBoundOfAStalledSend811(t *testing.T) {
	// WriteTimeout 1 s + closing drain 5 s + connState release hold 5 s +
	// a sweep every 0.8 s: 8 s on main and on the fix (8.1 / 8.6 s measured).
	const asserted = 20 * time.Second
	wait := 130 * time.Second
	if v, err := strconv.Atoi(os.Getenv("E14_PROBE_WAIT")); err == nil && v > 0 {
		wait = time.Duration(v) * time.Second
	}
	for _, tc := range []struct {
		name      string
		async     bool
		pipelined bool
		rht       time.Duration // 0: the 10 s default; -1: off
		assert    bool
	}{
		{"async_pipelined/rht_default", true, true, 0, true},
		{"async_pipelined/rht_off", true, true, -1, false},
		{"async_single/rht_default", true, false, 0, false},
		{"async_single/rht_off", true, false, -1, false},
		{"sync/rht_default", false, false, 0, false},
		{"sync/rht_off", false, false, -1, false},
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
			var gone time.Duration
			for time.Since(t0) < wait {
				time.Sleep(20 * time.Millisecond)
				if e.Metrics().ActiveConnections == 0 {
					gone = time.Since(t0)
					break
				}
			}
			t.Logf("celeris811 WRITETIMEOUT arm=%s conns=%d write_timeout_ms=1000 gone_ms=%d (0: still open after %d ms) cpu_frac=%.3f",
				tc.name, conns, gone.Milliseconds(), wait.Milliseconds(), float64(cpu811(t)-c0)/float64(time.Since(t0)))
			if tc.assert && (gone == 0 || gone > asserted) {
				t.Errorf("celeris#811: %d conns whose clients do not read, WriteTimeout 1 s, were not all torn down within %v "+
					"(gone_ms=%d, 0 = still open): the parked worker's sweep must still run in time", conns, asserted, gone.Milliseconds())
			}
		})
	}
}
