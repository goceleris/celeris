//go:build linux

package eventloop

// celeris#890: the caller-side reads of the WriteAndPoll* calls, after a
// teardown. TestWriteAndPollStopsReadingAfterUnregisterConn784 tears the conn
// down inside the first onRecv, which pins the reads that follow a delivery.
// Here the teardown comes between a call's last read and its poll(2): the
// call has read nothing yet (Phase A found the socket empty), the owner
// unregisters and closes the fd, and a new socket X takes the number with
// bytes queued. The poll then reports POLLIN for X, and the read the call
// issues next must be refused (readOpen's closed check), not served from X:
// X's bytes stay with X and the call reports ErrUnknownFD.
//
// The phases are the ones of WriteAndPoll and WriteAndPollBusy (B: the
// poll(0) rounds, C: the poll(1ms)); WriteAndPollMulti has no Phase B/C and
// its seam is isDone (multi_seam_784_linux_test.go). The worker has no
// goroutine of its own (newWorker, no run), so nothing but the call under
// test reads the conn.

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

func TestWriteAndPollPhasesRefuseAReadOfTheNumberAfterATeardown890(t *testing.T) {
	type call func(w *worker, fd int, rbuf []byte, onRecv func([]byte)) (bool, error)
	calls := []struct {
		name string
		call call
	}{
		{"WriteAndPoll", func(w *worker, fd int, rbuf []byte, onRecv func([]byte)) (bool, error) {
			return w.WriteAndPoll(fd, []byte("q"), rbuf, onRecv)
		}},
		{"WriteAndPollBusy", func(w *worker, fd int, rbuf []byte, onRecv func([]byte)) (bool, error) {
			return w.WriteAndPollBusy(fd, []byte("q"), rbuf, onRecv)
		}},
	}
	for _, tc := range calls {
		for _, phase := range []byte{'B', 'C'} {
			t.Run(tc.name+"/Phase"+string(phase), func(t *testing.T) {
				w, err := newWorker(0)
				if err != nil {
					t.Fatalf("newWorker: %v", err)
				}
				t.Cleanup(func() { _ = w.shutdown() })

				a, aPeer := socketPair(t)
				t.Cleanup(func() { _ = unix.Close(aPeer) })
				if err := w.RegisterConn(a, func([]byte) {}, func(error) {}); err != nil {
					t.Fatalf("RegisterConn(A): %v", err)
				}

				var seen []byte
				x := -1
				var xPeer int
				testHookBeforePoll = func(fd int, ph byte) {
					seen = append(seen, ph)
					if fd != a || ph != phase || x >= 0 {
						return
					}
					if err := w.UnregisterConn(a); err != nil {
						t.Errorf("UnregisterConn(A): %v", err)
					}
					if err := unix.Close(a); err != nil {
						t.Errorf("close A: %v", err)
					}
					x, xPeer = c784TakeNumber(t, a, true)
					c784Queue(t, xPeer, []byte(c784Stolen))
				}
				t.Cleanup(func() { testHookBeforePoll = nil })

				var onRecvGot [][]byte
				ok, cerr := tc.call(w, a, make([]byte, 1024), func(b []byte) {
					onRecvGot = append(onRecvGot, append([]byte(nil), b...))
				})
				testHookBeforePoll = nil
				if x < 0 {
					t.Fatalf("the hook never ran for phase %c (phases seen: %q; call returned %v, %v): the test did not reach the poll", phase, seen, ok, cerr)
				}
				got, rerr := c784ReadAll(x)
				t.Logf("C890 %s phase %c: phases seen %q; returned (%v, %v); X reads %q (err %v); onRecv got %d chunk(s)",
					tc.name, phase, seen, ok, cerr, got, rerr, len(onRecvGot))
				if got != c784Stolen {
					t.Errorf("X, which took A's number before the Phase %c poll, reads %q (err %v), want %q: %s read X's bytes", phase, got, rerr, c784Stolen, tc.name)
				}
				if c784Contains(onRecvGot, c784Stolen) {
					t.Errorf("%s handed X's bytes to A's onRecv", tc.name)
				}
				if !errors.Is(cerr, engine.ErrUnknownFD) {
					t.Errorf("%s returned (%v, %v) for a conn torn down before its Phase %c poll, want engine.ErrUnknownFD", tc.name, ok, cerr, phase)
				}
			})
		}
	}
}
