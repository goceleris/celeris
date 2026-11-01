//go:build linux

package iouring

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
	"github.com/goceleris/celeris/internal/resource"
)

// resetProbeCaches1200 forgets what the four cached probes learned, so that a
// test sees them as a process that has never run one, and forgets again when
// the test ends so that nothing it injected is kept for a later test.
func resetProbeCaches1200(t *testing.T) {
	t.Helper()
	reset := func() {
		cachedSendZC.reset1200()
		cachedFixedFiles.reset1200()
		cachedPbufRing.reset1200()
		cachedMultiAccept.reset1200()
	}
	reset()
	t.Cleanup(reset)
}

// reset1200 puts the cache back to a process that has not run its probe.
func (c *probeCache[R]) reset1200() {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero R
	c.known, c.res, c.why, c.expired, c.failedWhy, c.logged = false, zero, "", 0, "", false
}

// swap1200 sets *p to v and returns the function that puts the old value
// back; the test's cleanup calls it too, so the seam is restored whatever
// happens, and calling it twice is harmless.
func swap1200[T any](t *testing.T, p *T, v T) (restore func()) {
	t.Helper()
	old := *p
	*p = v
	var once sync.Once
	restore = func() { once.Do(func() { *p = old }) }
	t.Cleanup(restore)
	return restore
}

// probe1200 is one cached startup probe, as the tests of celeris#1200 drive it.
type probe1200 struct {
	name string
	// logName is the probe's name in the line New logs for a probe that
	// could not run.
	logName string
	// cached is the probe through its process-wide cache, as New calls it;
	// direct is one uncached run, for the answer the kernel gives.
	cached, direct func() (string, string)
	// ok says the answer is a working feature.
	ok func(res string) bool
	// listens and waits say whether the probe opens the loopback listener
	// and waits on its ring through the seams (the provided-buffers probe
	// does neither).
	listens, waits bool
	// waitCalls is how many waits a clean run makes at least.
	waitCalls int
}

func probes1200() []probe1200 {
	zc := func(f func() (SendZCProbeResult, string)) func() (string, string) {
		return func() (string, string) { r, why := f(); return r.String(), why }
	}
	b := func(f func() (bool, string)) func() (string, string) {
		return func() (string, string) { ok, why := f(); return boolName1195(ok), why }
	}
	isTrue := func(res string) bool { return res == "true" }
	return []probe1200{
		{"send_zc", "SEND_ZC", zc(probeSendZCCached), zc(probeSendZC),
			func(res string) bool { return res == SendZCCopyFallback.String() || res == SendZCTrueZeroCopy.String() },
			true, true, 2},
		{"fixed_files", "fixed files", b(probeFixedFilesCached), b(probeFixedFiles), isTrue, true, true, 1},
		{"provided_buffers", "provided buffers", b(probeProvidedBuffersCached), b(probeProvidedBuffers), isTrue, false, false, 0},
		{"multishot_accept", "multishot accept", b(probeMultishotAcceptCached), b(probeMultishotAccept), isTrue, true, true, 1},
	}
}

// emfile1200 is what net.Listen returns in a process out of file descriptors.
func emfile1200(op string) error {
	return &net.OpError{Op: op, Net: "tcp", Err: os.NewSyscallError("socket", unix.EMFILE)}
}

// wantAnswer1200 is the answer of one real run of p, or ends the test with
// skipOrFail656 (a skip, which the CI steps that count SKIP lines turn red, or
// a failure where CELERIS_REQUIRE_IOURING_WORKERS=1) for a kernel that does not
// answer with a working feature: there is then nothing the second call could
// show, and a pass would say otherwise.
func wantAnswer1200(t *testing.T, p probe1200) (want, why string) {
	t.Helper()
	want, why = p.direct()
	if !p.ok(want) {
		skipOrFail656(t, "%s: the kernel does not answer it with a working feature (%s, %q): nothing to keep", p.name, want, why)
	}
	return want, why
}

func requireRing1200(t *testing.T) {
	t.Helper()
	if r, err := NewRing(8, 0, 0); err != nil {
		skipOrFail656(t, "io_uring unavailable: %v", err)
	} else {
		_ = r.Close()
	}
}

