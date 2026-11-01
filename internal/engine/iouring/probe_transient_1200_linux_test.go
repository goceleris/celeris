//go:build linux

package iouring

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// resetProbeCaches1200 forgets what the four cached probes learned, so that a
// test sees them as a process that has never run one, and forgets again when
// the test ends so that nothing it injected is kept for a later test.
func resetProbeCaches1200(t *testing.T) {
	t.Helper()
	reset := func() {
		cachedSendZC = sync.Once{}
		cachedFixedFiles = sync.Once{}
		cachedPbufRing = sync.Once{}
		cachedMultiAccept = sync.Once{}
	}
	reset()
	t.Cleanup(reset)
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
		{"send_zc", zc(probeSendZCCached), zc(probeSendZC),
			func(res string) bool { return res == SendZCCopyFallback.String() || res == SendZCTrueZeroCopy.String() },
			true, true, 2},
		{"fixed_files", b(probeFixedFilesCached), b(probeFixedFiles), isTrue, true, true, 1},
		{"provided_buffers", b(probeProvidedBuffersCached), b(probeProvidedBuffers), isTrue, false, false, 0},
		{"multishot_accept", b(probeMultishotAcceptCached), b(probeMultishotAccept), isTrue, true, true, 1},
	}
}

// emfile1200 is what net.Listen returns in a process out of file descriptors.
func emfile1200(op string) error {
	return &net.OpError{Op: op, Net: "tcp", Err: os.NewSyscallError("socket", unix.EMFILE)}
}

// wantAnswer1200 is the answer of one real run of p, or skips the test for a
// kernel that does not answer with a working feature: there is then nothing
// the second call could show.
func wantAnswer1200(t *testing.T, p probe1200) (want, why string) {
	t.Helper()
	want, why = p.direct()
	if !p.ok(want) {
		t.Logf("%s: the kernel does not answer it with a working feature (%s, %q): nothing to keep", p.name, want, why)
		return "", ""
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
				if want == "" {
					return
				}
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
				if want == "" {
					return
				}

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
