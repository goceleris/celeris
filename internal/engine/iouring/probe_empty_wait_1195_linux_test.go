//go:build linux

package iouring

import (
	"errors"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// waitCut1195 says how a cut wait of emptyWaits1195 ends.
type waitCut1195 int

const (
	// cutHide: the wait is the real one, and the completions it finds are
	// kept from the caller until the next wait: the probe sees what a wait
	// woken before its completion was posted shows, an empty CQ ring that a
	// later enter fills. The probes' completions are posted before their wait
	// (the dial comes first), so the kernel does not make this happen on a
	// ring that is not DEFER_TASKRUN: the test makes it happen.
	cutHide waitCut1195 = iota
	// cutSkip: the wait returns nil at once, without entering the kernel,
	// which is what SubmitAndWaitTimeout answers for a wait a signal ended
	// before it did anything.
	cutSkip
	// cutExpire: as cutHide, and the call then takes the whole of its time, as
	// a wait does that nothing ends: the probe's budget is gone when it looks.
	cutExpire
)

func (c waitCut1195) String() string {
	switch c {
	case cutSkip:
		return "skip"
	case cutExpire:
		return "expire"
	}
	return "hide"
}

// emptyWaits1195 replaces probeRingWait for one test: the calls whose number
// is in empty (1-based, counted over the whole probe) come back with nothing
// to read in the CQ ring and no error, as mode says. Every other call is the
// real wait. calls and cut say what happened.
func emptyWaits1195(t *testing.T, mode waitCut1195, empty ...int) (calls, cut *int) {
	t.Helper()
	saved := probeRingWait
	t.Cleanup(func() { probeRingWait = saved })
	calls, cut = new(int), new(int)
	var heldRing *Ring // the ring the last cut wait hid completions on,
	var held uint32    // and the CQ head it hid them behind
	probeRingWait = func(r *Ring, d time.Duration) error {
		*calls++
		if heldRing == r {
			r.EndCQ(held)
		}
		heldRing = nil
		isCut := false
		for _, n := range empty {
			isCut = isCut || n == *calls
		}
		if isCut && mode == cutSkip {
			*cut++
			return nil
		}
		err := r.SubmitAndWaitTimeout(d)
		if isCut {
			*cut++
			if h, tl := r.BeginCQ(); h != tl {
				heldRing, held = r, h
				r.EndCQ(tl)
			}
			if mode == cutExpire {
				time.Sleep(d)
			}
		}
		return err
	}
	return calls, cut
}

// TestProbesSurviveAnEmptyWait1195: the SEND_ZC, fixed-files and
// multishot-accept probes waited once for their completion and read the CQ
// ring once, and the result is cached for the process. A wait that returns
// with no completion (a signal, or a timer's wake before its CQE is posted,
// celeris#1183) made the probe report the feature unsupported, so the engine
// ran without it until the process ended. Each probe is run uncached (the
// cache would keep an injected result for every later test), once as the
// kernel answers it and then with some of its waits cut short (emptyWaits1195):
// the first, the second (SEND_ZC's notification wait) and three in a row, so
// that a probe which retries once fails too. The answer must not change.
func TestProbesSurviveAnEmptyWait1195(t *testing.T) {
	type cut struct {
		mode  waitCut1195
		calls []int
	}
	type probe struct {
		name string
		run  func() (string, string)
		// ok says the probe, run without a cut wait, found the feature working.
		ok func(res string) bool
		// cuts lists the waits to cut short, one list per run.
		cuts []cut
		// waits is how many waits a clean run makes at least.
		waits int
	}
	all := func(second bool) []cut {
		cuts := []cut{{cutHide, []int{1}}, {cutHide, []int{1, 2, 3}}, {cutSkip, []int{1}}}
		if second {
			cuts = append(cuts, cut{cutHide, []int{2}}, cut{cutSkip, []int{2}})
		}
		return cuts
	}
	probes := []probe{
		{
			name:  "send_zc",
			run:   func() (string, string) { r, why := probeSendZC(); return r.String(), why },
			ok:    func(res string) bool { return res == SendZCCopyFallback.String() || res == SendZCTrueZeroCopy.String() },
			cuts:  all(true),
			waits: 2,
		},
		{
			name:  "fixed_files",
			run:   func() (string, string) { ok, why := probeFixedFiles(); return boolName1195(ok), why },
			ok:    func(res string) bool { return res == "true" },
			cuts:  all(false),
			waits: 1,
		},
		{
			name:  "multishot_accept",
			run:   func() (string, string) { ok, why := probeMultishotAccept(); return boolName1195(ok), why },
			ok:    func(res string) bool { return res == "true" },
			cuts:  all(false),
			waits: 1,
		},
	}
	if r, err := NewRing(8, 0, 0); err != nil {
		skipOrFail656(t, "io_uring unavailable: %v", err)
	} else {
		_ = r.Close()
	}
	for _, p := range probes {
		calls, _ := emptyWaits1195(t, cutHide)
		want, wantWhy := p.run()
		if !p.ok(want) {
			t.Logf("%s: the kernel does not answer it with a working feature (%s, %q): nothing to keep", p.name, want, wantWhy)
			continue
		}
		if *calls < p.waits {
			t.Fatalf("%s: a clean run waited %d time(s), want at least %d: the cuts below are numbered for that", p.name, *calls, p.waits)
		}
		for _, c := range p.cuts {
			calls, cuts := emptyWaits1195(t, c.mode, c.calls...)
			got, why := p.run()
			if got != want {
				t.Errorf("%s cut(%v) %v: the probe answered %s (%q) when a wait came back empty, %s (%q) when it did not: "+
					"an empty wait was taken for the kernel's answer", p.name, c.mode, c.calls, got, why, want, wantWhy)
			} else if *cuts != len(c.calls) {
				// Only an answer that survived can say whether every cut was
				// made: a probe that gave up early made fewer calls, and its
				// wrong answer is the failure.
				t.Errorf("%s cut(%v) %v: %d wait(s) were cut short of %d (the probe made %d call(s)): the test did not inject what it set out to",
					p.name, c.mode, c.calls, *cuts, len(c.calls), *calls)
			}
		}
	}
}

func boolName1195(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestProbeWaitExpiryIsRetriedOnce1195: a wait whose whole budget goes by with
// nothing to read is not yet the kernel's answer for the three probes whose
// first completion is there at submit (SEND_ZC's send, the two accepts): the
// result is cached for the process, and the probe is cheap, so it is run once
// more with a new ring before a negative is kept. Once, not more: a kernel that
// really posts nothing costs two budgets, once. SEND_ZC's notification wait is
// the exception: the send completed with F_MORE and the notification did not
// come in 2 s, which is what an ENA NIC does (SendZCBroken), an answer; a retry
// there would cost every startup on such a host 2 s more. The budgets are
// shortened to 20 ms; the cut waits take their whole budget (cutExpire).
func TestProbeWaitExpiryIsRetriedOnce1195(t *testing.T) {
	if r, err := NewRing(8, 0, 0); err != nil {
		skipOrFail656(t, "io_uring unavailable: %v", err)
	} else {
		_ = r.Close()
	}
	type probe struct {
		name string
		run  func() (string, string)
		ok   func(res string) bool
		// waits is how many waits one whole run makes: SEND_ZC waits for its
		// send and then for the notification.
		waits int
	}
	probes := []probe{
		{"send_zc", func() (string, string) { r, why := probeSendZC(); return r.String(), why },
			func(res string) bool { return res == SendZCCopyFallback.String() || res == SendZCTrueZeroCopy.String() }, 2},
		{"fixed_files", func() (string, string) { ok, why := probeFixedFiles(); return boolName1195(ok), why },
			func(res string) bool { return res == "true" }, 1},
		{"multishot_accept", func() (string, string) { ok, why := probeMultishotAccept(); return boolName1195(ok), why },
			func(res string) bool { return res == "true" }, 1},
	}
	type budgets struct{ first, notif time.Duration }
	saved := budgets{probeFirstWaitBudget, probeNotifWaitBudget}
	t.Cleanup(func() { probeFirstWaitBudget, probeNotifWaitBudget = saved.first, saved.notif })
	for _, p := range probes {
		probeFirstWaitBudget, probeNotifWaitBudget = saved.first, saved.notif
		emptyWaits1195(t, cutHide)
		want, wantWhy := p.run()
		if !p.ok(want) {
			t.Logf("%s: the kernel does not answer it with a working feature (%s, %q): nothing to keep", p.name, want, wantWhy)
			continue
		}
		probeFirstWaitBudget, probeNotifWaitBudget = 20*time.Millisecond, 20*time.Millisecond

		// The first wait of the first attempt takes its whole budget with
		// nothing to read: the second attempt answers, in all its waits.
		calls, cuts := emptyWaits1195(t, cutExpire, 1)
		got, why := p.run()
		if got != want || *calls != 1+p.waits || *cuts != 1 {
			t.Errorf("%s, first wait expired: the probe answered %s (%q) after %d wait(s), %d expired; want %s after %d, 1 expired: "+
				"an expired wait is retried once", p.name, got, why, *calls, *cuts, want, 1+p.waits)
		}

		// Both attempts expire: a negative, after two budgets and two waits.
		start := time.Now()
		calls, cuts = emptyWaits1195(t, cutExpire, 1, 2)
		got, why = p.run()
		if p.ok(got) || *calls != 2 || *cuts != 2 || time.Since(start) > time.Second {
			t.Errorf("%s, both attempts expired: the probe answered %s (%q) after %d wait(s), %d expired, %v; want a negative after 2, 2 expired: "+
				"retried once, and only once", p.name, got, why, *calls, *cuts, time.Since(start).Round(time.Millisecond))
		}

		if p.name != "send_zc" {
			continue
		}
		// The notification wait expires: the kernel's answer, not retried.
		calls, cuts = emptyWaits1195(t, cutExpire, 2)
		got, why = p.run()
		if got != SendZCBroken.String() || *calls != 2 || *cuts != 1 {
			t.Errorf("send_zc, notification wait expired: the probe answered %s (%q) after %d wait(s), %d expired; want %s after 2, 1 expired: "+
				"a missing notification is an answer and is not retried", got, why, *calls, *cuts, SendZCBroken)
		}
	}
}

// TestProbeWaitCQEIsBounded1195: probeWaitCQE re-enters while the CQ ring is
// empty, and what it asks of each wait is bounded. A storm of waits that
// return at once with nothing to read (a signal storm) ends when the budget
// does, each wait is given a positive time that only shrinks (a zero or
// negative timespec makes the kernel answer at once or EINVAL), an error ends
// the call at the first wait, and a real wait on an empty ring takes the
// budget and no more. Nothing here waits longer than 40 ms.
func TestProbeWaitCQEIsBounded1195(t *testing.T) {
	r, err := NewRing(8, 0, 0)
	if err != nil {
		skipOrFail656(t, "io_uring unavailable: %v", err)
	}
	defer func() { _ = r.Close() }()
	saved := probeRingWait
	t.Cleanup(func() { probeRingWait = saved })

	const budget = 30 * time.Millisecond
	var asked []time.Duration
	probeRingWait = func(_ *Ring, d time.Duration) error { asked = append(asked, d); return nil }
	start := time.Now()
	h, tl, empty, err := probeWaitCQE(r, budget)
	took := time.Since(start)
	if err != nil || h != tl {
		t.Fatalf("a storm of empty waits on an empty ring: head %d tail %d err %v, want an empty ring and no error", h, tl, err)
	}
	if took < budget || took > time.Second {
		t.Errorf("a storm of empty waits ended after %v, want the %v budget and not much more", took, budget)
	}
	if empty != len(asked) || empty < 2 {
		t.Errorf("%d empty returns for %d waits, want one per wait and more than one: the call must re-enter", empty, len(asked))
	}
	for i, d := range asked {
		if d <= 0 || d > budget || (i > 0 && d > asked[i-1]) {
			t.Errorf("wait %d was given %v of a %v budget (the one before: %v), want a positive time that only shrinks", i, d, budget, asked[max(i-1, 0)])
			break
		}
	}

	boom := errors.New("ring failed")
	calls := 0
	probeRingWait = func(*Ring, time.Duration) error { calls++; return boom }
	if _, _, _, err := probeWaitCQE(r, budget); !errors.Is(err, boom) || calls != 1 {
		t.Errorf("a wait that failed: err %v after %d wait(s), want the error after 1", err, calls)
	}

	// The real wait on an empty ring: its timeout is the budget.
	probeRingWait = saved
	start = time.Now()
	h, tl, empty, err = probeWaitCQE(r, 40*time.Millisecond)
	took = time.Since(start)
	if err != nil || h != tl || empty < 1 || took < 40*time.Millisecond || took > time.Second {
		t.Errorf("a real wait on an empty ring: head %d tail %d, %d empty, err %v after %v, want an empty ring after about 40 ms", h, tl, empty, err, took)
	}
}

// TestProbeWaitCQESurvivesSignals1195: the same wait on the real kernel, with
// Go's preemption signal (SIGURG, a no-op for a goroutine in a syscall)
// sent to the waiting thread every 5 ms while a 150 ms timer is pending. The
// timer was submitted before the wait, so no enter has anything to submit
// and the kernel reports each signal's EINTR (SubmitAndWaitTimeout turns it
// into nil): the wait comes back empty again and again, and probeWaitCQE must
// still deliver the timer's CQE. Both a plain ring and a DEFER_TASKRUN one,
// whose CQE only a GETEVENTS enter posts.
func TestProbeWaitCQESurvivesSignals1195(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pid, tid := unix.Getpid(), unix.Gettid()
	rings := 0
	for _, fl := range []struct {
		name  string
		flags uint32
	}{
		{"plain", 0},
		{"defer_taskrun", setupDeferTaskrun | setupSingleIssuer},
	} {
		r, err := NewRing(8, fl.flags, 0)
		if err != nil {
			t.Logf("celeris1195 flags=%s: ring unavailable: %v", fl.name, err)
			continue
		}
		rings++
		spec := kernelTimespec{Nsec: int64(150 * time.Millisecond)}
		prepTimer1195(r.GetSQE(), &spec)
		if _, err := r.Submit(); err != nil {
			_ = r.Close()
			t.Fatalf("flags=%s: submit: %v", fl.name, err)
		}
		stop, done := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			for tk := time.NewTicker(5 * time.Millisecond); ; {
				select {
				case <-stop:
					tk.Stop()
					return
				case <-tk.C:
					_ = unix.Tgkill(pid, tid, unix.SIGURG)
				}
			}
		}()
		h, tl, empty, err := probeWaitCQE(r, 2*time.Second)
		close(stop)
		<-done
		runtime.KeepAlive(&spec)
		if err != nil || h == tl {
			_ = r.Close()
			t.Fatalf("flags=%s: head %d tail %d err %v after %d empty return(s), want the timer's CQE", fl.name, h, tl, err, empty)
		}
		res := r.cqeAt(h).Res
		r.EndCQ(h + 1)
		_ = r.Close()
		t.Logf("celeris1195 flags=%s empty_returns=%d res=%d", fl.name, empty, res)
		if res != -int32(unix.ETIME) {
			t.Errorf("flags=%s: the timer completed with res=%d, want -ETIME", fl.name, res)
		}
		if empty == 0 {
			t.Errorf("flags=%s: no signal ended the wait early: the test is not exercising an empty return", fl.name)
		}
	}
	if rings == 0 {
		skipOrFail656(t, "no ring could be set up with either flag set")
	}
}

// prepTimer1195 is prepTimeout with count 0 and flags 0 (a relative timer that
// ends only at its deadline) written out here: prepTimeout's three callers all
// pass 0 and 0, and a fourth makes unparam report the parameter it was given.
func prepTimer1195(sqePtr unsafe.Pointer, ts *kernelTimespec) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[0] = opTIMEOUT
	*(*uint64)(unsafe.Pointer(&sqe[16])) = uint64(uintptr(unsafe.Pointer(ts)))
	*(*uint32)(unsafe.Pointer(&sqe[24])) = 1 // len: must be 1
}
