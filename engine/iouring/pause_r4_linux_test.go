//go:build linux

package iouring

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/deferlinger"
)

// TestPauseAcceptWaitIsWoken: PauseAccept waits for its workers to close their
// listeners by blocking on the engine's pause notification, which each
// worker's close and ResumeAccept deliver, not by polling (celeris#662 review: a
// 1 ms sleep loop through the 1.5 s linger).
//
// The wait also re-checks on a 100 ms timer (deferlinger.WaitRecheck), so a
// single pause can end on that timer even with notifications working, when
// its close or resume lands on a re-check: a race on one observation, never
// on all of them. So the notification is asserted over several pauses, and
// only its total absence fails: with no notification every one of them ends
// on the timer.
//
//   - one pause at the default linger: it takes the linger, closes every
//     listener, and re-checks no more often than its length allows;
//   - five pauses at a 50 ms linger: the listeners' close wakes the wait;
//   - five pauses withdrawn by ResumeAccept at 130-330 ms (never a multiple of
//     the re-check): the resume wakes the wait.
func TestPauseAcceptWaitIsWoken(t *testing.T) {
	r := startLingerL662(t)
	s0 := deferlinger.Snapshot()
	t0 := time.Now()
	_ = r.e.PauseAccept()
	took := time.Since(t0)
	s1 := deferlinger.Snapshot()
	rechecks := s1.WaitRechecks - s0.WaitRechecks
	t.Logf("workers=%d default linger: pause took %v, woken=%d rechecks=%d", r.workers,
		took.Round(time.Millisecond), s1.WaitWoken-s0.WaitWoken, rechecks)
	if !r.closed() {
		t.Fatal("PauseAccept returned with a listener still open")
	}
	if took < deferlinger.Linger() {
		t.Fatalf("premise: the pause took %v, shorter than the linger %v", took, deferlinger.Linger())
	}
	if limit := uint64(took/deferlinger.WaitRecheck) + 2; rechecks > limit {
		t.Errorf("PauseAccept re-checked %d times over %v, want at most %d: it polls", rechecks, took, limit)
	}

	resumeAndWait := func() {
		t.Helper()
		_ = r.e.ResumeAccept()
		for dl := time.Now().Add(3 * time.Second); len(mustListenersL662(t, r.port)) < r.workers; {
			if time.Now().After(dl) {
				t.Fatal("the listeners were not re-created after the resume")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// The close wakes the wait.
	old := deferlinger.SetLinger(50 * time.Millisecond)
	t.Cleanup(func() { deferlinger.SetLinger(old) })
	var byClose uint64
	for i := range 5 {
		resumeAndWait()
		a := deferlinger.Snapshot()
		t1 := time.Now()
		_ = r.e.PauseAccept()
		d := time.Since(t1)
		b := deferlinger.Snapshot()
		if !r.closed() {
			t.Fatalf("close cycle %d: PauseAccept returned with a listener still open", i)
		}
		byClose += b.WaitWoken - a.WaitWoken
		t.Logf("close cycle %d: pause took %v, woken=%d rechecks=%d", i, d.Round(10*time.Microsecond),
			b.WaitWoken-a.WaitWoken, b.WaitRechecks-a.WaitRechecks)
	}
	deferlinger.SetLinger(old)
	if byClose == 0 {
		t.Error("in 5 pauses no listener close woke PauseAccept's wait: every one ended on its re-check timer")
	}

	// A resume wakes the wait.
	var byResume uint64
	for i, after := range []time.Duration{130, 170, 230, 270, 330} {
		resumeAndWait()
		returned := make(chan time.Time, 1)
		a := deferlinger.Snapshot()
		go func() {
			_ = r.e.PauseAccept()
			returned <- time.Now()
		}()
		time.Sleep(after * time.Millisecond)
		tResume := time.Now()
		_ = r.e.ResumeAccept()
		var back time.Time
		select {
		case back = <-returned:
		case <-time.After(3 * time.Second):
			t.Fatalf("resume cycle %d: PauseAccept did not return after the resume", i)
		}
		b := deferlinger.Snapshot()
		byResume += b.WaitWoken - a.WaitWoken
		t.Logf("resume cycle %d at +%dms: PauseAccept returned %v after ResumeAccept, woken=%d rechecks=%d",
			i, after, back.Sub(tResume).Round(10*time.Microsecond), b.WaitWoken-a.WaitWoken,
			b.WaitRechecks-a.WaitRechecks)
	}
	if byResume == 0 {
		t.Error("in 5 pauses withdrawn by ResumeAccept no resume woke PauseAccept's wait: every one " +
			"ended on its re-check timer")
	}
}
