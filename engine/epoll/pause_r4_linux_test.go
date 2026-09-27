//go:build linux

package epoll

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/deferlinger"
)

// TestLingerTimeoutNeverBlocksPastTheDeadline: while a loop lingers, the
// epoll_wait timeout is capped at the time left to the linger's deadline --
// including an infinite wait (-1), which returned -1 unchanged and would have
// let a lingering loop sleep through its deadline (celeris#662 review).
func TestLingerTimeoutNeverBlocksPastTheDeadline(t *testing.T) {
	l := &Loop{}
	l.lingerUntil = deferlinger.After(50 * time.Millisecond)
	for _, ms := range []int{-1, 0, 1, 25, 1000} {
		got := l.lingerTimeoutMs(ms)
		switch {
		case got < 0:
			t.Errorf("lingerTimeoutMs(%d) = %d while lingering: a negative timeout blocks until an "+
				"event, past the linger's deadline", ms, got)
		case got > 50:
			t.Errorf("lingerTimeoutMs(%d) = %d, beyond the %d ms left to the deadline", ms, got, 50)
		case ms >= 0 && got > ms:
			t.Errorf("lingerTimeoutMs(%d) = %d, longer than the wait it was asked to cap", ms, got)
		}
	}
	l.lingerUntil = deferlinger.After(-time.Millisecond)
	for _, ms := range []int{-1, 0, 25} {
		if got := l.lingerTimeoutMs(ms); got != 0 {
			t.Errorf("lingerTimeoutMs(%d) = %d with the deadline passed, want 0", ms, got)
		}
	}
}

// TestPauseAcceptWaitIsWoken: PauseAccept waits for its loops to close their
// listeners by blocking on the engine's pause notification, which each loop's
// close and ResumeAccept deliver, not by polling (celeris#662 review: a 1 ms
// sleep loop through the 1.5 s linger). The wait's counters say how it ended:
// at least one wakeup by a notification, and no more timer re-checks than the
// pause's length allows at deferlinger.WaitRecheck. A resume while a
// PauseAccept waits wakes it too.
func TestPauseAcceptWaitIsWoken(t *testing.T) {
	r := startLingerL662(t)
	s0 := deferlinger.Snapshot()
	t0 := time.Now()
	_ = r.e.PauseAccept()
	took := time.Since(t0)
	s1 := deferlinger.Snapshot()
	woken, rechecks := s1.WaitWoken-s0.WaitWoken, s1.WaitRechecks-s0.WaitRechecks
	t.Logf("loops=%d pause took %v: woken=%d rechecks=%d", r.workers, took.Round(time.Millisecond),
		woken, rechecks)
	if !r.closed() {
		t.Fatal("PauseAccept returned with a listener still open")
	}
	if took < deferlinger.Linger() {
		t.Fatalf("premise: the pause took %v, shorter than the linger %v", took, deferlinger.Linger())
	}
	if woken == 0 {
		t.Error("PauseAccept's wait was never woken by a notification: the listeners' close " +
			"does not wake it, so it returned on a timer (or polled)")
	}
	if limit := uint64(took/deferlinger.WaitRecheck) + 2; rechecks > limit {
		t.Errorf("PauseAccept re-checked %d times over %v, want at most %d", rechecks, took, limit)
	}

	// A resume wakes a waiting PauseAccept.
	_ = r.e.ResumeAccept()
	for dl := time.Now().Add(3 * time.Second); len(mustListenersL662(t, r.port)) < r.workers; {
		if time.Now().After(dl) {
			t.Fatal("the listeners were not re-created after the resume")
		}
		time.Sleep(5 * time.Millisecond)
	}
	returned := make(chan time.Time, 1)
	go func() {
		_ = r.e.PauseAccept()
		returned <- time.Now()
	}()
	time.Sleep(300 * time.Millisecond)
	s2 := deferlinger.Snapshot()
	tResume := time.Now()
	_ = r.e.ResumeAccept()
	var back time.Time
	select {
	case back = <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("PauseAccept did not return after the resume")
	}
	s3 := deferlinger.Snapshot()
	t.Logf("resume: PauseAccept returned %v after ResumeAccept; woken=%d rechecks=%d",
		back.Sub(tResume).Round(10*time.Microsecond), s3.WaitWoken-s2.WaitWoken, s3.WaitRechecks-s2.WaitRechecks)
	if s3.WaitWoken == s2.WaitWoken {
		t.Error("the resume did not wake the waiting PauseAccept: it returned on its re-check timer")
	}
}