// TestProbeSetupFailureIsNotCached1200: a probe whose ring or listener could
// not be set up (a process out of file descriptors at its first New: EMFILE)
// said "unsupported" and the answer was cached for the process, so the engine
// ran without the feature until the process ended, with no line in the log
// that said why. A failure to set up says nothing about the kernel: the first
// call answers "unsupported" for that engine, and the second probes for real
// and gives the kernel's answer; once it has, the answer is kept.
func TestProbeSetupFailureIsNotCached1200(t *testing.T) {
	requireRing1200(t)
	for _, p := range probes1200() {
		for _, inj := range []string{"NewRing", "Listen"} {
			if inj == "Listen" && !p.listens {
				continue
			}
			t.Run(p.name+"/"+inj, func(t *testing.T) {
				resetProbeCaches1200(t)
				want, wantWhy := wantAnswer1200(t, p)
				hits := 0
				var restore func()
				if inj == "NewRing" {
					restore = swap1200(t, &probeNewRing, func(uint32, uint32, uint32) (*Ring, error) {
						hits++
						return nil, fmt.Errorf("io_uring_setup: %w", unix.EMFILE)
					})
				} else {
					restore = swap1200(t, &probeListen, func() (net.Listener, error) {
						hits++
						return nil, emfile1200("listen")
					})
				}
				got, why := p.cached()
				if hits == 0 {
					t.Fatalf("%s: the %s seam was never reached: the test did not inject what it set out to", p.name, inj)
				}
				if p.ok(got) || why == "" {
					t.Fatalf("%s: with %s failing the probe answered %s (%q), want a negative with its reason", p.name, inj, got, why)
				}
				// The line that says the probe could not run: once, however often
				// New asks.
				log, lines := logCapture1200()
				logProbesNotRun(log)
				logProbesNotRun(log)
				if n := lines.count(p.logName); n != 1 {
					t.Errorf("%s: %d line(s) say the probe could not run after two asks, want exactly 1: %q", p.name, n, lines.buf.String())
				}
				restore()
				injected := hits

				got, why = p.cached()
				if got != want {
					t.Errorf("%s: the second call, with %s working again, answered %s (%q), want the kernel's %s (%q): "+
						"a failure to set up the probe was kept as the answer for the process", p.name, inj, got, why, want, wantWhy)
				}
				if hits != injected {
					t.Errorf("%s: the seam was reached again after it was restored", p.name)
				}
				if got != want {
					return
				}

				// The kernel's answer, once there, is kept: a third call does not
				// make a ring (the counting seam sees every ring a probe makes).
				rings := 0
				swap1200(t, &probeNewRing, func(e, f, s uint32) (*Ring, error) { rings++; return NewRing(e, f, s) })
				if got, _ = p.cached(); got != want || rings != 0 {
					t.Errorf("%s: the third call answered %s and made %d ring(s), want %s and none: the answer is kept", p.name, got, rings, want)
				}
			})
		}
	}
}

// wait1200 makes a probe's wait number n (1-based, counted over the whole run
// since install) fail with err, without entering the kernel; every other wait
// is the real one. calls counts the waits made.
type wait1200 struct {
	n, calls int
	err      error
}

func (w *wait1200) install(t *testing.T) (restore func()) {
	t.Helper()
	return swap1200(t, &probeRingWait, func(r *Ring, d time.Duration) error {
		w.calls++
		if w.calls == w.n {
			return w.err
		}
		return r.SubmitAndWaitTimeout(d)
	})
}

