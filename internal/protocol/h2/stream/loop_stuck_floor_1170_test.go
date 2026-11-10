package stream

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/mono"
)

// celeris#1170, the loop-stuck fallback (LoopWatch): a loop is taken as stuck when it
// has been in ONE batch for max(WriteTimeout/2, floor). With the floor at 250 ms a
// healthy loop of a host that is overloaded or runs -race and a coverage profile (a
// batch of 0.4 to 1.9 s measured, mostly one h2 connection's write-queue drain) cut
// healthy waiters of 8 parallel downloads as "loop-blocked" (Coverage root shard c,
// run 41246492268; 7 of 72 cells with two busy loops on two CPUs). A blocked loop
// (a synchronous handler waiting for a lock a waiting pool handler holds) is never a
// batch that short, so the floor is 2 s.
//
// The clock is mono.Manual: the verdicts are exact, with no sleep and no margin for
// the speed of the runner.

func TestLoopStuckThresholdHasAFloor1170(t *testing.T) {
	for _, tc := range []struct{ wt, want time.Duration }{
		{100 * time.Millisecond, 2 * time.Second},
		{time.Second, 2 * time.Second},
		{4 * time.Second, 2 * time.Second},
		{5 * time.Second, 2500 * time.Millisecond},
		{10 * time.Second, 5 * time.Second},
		{60 * time.Second, 30 * time.Second}, // the default WriteTimeout: unchanged
	} {
		if got := time.Duration(loopStuckFor(int64(tc.wt))); got != tc.want {
			t.Errorf("loopStuckFor(%v) = %v, want %v", tc.wt, got, tc.want)
		}
	}
}

// A long batch that is not yet the threshold old is not a wedge; at the threshold it is.
func TestHealthyLongBatchIsNotAWedge1170(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wt     time.Duration
		age    time.Duration // how long the loop has been in its batch at the judgement
		waited time.Duration // how long the waiter has waited
		cut    bool
	}{
		{"WT 1s, batch of 0.8 s (healthy, instrumented)", time.Second, 800 * time.Millisecond, 20 * time.Second, false},
		{"WT 1s, batch of 1.5 s", time.Second, 1500 * time.Millisecond, 20 * time.Second, false},
		{"WT 1s, batch of 1.9 s", time.Second, 1900 * time.Millisecond, 20 * time.Second, false},
		{"WT 1s, batch of 2 s", time.Second, 2 * time.Second, 20 * time.Second, true},
		{"WT 1s, batch of 3 s, waited under a WriteTimeout", time.Second, 3 * time.Second, 500 * time.Millisecond, false},
		{"WT 5s, batch of 2.4 s", 5 * time.Second, 2400 * time.Millisecond, 20 * time.Second, false},
		{"WT 5s, batch of 2.6 s", 5 * time.Second, 2600 * time.Millisecond, 20 * time.Second, true},
		{"WT 60s (default), batch of 29 s", time.Minute, 29 * time.Second, 70 * time.Second, false},
		{"WT 60s (default), batch of 31 s", time.Minute, 31 * time.Second, 70 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := mono.NewManual()
			t.Cleanup(mono.SetManual(clk))
			m, s := waitRig1170(t, tc.wt, 0)
			lw := NewLoopWatch()
			lw.Slot().Enter(mono.Now() - int64(tc.age))
			w := SendWait{Loops: lw, start: mono.Now() - int64(tc.waited)}
			v, _ := m.judgeWait(s, s.ID, &w, 4<<20)
			if cut := v == waitExpired && w.BlockedLoop(); cut != tc.cut {
				t.Fatalf("verdict %v, loop-blocked %v: want cut=%v", v, w.BlockedLoop(), tc.cut)
			}
		})
	}
}

// The bound of a wedge: a loop that blocks while a waiter is waiting is cut at the first
// judgement (one per WriteTimeout window) at which its batch is max(WT/2, 2 s) old and
// the wait has lasted a WriteTimeout, so at most that plus one WriteTimeout after the
// loop blocked. Positive control: the fallback still cuts, within the stated bound.
func TestBlockedLoopIsCutWithinTheBound1170(t *testing.T) {
	for _, wt := range []time.Duration{time.Second, 5 * time.Second, 60 * time.Second} {
		t.Run(wt.String(), func(t *testing.T) {
			clk := mono.NewManual()
			t.Cleanup(mono.SetManual(clk))
			m, s := waitRig1170(t, wt, 0)
			lw := NewLoopWatch()
			slot := lw.Slot()
			start := mono.Now() // the waiter blocks
			clk.Advance(wt * 7 / 10)
			slot.Enter(mono.Now()) // the loop blocks 0.7 WT into the wait and never returns
			w := SendWait{Loops: lw, start: start}
			var cutAt time.Duration
			for k := 1; k <= 400 && cutAt == 0; k++ { // the waiter's timer: one judgement per window
				clk.Advance(wt)
				m.dropWait(s.ID) // the rate rule is not under test: the record is made afresh
				if v, _ := m.judgeWait(s, s.ID, &w, 4<<20); v == waitExpired {
					if !w.BlockedLoop() {
						t.Fatalf("cut by the rate rule at judgement %d", k)
					}
					cutAt = time.Duration(mono.Now() - slot.since.Load()) // how long the loop had been blocked
				}
			}
			bound := time.Duration(loopStuckFor(int64(wt))) + wt // threshold, then one window to the next judgement
			if cutAt == 0 {
				t.Fatalf("a blocked loop was never cut")
			}
			if cutAt < time.Duration(loopStuckFor(int64(wt))) || cutAt > bound {
				t.Fatalf("cut %v after the loop blocked, want within [%v, %v]", cutAt, time.Duration(loopStuckFor(int64(wt))), bound)
			}
		})
	}
}
