//go:build linux

package iouring

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#884 item 1 (from celeris#762 item 2). The kernel header timer of a
// promoted async conn is armed from three places that no test reached with a
// deadline pending: handleRecv's async feed and promoteConnToAsync arm it from
// the deadline asyncHeaderDeadline read before the dispatch goroutine could
// change cs.h1State (celeris#722), and handleHeaderTimer re-arms it on an
// early fire. Deleting either arm survived the whole package (mutant Z12), and
// no test called handleHeaderTimer at all.
//
// Every test runs on an fdlFixture with AsyncHandlers on and ReadHeaderTimeout
// set, on a ring the kernel never sees (takeSQEs): the SQEs the worker places
// are the observation, the completions are the test's own.

const hdrTimeout762 = time.Hour // a deadline no test run can reach: an early fire is the only fire

// headerTimerFixture762 is an fdlFixture whose h1State carries a header
// deadline hdrTimeout762 away, as a conn that has just been accepted, with the
// timer SQE initProtocol placed taken off the ring and counted: one TIMEOUT
// tagged udHeaderTimer for this conn and generation. The conn is promoted when
// promoted is set, and a drain of its dispatch goroutine is registered as a
// cleanup either way.
func headerTimerFixture762(t *testing.T, promoted bool) *fdlFixture {
	t.Helper()
	f := newFDLFixture(t, true)
	f.w.cfg.ReadHeaderTimeout = hdrTimeout762
	f.w.initProtocol(f.cs) // a fresh h1State with the header deadline armed, as onAcceptedFD makes it
	if f.cs.detachMu == nil {
		t.Fatalf("apparatus: an async conn has no detachMu; snapshotH1Deadlines would read cs.h1State without a lock")
	}
	got := takeSQEs(f.w.ring)
	if len(got) != 1 || got[0].op != opTIMEOUT || got[0].ud != f.timerUD() || !f.cs.headerTimerArmed {
		t.Fatalf("apparatus: accept-time arm placed %v (headerTimerArmed=%v), want one TIMEOUT tagged udHeaderTimer for fd %d",
			got, f.cs.headerTimerArmed, f.fd)
	}
	if dl := f.cs.h1State.HeaderDeadlineNs.Load(); dl <= time.Now().UnixNano() {
		t.Fatalf("apparatus: HeaderDeadlineNs %d is not in the future", dl)
	}
	if promoted {
		f.cs.asyncPromoted.Store(true)
		f.armFirstRecv()
	}
	t.Cleanup(func() {
		f.cs.asyncInMu.Lock()
		f.cs.asyncClosed.Store(true)
		f.cs.asyncCond.Broadcast()
		f.cs.asyncInMu.Unlock()
		f.w.asyncWG.Wait()
	})
	return f
}

// timerUD is the user data of this conn's header-timer SQE.
func (f *fdlFixture) timerUD() uint64 { return encodeUserDataGen(udHeaderTimer, f.fd, f.gen) }

// timerCQE is the header timer's completion: the kernel's -ETIME.
func (f *fdlFixture) timerCQE() *completionEntry {
	return &completionEntry{UserData: f.timerUD(), Res: -int32(unix.ETIME)}
}

// timers762 are the TIMEOUT SQEs among recs, and whether anything else but a
// RECV is among them.
func timers762(recs []sqeRec) (timers []sqeRec, other []sqeRec) {
	for _, r := range recs {
		switch r.op {
		case opTIMEOUT:
			timers = append(timers, r)
		case opRECV:
		default:
			other = append(other, r)
		}
	}
	return timers, other
}

// wantOneTimer762 fails unless recs holds exactly one TIMEOUT, tagged for f's
// conn, and nothing besides it and RECVs.
func wantOneTimer762(t *testing.T, f *fdlFixture, what string, recs []sqeRec) {
	t.Helper()
	timers, other := timers762(recs)
	if len(timers) != 1 || timers[0].ud != f.timerUD() || len(other) != 0 {
		t.Fatalf("%s placed %v, want one TIMEOUT tagged udHeaderTimer for fd %d (and RECVs)", what, recs, f.fd)
	}
	if !f.cs.headerTimerArmed {
		t.Fatalf("%s placed the TIMEOUT but left headerTimerArmed false: a second arm would queue another", what)
	}
}

// partial762 is a request head with no blank line: the header deadline stays
// pending however many times it is delivered.
const partial762 = "GET /slow HTTP/1.1\r\nHost: x\r\n"

// more762 is one more header line of that head, for a later recv.
const more762 = "X-Slow: 1\r\n"

