package stream

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/mono"
)

// celeris#1190. D2 for a pool handler that waits for the peer's window (celeris#1170):
// its record is judged at the end of its window by the handler's OWN timer, not by the
// sweep (ExpiredWrites skips waiters), so it is a separate path from the buffered
// streams the conn tests cover. The clock is mono.Manual: it moves only when the test
// moves it, so no verdict can come before Advance, whatever the runner's speed, and
// the timer (a real one, WriteTimeout long) re-arms itself against the clock until the
// time has passed.
//
// For both directions of a step of the wall clock (75 s): the step alone must give no
// verdict (a wait cut early), and the time that then passes must (a wait held past its
// bound, which a backward step does to a clock on the wall).
func TestWaiterWindowIgnoresAStepOfTheWallClock1190(t *testing.T) {
	for _, tc := range []struct {
		name string
		step time.Duration
	}{{"forward", 75 * time.Second}, {"backward", -75 * time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			clk := mono.NewManual()
			t.Cleanup(mono.SetManual(clk))
			m, s := waitRig1170(t, wait1170, 0) // nothing in flight: the first window is the verdict
			var w SendWait
			done := make(chan error, 1)
			go func() { done <- s.AwaitSendWindowRate(&w, 4<<20) }()
			t.Cleanup(func() { w.End(s, s.Gen()) })

			// The wait blocked: its record exists. (Polled; no time is assumed.)
			for dl := time.Now().Add(10 * time.Second); m.stalledStreams() == 0; time.Sleep(time.Millisecond) {
				if time.Now().After(dl) {
					t.Fatal("the wait did not block")
				}
			}
			// Several of the waiter's timer periods of real time, the clock still: no verdict.
			noVerdict := func(what string) {
				t.Helper()
				select {
				case err := <-done:
					t.Fatalf("%s: the wait ended with %v before its window did", what, err)
				case <-time.After(4 * wait1170):
				}
			}
			noVerdict("before anything moved")
			clk.Step(tc.step)
			noVerdict("after a step of " + tc.step.String())
			clk.Advance(2 * wait1170)
			select {
			case err := <-done:
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("err %v, want os.ErrDeadlineExceeded", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("the window ended (%v of time, step %v) and the wait was not judged: held past its bound", 2*wait1170, tc.step)
			}
		})
	}
}
