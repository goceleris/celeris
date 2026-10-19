//go:build linux

package eventloop

// The cost of the slow path of WriteAndPoll and WriteAndPollBusy: the calls
// whose Phase A read finds nothing and go on to Phase B (16 poll(0) rounds) and
// Phase C (poll(1ms)), where the testHookBeforePoll sites of celeris#890 are.
// BenchmarkWriteAndPoll784 (read_cost_784_bench_linux_test.go) queues the reply
// before each call, so Phase A always reads data and the slow path never runs:
// it measures the fast path and code layout only.
//
// Here the reply is written by a responder goroutine after the request has
// arrived, so Phase A finds nothing:
//
//	ReplyAtOnce:  the responder writes as soon as it wakes (tens of
//	              microseconds after the request). WriteAndPoll: its single
//	              Phase A read usually misses, so Phase B's hook runs on most
//	              calls and Phase C's on some (on a loaded host the responder
//	              can beat the read: as few as 3 of 300 calls reached Phase B). WriteAndPollBusy: its Phase A is 64 tight
//	              reads (about 15us), which often catch this reply, so the
//	              hook runs on fewer calls (measured, arm64: Phase B's hook
//	              ran in 1% to 98% of the calls, varying run to run); this row
//	              is partly its fast path
//	ReplyAfter200us: the responder spins 200us first: both calls miss in Phase A
//	              and Phase B, and Phase C's poll(1ms) reads it; both hooks
//	              run on almost every call (a loaded host can delay the
//	              caller past the 200us)
//
// This file calls no hook and no code of this change, so the same file builds
// on the base commit and the A/B runs compare the two binaries on identical
// benchmark code. TestSlowPathBenchShapeReachesThePollHook890 (the test side,
// poll_reach_890_linux_test.go) drives this shape with the reply held back
// until the hook site has run, and counts the hook sites it reaches.

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// slow890Responder answers each request that arrives on peer with resp, after
// spinning for delay and, when gate is not nil, after a receive from gate (the
// benchmark passes nil; the test hands the responder a token from the hook
// site, so the reply cannot arrive before the call has got there). It stops,
// and is waited for, in tb's cleanup. Register it after the worker's cleanup
// (cleanups run last-in first-out), so it is gone before the descriptors close.
func slow890Responder(tb testing.TB, peer int, resp []byte, delay time.Duration, gate <-chan struct{}) {
	tb.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var buf [256]byte
		pfd := []unix.PollFd{{Fd: int32(peer), Events: unix.POLLIN}}
		for {
			select {
			case <-stop:
				return
			default:
			}
			pfd[0].Revents = 0
			if n, _ := unix.Poll(pfd, 20); n <= 0 {
				continue
			}
			n, err := unix.Read(peer, buf[:])
			for err == unix.EINTR {
				n, err = unix.Read(peer, buf[:])
			}
			if n <= 0 || err != nil {
				continue
			}
			for t0 := time.Now(); time.Since(t0) < delay; {
			}
			if gate != nil {
				select {
				case <-gate:
				case <-stop:
					return
				}
			}
			_, _ = unix.Write(peer, resp)
		}
	}()
	tb.Cleanup(func() {
		close(stop)
		<-done
	})
}

func BenchmarkWriteAndPollSlow890(b *testing.B) {
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
		}{
			{"ReplyAtOnce", 0},
			{"ReplyAfter200us", 200 * time.Microsecond},
		} {
			b.Run(bc.name+"/"+d.name, func(b *testing.B) {
				w, fd, peer := bench784Worker(b)
				req := make([]byte, 32)
				resp := make([]byte, 64)
				rbuf := make([]byte, 16<<10)
				slow890Responder(b, peer, resp, d.delay, nil)
				got := 0
				onRecv := func(p []byte) { got += len(p) }
				misses := 0
				b.ReportAllocs()
				for b.Loop() {
					got = 0
					ok, err := bc.call(w, fd, req, rbuf, onRecv)
					if err != nil || (!ok && got > 0) {
						b.Fatalf("%s: (%v, %v) after %d bytes", bc.name, ok, err, got)
					}
					if got < len(resp) {
						// The call gave up before the reply arrived (poll(1ms)
						// timed out: (false, nil)). Collect the reply so the
						// next op starts clean, and count the miss.
						misses++
						pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
						for dl := time.Now().Add(2 * time.Second); got < len(resp); {
							if time.Now().After(dl) {
								b.Fatalf("%s: the reply never arrived (%d of %d bytes)", bc.name, got, len(resp))
							}
							// A timeout or an EINTR (the runtime's preemption
							// signal) returns n <= 0: ask again until the deadline.
							if n, _ := unix.Poll(pfd, 100); n <= 0 {
								continue
							}
							n, err := unix.Read(fd, rbuf)
							if n > 0 {
								got += n
							} else if err != nil && err != unix.EAGAIN {
								b.Fatalf("%s: read: %v", bc.name, err)
							}
						}
					}
				}
				b.ReportMetric(float64(misses)/float64(b.N), "misses/op")
			})
		}
	}
}