// TestAsyncFeedArmsTheHeaderTimer762 (Z12, the feed site): the conn is
// promoted and its dispatch goroutine is parked on a head that is not
// complete, so HeaderDeadlineNs is pending. The timer is then gone (it fired,
// or the SQ ring dropped the arm and the flag was cleared: the case
// handleRecv's comment describes), and the next recv the worker feeds to the
// goroutine must place a fresh TIMEOUT.
func TestAsyncFeedArmsTheHeaderTimer762(t *testing.T) {
	f := headerTimerFixture762(t, true)
	// The first feed: the accept-time timer is in flight (headerTimerArmed),
	// so the feed places none.
	f.deliver(partial762)
	waitDispatchIdle751(t, f, false)
	if timers, _ := timers762(takeSQEs(f.w.ring)); len(timers) != 0 {
		t.Fatalf("a feed placed %d TIMEOUTs with the accept-time timer still in flight", len(timers))
	}
	if !f.cs.headerTimerArmed {
		t.Fatalf("the accept-time timer's flag was cleared by a feed")
	}
	if f.cs.h1State.HeaderDeadlineNs.Load() == 0 {
		t.Fatalf("apparatus: the parked goroutine has no header deadline pending")
	}

	f.cs.headerTimerArmed = false
	f.deliver(more762)
	wantOneTimer762(t, f, "the feed after the timer was lost", takeSQEs(f.w.ring))
	waitDispatchIdle751(t, f, false)

	// The same feed with the timer in flight places none: the arm is
	// idempotent, a fast-cycling client does not queue timers without bound.
	f.deliver(more762)
	if timers, _ := timers762(takeSQEs(f.w.ring)); len(timers) != 0 {
		t.Fatalf("a feed placed %d TIMEOUTs with the timer in flight", len(timers))
	}
}

// TestAsyncPromotionArmsTheHeaderTimer762 (Z12, the promotion site) calls
// promoteConnToAsync with a deadline pending and no timer in flight. The
// deadline is staged: through the inline path ProcessH1 clears it before it
// returns ErrAsyncDispatch (the head is complete by then), so a real
// promotion finds none and this arm is not reached by traffic; the unit is
// what keeps it right for the day a promotion happens with one pending.
func TestAsyncPromotionArmsTheHeaderTimer762(t *testing.T) {
	f := headerTimerFixture762(t, false)
	f.cs.headerTimerArmed = false
	f.cs.asyncPromoted.Store(true) // the caller's step, per promoteConnToAsync's comment
	f.w.promoteConnToAsync(f.cs, f.fd, []byte(partial762), &completionEntry{Res: int32(len(partial762))})
	wantOneTimer762(t, f, "promoteConnToAsync", takeSQEs(f.w.ring))
}

// TestHeaderTimerEarlyFireRearms762 (handleHeaderTimer's early-fire branch,
// which no test reached): the timer's CQE arrives with the deadline still
// ahead, as it does on kernel clock drift. The conn must stay open and a
// fresh timer must be placed for the time that is left. The late fire, the
// completed head, a conn inside its handler and a conn already closing are
// the branches around it: none re-arms, and only the late fire closes.
func TestHeaderTimerEarlyFireRearms762(t *testing.T) {
	t.Run("early_rearms", func(t *testing.T) {
		f := headerTimerFixture762(t, false)
		dl := f.cs.h1State.HeaderDeadlineNs.Load()
		before := time.Now().UnixNano()
		f.process(f.timerCQE())
		after := time.Now().UnixNano()
		wantOneTimer762(t, f, "the early fire", takeSQEs(f.w.ring))
		if f.cs.closing || f.w.conns[f.fd] != f.cs {
			t.Fatalf("an early fire closed the conn (closing=%v)", f.cs.closing)
		}
		left := time.Duration(f.cs.headerTimerSpec.Sec)*time.Second + time.Duration(f.cs.headerTimerSpec.Nsec)
		if lo, hi := time.Duration(dl-after), time.Duration(dl-before); left < lo || left > hi {
			t.Fatalf("the fresh timer is set for %v, want what is left to the deadline when it was placed: between %v and %v", left, lo, hi)
		}
	})
	t.Run("late_closes", func(t *testing.T) {
		f := headerTimerFixture762(t, false)
		f.cs.h1State.HeaderDeadlineNs.Store(time.Now().UnixNano() - int64(time.Millisecond))
		f.process(f.timerCQE())
		if timers, _ := timers762(takeSQEs(f.w.ring)); len(timers) != 0 {
			t.Fatalf("a late fire placed %d TIMEOUTs", len(timers))
		}
		if f.w.conns[f.fd] == f.cs {
			t.Fatalf("a timer that fired past the deadline did not close the conn (closing=%v)", f.cs.closing)
		}
	})
	t.Run("headers_complete_is_a_noop", func(t *testing.T) {
		f := headerTimerFixture762(t, false)
		f.cs.h1State.ClearHeaderDeadline()
		f.process(f.timerCQE())
		if recs := takeSQEs(f.w.ring); len(recs) != 0 {
			t.Fatalf("a fire after the head completed placed %v", recs)
		}
		if f.cs.closing || f.w.conns[f.fd] != f.cs || f.cs.headerTimerArmed {
			t.Fatalf("a fire after the head completed: closing=%v inTable=%v headerTimerArmed=%v, want false/true/false",
				f.cs.closing, f.w.conns[f.fd] == f.cs, f.cs.headerTimerArmed)
		}
	})
	t.Run("detachMu_held_drops_the_timer", func(t *testing.T) {
		f := headerTimerFixture762(t, false)
		f.cs.detachMu.Lock()
		f.process(f.timerCQE())
		f.cs.detachMu.Unlock()
		if recs := takeSQEs(f.w.ring); len(recs) != 0 {
			t.Fatalf("a fire while the conn was inside its handler placed %v: re-arming there spins", recs)
		}
		if f.cs.closing || f.w.conns[f.fd] != f.cs || f.cs.headerTimerArmed {
			t.Fatalf("a fire while the conn was inside its handler: closing=%v inTable=%v headerTimerArmed=%v, want false/true/false",
				f.cs.closing, f.w.conns[f.fd] == f.cs, f.cs.headerTimerArmed)
		}
	})
	t.Run("closing_conn_is_left_alone", func(t *testing.T) {
		f := headerTimerFixture762(t, false)
		f.cs.closing = true
		f.process(f.timerCQE())
		if recs := takeSQEs(f.w.ring); len(recs) != 0 {
			t.Fatalf("a fire on a closing conn placed %v", recs)
		}
		if f.cs.headerTimerArmed {
			t.Fatalf("a fire on a closing conn left headerTimerArmed set")
		}
	})
}

