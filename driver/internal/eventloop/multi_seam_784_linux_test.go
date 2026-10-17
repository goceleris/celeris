//go:build linux

package eventloop

// celeris#784, #890: WriteAndPollMulti's caller-side reads after a teardown
// that comes between two of its polls. Its seam is isDone: the call runs it
// after the initial read (round 0) and after each poll round that read data.
// The test tears the conn down inside isDone and puts a new socket X on the
// number with bytes queued; the next read of the number must be refused, not
// served from X. The call has three reads that follow an isDone: the poll
// loop's read (tearAt 1: after the initial read's isDone; tearAt 2: after the
// first poll round's) and the final drain after "done:". A refused poll-loop
// read sets readErr, which skips the final drain, so the first two cases never
// reach it. The third does: isDone tears A down and returns true, the call
// goes to done with data in hand and no error, and the final drain's read is
// the next read. (No production hook: isDone is the call's own callback.)

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

func TestWriteAndPollMultiRefusesAReadOfTheNumberAfterATeardownInIsDone784(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tearAt   int  // the isDone call that tears A down
		finished bool // whether that isDone returns true (the call goes to done)
	}{
		{"AfterInitialRead", 1, false},
		{"AfterFirstPollRound", 2, false},
		{"FinalDrainAfterDone", 1, true},
	} {
		tearAt := tc.tearAt
		t.Run(tc.name, func(t *testing.T) {
			w, err := newWorker(0)
			if err != nil {
				t.Fatalf("newWorker: %v", err)
			}
			t.Cleanup(func() { _ = w.shutdown() })

			a, aPeer := socketPair(t)
			t.Cleanup(func() { _ = unix.Close(aPeer) })
			// Queued before the call: the initial read gets it, so isDone runs.
			c784Queue(t, aPeer, []byte("a"))
			if err := w.RegisterConn(a, func([]byte) {}, func(error) {}); err != nil {
				t.Fatalf("RegisterConn(A): %v", err)
			}

			calls := 0
			x := -1
			isDone := func() bool {
				calls++
				switch calls {
				case tearAt:
					if err := w.UnregisterConn(a); err != nil {
						t.Errorf("UnregisterConn(A): %v", err)
					}
					if err := unix.Close(a); err != nil {
						t.Errorf("close A: %v", err)
					}
					var xPeer int
					x, xPeer = c784TakeNumber(t, a, true)
					c784Queue(t, xPeer, []byte(c784Stolen))
					return tc.finished
				case 1:
					// tearAt 2: more of A's data for the first poll round.
					c784Queue(t, aPeer, []byte("b"))
				}
				return false
			}
			var onRecvGot [][]byte
			ok, cerr := w.WriteAndPollMulti(a, []byte("q"), make([]byte, 1024), func(b []byte) {
				onRecvGot = append(onRecvGot, append([]byte(nil), b...))
			}, isDone, nil)
			if x < 0 {
				t.Fatalf("isDone never reached call %d (%d calls; call returned %v, %v): the test did not drive the seam", tearAt, calls, ok, cerr)
			}
			got, rerr := c784ReadAll(x)
			t.Logf("C784 multi seam %s: isDone called %d times; returned (%v, %v); X reads %q (err %v)", tc.name, calls, ok, cerr, got, rerr)
			if got != c784Stolen {
				t.Errorf("X, which took A's number inside isDone, reads %q (err %v), want %q: WriteAndPollMulti read X's bytes", got, rerr, c784Stolen)
			}
			if c784Contains(onRecvGot, c784Stolen) {
				t.Errorf("WriteAndPollMulti handed X's bytes to A's onRecv")
			}
			if !errors.Is(cerr, engine.ErrUnknownFD) {
				t.Errorf("WriteAndPollMulti returned (%v, %v) for a conn torn down in isDone, want engine.ErrUnknownFD", ok, cerr)
			}
		})
	}
}