// TestProbeWaitErrorIsNotCached1200: a wait that failed with an errno other
// than ETIME and EINTR (EBUSY, EAGAIN, ENOMEM from io_uring_enter) was taken
// for the kernel's answer. Only the kernel refusing the call (EINVAL, as a
// kernel without EXT_ARG does, EOPNOTSUPP, ENOSYS) is one. SEND_ZC's second
// wait, for the notification, is covered too: a missing notification is the
// kernel's answer (SendZCBroken), a failed wait is not.
func TestProbeWaitErrorIsNotCached1200(t *testing.T) {
	requireRing1200(t)
	for _, p := range probes1200() {
		if !p.waits {
			continue
		}
		for n := 1; n <= p.waitCalls; n++ {
			t.Run(fmt.Sprintf("%s/wait%d", p.name, n), func(t *testing.T) {
				resetProbeCaches1200(t)
				want, wantWhy := wantAnswer1200(t, p)

				// A transient errno: the first call is negative, the second the kernel's.
				w := &wait1200{n: n, err: fmt.Errorf("io_uring_enter submit+wait timeout: %w", unix.EBUSY)}
				restore := w.install(t)
				if got, why := p.cached(); p.ok(got) || w.calls < n {
					t.Fatalf("%s: wait %d failing with EBUSY: the probe answered %s (%q) after %d wait(s), want a negative after at least %d",
						p.name, n, got, why, w.calls, n)
				}
				restore()
				if got, why := p.cached(); got != want {
					t.Errorf("%s: wait %d failed with EBUSY and the next call answered %s (%q), want the kernel's %s (%q): "+
						"a wait error was kept as the answer for the process", p.name, n, got, why, want, wantWhy)
				}

				// The kernel refusing the call is an answer, and is kept: this
				// guard passes with and without the fix, by design.
				resetProbeCaches1200(t)
				w = &wait1200{n: n, err: fmt.Errorf("io_uring_enter submit+wait timeout: %w", unix.EINVAL)}
				restore = w.install(t)
				first, why := p.cached()
				if p.ok(first) || !strings.Contains(why, "EINVAL") && !strings.Contains(why, "invalid argument") {
					t.Fatalf("%s: wait %d failing with EINVAL: the probe answered %s (%q), want a negative that names the error", p.name, n, first, why)
				}
				restore()
				if got, _ := p.cached(); got != first {
					t.Errorf("%s: the kernel refused the wait (EINVAL) and the next call answered %s, want the kept %s: a refusal is an answer", p.name, got, first)
				}
			})
		}
	}
}

// lines1200 is what a logger captured, one JSON object per line.
type lines1200 struct{ buf *bytes.Buffer }

func logCapture1200() (*slog.Logger, lines1200) {
	buf := new(bytes.Buffer)
	return slog.New(slog.NewJSONHandler(buf, nil)), lines1200{buf}
}

// count is how many Warn lines about a probe that could not run name the probe.
func (l lines1200) count(probe string) int {
	n := 0
	for _, ln := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(ln), &rec) != nil {
			continue
		}
		if rec["level"] == "WARN" && rec["probe"] == probe && strings.Contains(fmt.Sprint(rec["msg"]), "could not run") {
			n++
		}
	}
	return n
}

// TestProbeNoAnswerIsCappedAndNotifExpiryIsKept1200: a probe whose wait for its
// first completion used up two budgets in a row (the kernel took the SQE and
// posted nothing) is not kept at the first such run, which would be a stalled
// process taken for a kernel; the next New runs it again, and the
// probeMaxExpiredRuns-th is final, so that a kernel that never answers costs
// a bounded time and not a second per New. A missing SEND_ZC notification is
// the kernel's answer (SendZCBroken, AWS 6.17) and is kept at once.
func TestProbeNoAnswerIsCappedAndNotifExpiryIsKept1200(t *testing.T) {
	requireRing1200(t)
	swap1200(t, &probeFirstWaitBudget, 10*time.Millisecond)
	swap1200(t, &probeNotifWaitBudget, 10*time.Millisecond)
	if probeMaxExpiredRuns != 2 {
		t.Fatalf("probeMaxExpiredRuns is %d: this test is written for 2 runs, a first that expires and a second that answers or is final", probeMaxExpiredRuns)
	}
	for _, p := range probes1200() {
		if !p.waits {
			continue
		}
		t.Run(p.name, func(t *testing.T) {
			resetProbeCaches1200(t)
			want, wantWhy := wantAnswer1200(t, p)

			// The first run expires (its two waits), the second answers.
			calls, cuts := emptyWaits1195(t, cutExpire, 1, 2)
			if got, why := p.cached(); p.ok(got) || *cuts != 2 {
				t.Fatalf("%s: first run, both waits expired: answered %s (%q) after %d expired wait(s), want a negative after 2", p.name, got, why, *cuts)
			}
			log, lines := logCapture1200()
			logProbesNotRun(log)
			logProbesNotRun(log)
			if n := lines.count(p.logName); n != 1 {
				t.Errorf("%s: %d line(s) say the probe could not run, want exactly 1: %q", p.name, n, lines.buf.String())
			}
			if got, why := p.cached(); got != want {
				t.Errorf("%s: the first run expired and the next call answered %s (%q), want the kernel's %s (%q): "+
					"one run without an answer was kept for the process", p.name, got, why, want, wantWhy)
			}
			before := *calls
			if got, _ := p.cached(); got != want || *calls != before {
				t.Errorf("%s: the answer is kept: the third call answered %s after %d more wait(s), want %s and none", p.name, got, *calls-before, want)
			}

			// Both runs expire: the second is final.
			resetProbeCaches1200(t)
			calls, cuts = emptyWaits1195(t, cutExpire, 1, 2, 3, 4)
			first, _ := p.cached()
			second, why := p.cached()
			third, _ := p.cached()
			if p.ok(first) || p.ok(second) || p.ok(third) || *cuts != 4 || *calls != 4 || first != second || second != third {
				t.Errorf("%s: two runs without an answer: answers %s, %s, %s (%q) after %d wait(s), %d expired; want the same negative three times "+
					"after 4 waits: the second run is final, the third call does not run", p.name, first, second, third, why, *calls, *cuts)
			}

			if p.name != "send_zc" {
				return
			}
			// The notification wait expires: SendZCBroken, the kernel's answer, at once.
			resetProbeCaches1200(t)
			calls, cuts = emptyWaits1195(t, cutExpire, 2)
			first, _ = p.cached()
			second, _ = p.cached()
			if first != SendZCBroken.String() || second != first || *calls != 2 || *cuts != 1 {
				t.Errorf("send_zc, notification wait expired: answers %s then %s after %d wait(s), %d expired; want %s twice after 2, 1 expired: "+
					"a missing notification is an answer and is kept", first, second, *calls, *cuts, SendZCBroken)
			}
		})
	}
}