// TestHeaderTimerRearmRacesH1StateSwap762 (-race): handleHeaderTimer reads the
// deadline under detachMu (snapshotH1Deadlines), because a conn's dispatch
// goroutine owns cs.h1State across ProcessH1 and sets it to nil when the
// request is an h2c upgrade (celeris#722). A goroutine stands in for that one
// and swaps cs.h1State between nil and the state under the lock, as
// switchToH2Local does, while the worker's side fires the early-fire path in a
// loop. A read of cs.h1State outside the lock is a data race the detector
// reports; the counts below keep the run from passing vacuously: the loop must
// have seen the state (and re-armed) and have missed it (nil, or the lock held).
func TestHeaderTimerRearmRacesH1StateSwap762(t *testing.T) {
	f := headerTimerFixture762(t, false)
	h1 := f.cs.h1State
	mu := f.cs.detachMu
	// Both loops yield on every step, so the test finishes at GOMAXPROCS=1
	// too (a 1-CPU container, `-cpu 1`): a goroutine that never yields there
	// is preempted once per scheduler slice, and 50000 swaps took minutes.
	// The wall-clock cap is the last resort; the non-vacuity check below still
	// applies when it is the thing that ends the run.
	const swaps = 20000
	const wallCap = 20 * time.Second
	var stop atomic.Bool
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < swaps && !stop.Load(); i++ {
			mu.Lock()
			f.cs.h1State = nil
			mu.Unlock()
			runtime.Gosched()
			mu.Lock()
			f.cs.h1State = h1
			mu.Unlock()
			runtime.Gosched()
		}
		// Leave the state as the fixture had it, whichever way the loop ended.
		mu.Lock()
		f.cs.h1State = h1
		mu.Unlock()
	}()
	start := time.Now()
	var rearmed, missed, iters int
	for finished := false; !finished || iters < 1000; iters++ {
		select {
		case <-done:
			finished = true
		default:
			if iters&1023 == 0 && time.Since(start) > wallCap {
				stop.Store(true)
			}
		}
		runtime.Gosched()
		f.cs.headerTimerArmed = false // the timer's CQE has been taken, as handleHeaderTimer's caller leaves it
		f.w.handleHeaderTimer(f.fd)
		if f.cs.headerTimerArmed {
			rearmed++
		} else {
			missed++
		}
		takeSQEs(f.w.ring)
	}
	wg.Wait()
	t.Logf("handleHeaderTimer ran %d times against %d swaps: re-armed %d, skipped %d", iters, swaps, rearmed, missed)
	if rearmed == 0 || missed == 0 {
		t.Fatalf("vacuous run: %d re-arms and %d skips; the swap and the early fire did not overlap", rearmed, missed)
	}
	if f.cs.closing {
		t.Fatalf("the conn was closed: the deadline is an hour away")
	}
}
