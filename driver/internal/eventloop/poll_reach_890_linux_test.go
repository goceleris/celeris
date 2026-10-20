//go:build linux

package eventloop

// celeris#890 item 36, RULE 74: BenchmarkWriteAndPollSlow890 is the benchmark
// that is supposed to run the slow path (Phase B and C) of WriteAndPoll and
// WriteAndPollBusy, where testHookBeforePoll is. The benchmark itself cannot
// say whether it does (the A/B runs compare it against a base that has no
// hook), so this test drives the same shape with the hook set and counts.
//
// The benchmark's responder replies on its own thread, so which phase serves a
// call depends on how fast that thread wakes against the caller's Phase A
// read: WriteAndPoll/ReplyAtOnce reached Phase B's hook in 15% to 100% of the
// calls in an unloaded arm64 container, and in as few as 1% (3 of 300) under
// -race -covermode=atomic on two CPUs with six busy loops, where a floor of 5%
// failed in 10 of 30 runs (CI saw 8 of 300). No count of such a race can be
// asserted. Here the responder is gated
// (slow890Responder's gate): it reads the request and spins for the row's
// delay as the benchmark's does, then holds the reply until the hook of the
// row's phase hands it a token. The reply cannot exist before the call has
// reached that hook, so Phase A (one read, or Busy's 64) finds nothing and
// every call reaches Phase B's hook, and with the reply held at Phase C's
// hook, Phase B's poll rounds find nothing and every call reaches Phase C's.
// The assertions are then exact (every call, not a share of them), and the
// scheduler decides only whether Phase B's rounds or Phase C's poll collect
// the reply, which the test does not ask.
//
// A call that skips a phase's hook is also caught at once: the responder is
// still holding the reply, so the test fails on that call by name instead of
// waiting for a reply that cannot come.

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSlowPathBenchShapeReachesThePollHook890(t *testing.T) {
	type call func(w *worker, fd int, req, rbuf []byte, onRecv func([]byte)) (bool, error)
	for _, bc := range []struct {
		name string
		call call
	}{
		{"WriteAndPoll", func(w *worker, fd int, req, rbuf []byte, onRecv func([]byte)) (bool, error) {
			return w.WriteAndPoll(fd, req, rbuf, onRecv)
		}},
		{"WriteAndPollBusy", func(w *worker, fd int, req, rbuf []byte, onRecv func([]byte)) (bool, error) {
			return w.WriteAndPollBusy(fd, req, rbuf, onRecv)
		}},
	} {
		for _, d := range []struct {
			name  string
			delay time.Duration
			// the hook that releases the reply: the call has reached
			// Phase B's (ReplyAtOnce) or Phase C's (ReplyAfter200us) hook
			releaseAt byte
		}{
			{"ReplyAtOnce", 0, 'B'},
			{"ReplyAfter200us", 200 * time.Microsecond, 'C'},
		} {
			t.Run(bc.name+"/"+d.name, func(t *testing.T) {
				fd, peer := socketPair(t)
				t.Cleanup(func() { _ = unix.Close(fd); _ = unix.Close(peer) })
				w, err := newWorker(0)
				if err != nil {
					t.Fatalf("newWorker: %v", err)
				}
				t.Cleanup(func() { _ = w.shutdown() })
				if err := w.RegisterConn(fd, func([]byte) {}, func(error) {}); err != nil {
					t.Fatalf("RegisterConn: %v", err)
				}
				resp := make([]byte, 64)
				// One token per call: the hook of the releasing phase runs
				// once per call, the call returns, and the test collects
				// the whole reply before the next call.
				gate := make(chan struct{}, 1)
				slow890Responder(t, peer, resp, d.delay, gate)
				var nB, nC int
				testHookBeforePoll = func(_ int, phase byte) {
					if phase == 'B' {
						nB++
					} else {
						nC++
					}
					if phase == d.releaseAt {
						select {
						case gate <- struct{}{}:
						default:
						}
					}
				}
				t.Cleanup(func() { testHookBeforePoll = nil })
				const n = 300
				req, rbuf := make([]byte, 32), make([]byte, 16<<10)
				for i := range n {
					got := 0
					bBefore, cBefore := nB, nC
					ok, err := bc.call(w, fd, req, rbuf, func(p []byte) { got += len(p) })
					// (false, nil) is a call that gave up before the reply
					// (Phase C's poll(1ms) timed out, which a slow host or
					// -race can cause with the reply released only at the
					// hook); it still reached the hooks, and the reply is
					// collected below.
					if err != nil || (!ok && got > 0) {
						t.Fatalf("call %d: (%v, %v) after %d bytes", i, ok, err, got)
					}
					// The reply is released by the releasing phase's hook, so
					// a call that did not run it left the reply unsent.
					if (d.releaseAt == 'B' && nB != bBefore+1) || (d.releaseAt == 'C' && (nB != bBefore+1 || nC != cBefore+1)) {
						t.Fatalf("call %d: Phase B's hook ran %d times and Phase C's %d times (returned %v, %v): this benchmark shape does not reach the %c hook", i, nB-bBefore, nC-cBefore, ok, err, d.releaseAt)
					}
					// If the call gave up before the reply (Phase C timed out),
					// collect it so the next call starts clean.
					for dl := time.Now().Add(5 * time.Second); got < len(resp); time.Sleep(time.Millisecond) {
						if m, _ := unix.Read(fd, rbuf); m > 0 {
							got += m
						}
						if time.Now().After(dl) {
							t.Fatalf("call %d: the reply never arrived (%d of %d bytes)", i, got, len(resp))
						}
					}
				}
				testHookBeforePoll = nil
				t.Logf("%s %s: %d calls reached Phase B's hook %d times and Phase C's %d times", bc.name, d.name, n, nB, nC)
				if nB != n {
					t.Errorf("Phase B's hook ran %d of %d calls: this benchmark shape does not reach it", nB, n)
				}
				if d.releaseAt == 'C' && nC != n {
					t.Errorf("Phase C's hook ran %d of %d calls: this benchmark shape does not reach it", nC, n)
				}
			})
		}
	}
}
