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
// stopped reading) runs no iteration, so no sweep: on main nothing bounded it
// (the sync and partial_read arms stayed open for 130 s and more). Since
// celeris#884 item 62 flushSend marks a remainder or a long SEND
// (remainderWatch), which keeps the worker out of that wait, so the sweep
// comes for it.
//
// Each arm: one conn (pipelined: busy811Conns, the count that kept the old
// worker spinning on the default configuration), a 3 MiB response nobody
// reads (pipelined: and a second request behind it, which is what kept the
// old dirty list non-empty), WriteTimeout 1 s, ReadTimeout and IdleTimeout
// off. It logs the time from the request to the conn's teardown
// (ActiveConnections back to 0) and the CPU over it.
//
// Every arm is asserted: the conn must be gone in a bound far under what the
// gate alone gives (about 100 s with ReadHeaderTimeout off). On main only the
// pipelined async arms were asserted; the others were probes
// (E14_PROBE=1), and sync/rht_off, async_single/rht_off and
// partial_read/sync stayed open for a 40 s wait (sync/rht_default and
// async_single/rht_default took 15 s). Since celeris#884 item 62 every arm is
// gone in about 7 to 8 s.
// E14_PROBE_WAIT (seconds, default 130) is how long an arm waits.
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
	}{
		{"async_pipelined/rht_default", true, true, 0, 0},
		{"async_pipelined/rht_off", true, true, 0, -1},
		{"async_single/rht_default", true, false, 0, 0},
		{"async_single/rht_off", true, false, 0, -1},
		{"sync/rht_default", false, false, 0, 0},
		{"sync/rht_off", false, false, 0, -1},
		{"partial_read/async/rht_off", true, false, 256 << 10, -1},
		{"partial_read/sync/rht_off", false, false, 256 << 10, -1},
	} {
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
			if gone == 0 || gone > asserted {
				t.Errorf("celeris#811: %d conns whose clients do not read, WriteTimeout 1 s, were not all torn down within %v "+
					"(gone_ms=%d, 0 = still open): the parked worker's sweep must still run in time", conns, asserted, gone.Milliseconds())
			}
		})
	}
}

// TestIouringStalledKeepAliveResponseIsBoundedByWriteTimeout884 is celeris#884
// item 64 end to end: a keep-alive HTTP/1.1 client with a 4 KiB receive buffer
// asks for 16 MiB and reads nothing, WriteTimeout is 1 s (ReadTimeout,
// IdleTimeout and ReadHeaderTimeout off, so only the write side can end the
// conn), and the worker's last submission is the SEND of the response's
// remainder, which it waits for with no timeout. The conn must be torn down
// within the bound of the arms above (WriteTimeout, then the closing drain),
// and the client, which starts to read only then, must not be sent the
// response: what it gets is what the kernel held at the teardown (the server's
// 64 KiB send buffer and the client's window).
func TestIouringStalledKeepAliveResponseIsBoundedByWriteTimeout884(t *testing.T) {
	const body = 16 << 20
	const bound = 20 * time.Second
	for _, async := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "async"}[async], func(t *testing.T) {
			h := &busyHandler811{body: make([]byte, body)}
			e, addr := startFDLEngine(t, h, func(c *resource.Config) {
				c.AsyncHandlers = async
				c.Resources.SocketSend = busy811SndBuf
				c.WriteTimeout = time.Second
				c.ReadTimeout = -1
				c.IdleTimeout = -1
				c.ReadHeaderTimeout = -1
			})
			cl := dialBusy811(t, addr)
			cl.send(t, "/big")
			t0 := time.Now()
			var gone time.Duration
			for time.Since(t0) < bound {
				time.Sleep(20 * time.Millisecond)
				if e.Metrics().ActiveConnections == 0 {
					gone = time.Since(t0)
					break
				}
			}
			_ = cl.c.SetReadDeadline(time.Now().Add(20 * time.Second))
			n, _ := io.Copy(io.Discard, cl.br)
			t.Logf("celeris884 STALLKA async=%v write_timeout_ms=1000 gone_ms=%d (0: still open after %d ms) client_received=%d of %d",
				async, gone.Milliseconds(), bound.Milliseconds(), n, body)
			if gone == 0 {
				t.Errorf("celeris#884 item 64: the conn of a keep-alive client that read nothing was still open %v in with WriteTimeout 1s", bound)
			}
			if n >= body/2 {
				t.Errorf("celeris#884 item 64: the client that read nothing for %v then received %d of %d response bytes "+
					"with WriteTimeout 1s", bound, n, body)
			}
		})
	}
}