// TestProbeRefusalAndResourceClassification1200: which errors are the
// kernel's answer. A refusal of the call (EINVAL, EOPNOTSUPP, ENOSYS, however
// wrapped) is; every other errno, and an error with none, is the probe failing
// to run. For a completion's result the other way round: a resource error
// (-EMFILE, -ENFILE, -ENOMEM, -ENOBUFS, -EAGAIN, -EINTR, -EBUSY) is not the
// kernel's answer, any other result is.
func TestProbeRefusalAndResourceClassification1200(t *testing.T) {
	for _, tc := range []struct {
		err     error
		refused bool
	}{
		{unix.EINVAL, true},
		{fmt.Errorf("io_uring_enter submit+wait timeout: %w", unix.EINVAL), true},
		{fmt.Errorf("io_uring_register files: %w", unix.EOPNOTSUPP), true},
		{fmt.Errorf("io_uring_setup: %w", unix.ENOSYS), true},
		{fmt.Errorf("a: %w", fmt.Errorf("b: %w", unix.EINVAL)), true},
		{fmt.Errorf("io_uring_setup: %w", unix.EMFILE), false},
		{unix.ENFILE, false},
		{fmt.Errorf("io_uring_setup: %w", unix.ENOMEM), false},
		{fmt.Errorf("io_uring_enter submit+wait timeout: %w", unix.EBUSY), false},
		{unix.EAGAIN, false},
		{unix.EPERM, false},
		{unix.EACCES, false},
		{errors.New("ring failed"), false},
		{emfile1200("listen"), false},
	} {
		if got := probeRefused(tc.err); got != tc.refused {
			t.Errorf("probeRefused(%v) = %v, want %v", tc.err, got, tc.refused)
		}
		want := probeNotRun
		if tc.refused {
			want = probeAnswered
		}
		if got := probeRingOutcome(tc.err); got != want {
			t.Errorf("probeRingOutcome(%v) = %v, want %v", tc.err, got, want)
		}
	}
	for _, tc := range []struct {
		res  int32
		want probeOutcome
	}{
		{0, probeAnswered}, {7, probeAnswered}, {-int32(unix.EINVAL), probeAnswered}, {-int32(unix.EOPNOTSUPP), probeAnswered},
		{-int32(unix.ENOSYS), probeAnswered}, {-int32(unix.ECONNABORTED), probeAnswered}, {-int32(unix.EPERM), probeAnswered},
		{-int32(unix.EMFILE), probeNotRun}, {-int32(unix.ENFILE), probeNotRun}, {-int32(unix.ENOMEM), probeNotRun},
		{-int32(unix.ENOBUFS), probeNotRun}, {-int32(unix.EAGAIN), probeNotRun}, {-int32(unix.EINTR), probeNotRun},
		{-int32(unix.EBUSY), probeNotRun},
	} {
		if got := probeResOutcome(tc.res); got != tc.want {
			t.Errorf("probeResOutcome(%d) = %v, want %v", tc.res, got, tc.want)
		}
	}
	// The completions of the three probes, read by the functions the probes call.
	for _, res := range []int32{-int32(unix.EMFILE), -int32(unix.ENOMEM), -int32(unix.ENOBUFS)} {
		if r, why, out := fixedFilesCQE(res); r || out != probeNotRun || why == "" {
			t.Errorf("fixedFilesCQE(%d) = %v, %q, %v, want a negative with its reason, not run", res, r, why, out)
		}
		if r, why, out := multishotAcceptCQE(res, 0); r || out != probeNotRun || why == "" {
			t.Errorf("multishotAcceptCQE(%d, 0) = %v, %q, %v, want a negative with its reason, not run", res, r, why, out)
		}
		if r, why, out, done := sendZCFirstCQE(res, 0); r != SendZCUnsupported || out != probeNotRun || why == "" || !done {
			t.Errorf("sendZCFirstCQE(%d, 0) = %v, %q, %v, %v, want unsupported with its reason, not run, done", res, r, why, out, done)
		}
	}
	if r, _, out := fixedFilesCQE(-int32(unix.EINVAL)); r || out != probeAnswered {
		t.Errorf("fixedFilesCQE(-EINVAL) = %v, %v, want the kernel's rejection", r, out)
	}
	if r, _, out := fixedFilesCQE(0); !r || out != probeAnswered {
		t.Errorf("fixedFilesCQE(0) = %v, %v, want a working feature", r, out)
	}
	for _, tc := range []struct {
		res   int32
		flags uint32
		ok    bool
	}{{5, cqeFMore, true}, {5, 0, false}, {-int32(unix.EINVAL), 0, false}, {-int32(unix.ECONNABORTED), cqeFMore, false}} {
		if r, _, out := multishotAcceptCQE(tc.res, tc.flags); r != tc.ok || out != probeAnswered {
			t.Errorf("multishotAcceptCQE(%d, %#x) = %v, %v, want %v, the kernel's answer", tc.res, tc.flags, r, out, tc.ok)
		}
	}
	for _, tc := range []struct {
		res   int32
		flags uint32
		want  SendZCProbeResult
		done  bool
	}{
		{-int32(unix.EINVAL), 0, SendZCUnsupported, true},
		{8, 0, SendZCNoNotification, true},
		{8, 0x02, 0, false},
	} {
		if r, _, out, done := sendZCFirstCQE(tc.res, tc.flags); done != tc.done || out != probeAnswered || done && r != tc.want {
			t.Errorf("sendZCFirstCQE(%d, %#x) = %v, %v, %v, want %v answered, done %v", tc.res, tc.flags, r, out, done, tc.want, tc.done)
		}
	}
	// A dial that timed out costs a budget each time: expired, and capped.
	// Anything else that fails a dial returns at once.
	for _, tc := range []struct {
		err  error
		want probeOutcome
	}{
		{&net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, probeExpired},
		{fmt.Errorf("probe: %w", &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}), probeExpired},
		{emfile1200("dial"), probeNotRun},
		{&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", unix.ECONNREFUSED)}, probeNotRun},
		{errors.New("dial failed"), probeNotRun},
	} {
		if got := probeDialOutcome(tc.err); got != tc.want {
			t.Errorf("probeDialOutcome(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
	var unset probeOutcome
	if unset == probeAnswered {
		t.Errorf("the zero value of probeOutcome reads as an answer: an outcome that was never set would be cached")
	}
}

// TestNewSaysOnceThatAProbeCouldNotRun1200: through New. With the ring of
// every probe failing (EMFILE), the first engine is built without the probed
// features and the log says, once per probe, that the probe could not run;
// the second engine, with the kernel working again, probes for real, and
// neither says it again.
func TestNewSaysOnceThatAProbeCouldNotRun1200(t *testing.T) {
	requireRing1200(t)
	resetProbeCaches1200(t)
	log, lines := logCapture1200()
	cfg := resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, Logger: log}

	restore := swap1200(t, &probeNewRing, func(uint32, uint32, uint32) (*Ring, error) {
		return nil, fmt.Errorf("io_uring_setup: %w", unix.EMFILE)
	})
	if _, err := New(cfg, transplantTestHandler{}); err != nil {
		skipOrFail656(t, "iouring engine unavailable: %v", err)
	}
	if _, err := New(cfg, transplantTestHandler{}); err != nil {
		t.Fatalf("second New with the probes failing: %v", err)
	}
	// The probes New runs are the ones the host's profile has the feature for
	// (a run capped to a lower tier, CELERIS_MAX_IOURING_TIER, runs fewer or
	// none); a probe that did not run cannot say it could not.
	prof := probe.Probe()
	byProbe := map[string]bool{"SEND_ZC": prof.SendZC, "fixed files": prof.FixedFiles,
		"provided buffers": prof.ProvidedBuffers, "multishot accept": prof.MultishotAccept}
	total, ran := 0, 0
	for name, ranHere := range byProbe {
		n := lines.count(name)
		if n > 1 || n == 1 && !ranHere {
			t.Errorf("probe %q (run by New: %v): %d lines say it could not run after two engines, want 1 if it ran and none if not: %q",
				name, ranHere, n, lines.buf.String())
		}
		total += n
		if ranHere {
			ran++
		}
	}
	if ran > 0 && total == 0 {
		t.Errorf("no \"could not run\" line after a New with every ring failing, with %d probe(s) run: %q", ran, lines.buf.String())
	}

	restore()
	before := lines.buf.Len()
	if _, err := New(cfg, transplantTestHandler{}); err != nil {
		t.Fatalf("third New: %v", err)
	}
	if strings.Contains(lines.buf.String()[before:], "could not run") {
		t.Errorf("a probe that ran fine said it could not: %q", lines.buf.String()[before:])
	}
}

// TestAsyncCancelProbeSecondWaitIsPositive1200: the cancel probe's second wait
// is given the time that is left, and that time was read after the check that
// there was some: a first wait that came back a few nanoseconds before the
// deadline could be followed by one with a zero or negative timespec (EINVAL, or
// an instant return, and the probe called that no answer). The first wait of
// each run here comes back empty a little before its time is up, a little
// earlier each run (0 to 400 ns), so that one of them lands between the
// check and the read; no wait may be asked for less than a nanosecond. The
// window is nanoseconds wide: this guard is likely, not certain, to fail
// without the fix.
func TestAsyncCancelProbeSecondWaitIsPositive1200(t *testing.T) {
	requireRing1200(t)
	swap1200(t, &asyncCancelProbeSubmit, func(*Ring) (int, error) { return 0, nil })
	swap1200(t, &asyncCancelProbeTimeout, 200*time.Microsecond)
	var waits int
	var lead time.Duration
	var asked []time.Duration
	swap1200(t, &asyncCancelProbeWait, func(r *Ring, d time.Duration) error {
		waits++
		asked = append(asked, d)
		if waits == 1 {
			for end := time.Now().Add(d - lead); time.Now().Before(end); {
			}
			return nil
		}
		return r.SubmitAndWaitTimeout(d)
	})
	for lead = 0; lead < 400*time.Nanosecond; lead += 4 * time.Nanosecond {
		waits = 0
		probeAsyncCancel(cancelAll)
	}
	for _, d := range asked {
		if d <= 0 {
			t.Fatalf("a wait was asked for %v: the time left after the check must be positive", d)
		}
	}
}

// TestProbeSustainedSetupFailureIsNeverKept1200: a setup failure that lasts is
// not counted toward the cap of the expired runs: five calls in a row with the
// ring failing (EMFILE, a process that stays out of descriptors) all give the
// negative, and the call after the failure ends gives the kernel's answer. A
// "N failed runs, then final" policy fails here, and it is the one that makes
// a sustained EMFILE permanent.
func TestProbeSustainedSetupFailureIsNeverKept1200(t *testing.T) {
	requireRing1200(t)
	for _, p := range probes1200() {
		t.Run(p.name, func(t *testing.T) {
			resetProbeCaches1200(t)
			want, wantWhy := wantAnswer1200(t, p)
			restore := swap1200(t, &probeNewRing, func(uint32, uint32, uint32) (*Ring, error) {
				return nil, fmt.Errorf("io_uring_setup: %w", unix.EMFILE)
			})
			for i := 1; i <= 5; i++ {
				if got, why := p.cached(); p.ok(got) {
					t.Fatalf("%s: call %d with the ring failing answered %s (%q), want a negative", p.name, i, got, why)
				}
			}
			restore()
			if got, why := p.cached(); got != want {
				t.Errorf("%s: after 5 failed calls the next answered %s (%q), want the kernel's %s (%q): "+
					"a failure to set up was kept after a few runs", p.name, got, why, want, wantWhy)
			}
		})
	}
}

// TestNewDoesNotWarnWhenTheProbesRan1200: the line that says a probe could not
// run is for a probe that could not, not for one that ran: a New from probes
// that have never run, on a host where they work, logs none.
func TestNewDoesNotWarnWhenTheProbesRan1200(t *testing.T) {
	requireRing1200(t)
	resetProbeCaches1200(t)
	log, lines := logCapture1200()
	cfg := resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, Logger: log}
	for i := 0; i < 2; i++ {
		if _, err := New(cfg, transplantTestHandler{}); err != nil {
			skipOrFail656(t, "iouring engine unavailable: %v", err)
		}
	}
	if strings.Contains(lines.buf.String(), "could not run") {
		t.Errorf("a New whose probes ran said one could not: %q", lines.buf.String())
	}
}

const emfileChild1200 = "CELERIS_TEST_EMFILE_CHILD_1200"

// TestMultishotAcceptEMFILECompletion1200: a real -EMFILE completion. With the
// descriptor table full except for the slot the probe's own dial takes, the
// kernel completes the multishot accept with cqe.res=-24 (it has no descriptor
// for the connection): that is not an answer about multishot accept, and the
// probe must say it did not run and be run again. The only probe that can get
// this from the kernel: ACCEPT_DIRECT fills slots of the registered table, not
// descriptors, and SEND_ZC allocates none; theirs stay checked against
// synthetic completions. It runs in a child of the test binary, so that the
// lowered RLIMIT_NOFILE reaches no other test.
func TestMultishotAcceptEMFILECompletion1200(t *testing.T) {
	if os.Getenv(emfileChild1200) != "1" {
		requireRing1200(t)
		cmd := exec.Command(os.Args[0], "-test.run=^TestMultishotAcceptEMFILECompletion1200$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), emfileChild1200+"=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the child failed: %v\n%s", err, out)
		}
		if !bytes.Contains(out, []byte("--- PASS: TestMultishotAcceptEMFILECompletion1200")) {
			t.Fatalf("the child did not pass the test (it skipped?):\n%s", out)
		}
		return
	}
	requireRing1200(t)
	p := probes1200()[3]
	want, _ := wantAnswer1200(t, p) // before the limit is lowered

	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatalf("getrlimit: %v", err)
	}
	saved := lim
	lim.Cur = min(512, lim.Max)
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatalf("cannot lower RLIMIT_NOFILE: %v", err)
	}
	t.Cleanup(func() { _ = unix.Setrlimit(unix.RLIMIT_NOFILE, &saved) })

	resetProbeCaches1200(t)
	var dups []int
	restore := swap1200(t, &probeListen, func() (net.Listener, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		// Fill the table, then give one slot back: the probe's dial takes it,
		// and the accept of that connection finds none.
		for {
			fd, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if err != nil {
				break
			}
			dups = append(dups, fd)
		}
		if len(dups) > 0 {
			_ = unix.Close(dups[len(dups)-1])
			dups = dups[:len(dups)-1]
		}
		return ln, nil
	})
	got, why := p.cached()
	restore()
	for _, fd := range dups {
		_ = unix.Close(fd)
	}
	if p.ok(got) || !strings.Contains(why, "out of a resource") || !strings.Contains(why, "cqe.res=-24") {
		t.Fatalf("with the descriptor table full the probe answered %s (%q), want a negative from a -EMFILE completion (out of a resource)", got, why)
	}
	if got, why := p.cached(); got != want {
		t.Errorf("after the table was emptied the next call answered %s (%q), want the kernel's %s: the -EMFILE completion was kept", got, why, want)
	}
}
