//go:build linux

package eventloop

// celeris#890 item 36, RULE 74: BenchmarkWriteAndPollSlow890 is the benchmark
// that is supposed to run the slow path (Phase B and C) of WriteAndPoll and
// WriteAndPollBusy, where testHookBeforePoll is. The benchmark itself cannot
// say whether it does (the A/B runs compare it against a base that has no
// hook), so this test drives the same shape with the hook set and counts. Which
// phase serves a call depends on how fast the responder's thread wakes, so the
// counts vary from run to run. Measured (arm64 container, 300 calls per run,
// -race -count=20, no-race -count=20 and -race -cpu=1 -count=10): in the
// asserted rows Phase B's hook ran in 15% to 100% of the calls and Phase C's in
// 80% to 100% of the delayed ones. WriteAndPollBusy/ReplyAtOnce is not asserted
// (its 64-read Phase A usually catches the reply: Phase B ran in 1% to 92%).
// The test asks for 5% of the calls, which a benchmark that did not reach the
// site would miss by far (0 of 2000 for BenchmarkWriteAndPoll784's shape).

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
			// the phases whose hook must run in at least 5% of the calls
			wantB, wantC bool
		}{
			{"ReplyAtOnce", 0, true, false},
			{"ReplyAfter200us", 200 * time.Microsecond, true, true},
		} {
			// WriteAndPollBusy's Phase A is 64 tight reads: they catch an
			// immediate reply, so only the delayed reply must reach its hooks.
			if bc.name == "WriteAndPollBusy" && d.delay == 0 {
				d.wantB = false
			}
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
				slow890Responder(t, peer, resp, d.delay)
				var nB, nC int
				testHookBeforePoll = func(_ int, phase byte) {
					if phase == 'B' {
						nB++
					} else {
						nC++
					}
				}
				t.Cleanup(func() { testHookBeforePoll = nil })
				const n = 300
				req, rbuf := make([]byte, 32), make([]byte, 16<<10)
				for i := range n {
					got := 0
					ok, err := bc.call(w, fd, req, rbuf, func(p []byte) { got += len(p) })
					// (false, nil) is a call that gave up before the reply
					// (Phase C's poll(1ms) timed out, which a slow host or
					// -race can cause with a 200us reply); it still reached
					// the hooks, and the reply is collected below.
					if err != nil || (!ok && got > 0) {
						t.Fatalf("call %d: (%v, %v) after %d bytes", i, ok, err, got)
					}
					// If the call gave up before the reply (Phase C timed out),
					// collect it so the next call starts clean.
					for dl := time.Now().Add(2 * time.Second); got < len(resp); time.Sleep(time.Millisecond) {
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
				if d.wantB && nB < n/20 {
					t.Errorf("Phase B's hook ran %d of %d calls: this benchmark shape does not reach it", nB, n)
				}
				if d.wantC && nC < n/20 {
					t.Errorf("Phase C's hook ran %d of %d calls: this benchmark shape does not reach it", nC, n)
				}
			})
		}
	}
}
