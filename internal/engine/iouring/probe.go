//go:build linux

package iouring

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Probe results are cached process-wide: kernel capabilities don't change
// inside a running process, but iouring.New() can be called
// hundreds-to-thousands of times in benchmark harnesses (a cross-framework
// matrix can run thousands of cells in one process with -race), and
// re-running every probe
// (each opens a temp ring + a TCP listener + a dial) on every New() is
// pure overhead. Only an answer is cached, though, and what counts as one is
// probeOutcome's business (celeris#1200). The async-cancel-flags probe is
// cached only once the kernel has answered it (probeAsyncCancelFlagsCached).
var (
	cachedSendZC      = probeCache[SendZCProbeResult]{name: "SEND_ZC"}
	cachedFixedFiles  = probeCache[bool]{name: "fixed files"}
	cachedPbufRing    = probeCache[bool]{name: "provided buffers"}
	cachedMultiAccept = probeCache[bool]{name: "multishot accept"}
	// The async-cancel-flags probe's answer, once the kernel has given one:
	// asyncCancelKnown says whether cachedAsyncCancelRes/Reas hold it. All
	// three are guarded by asyncCancelMu.
	asyncCancelMu         sync.Mutex
	asyncCancelKnown      bool
	cachedAsyncCancelRes  asyncCancelProbe
	cachedAsyncCancelReas string
)

// probeOutcome says what one run of a startup probe learned about the kernel.
// The zero value is probeNotRun, so an outcome that was never set reads as
// not cached, never as an answer (as asyncCancelNoAnswer).
type probeOutcome uint8

const (
	// probeNotRun: the probe failed before the kernel answered it, for a
	// reason that says nothing about the kernel: it could not set up its ring
	// or its loopback sockets (EMFILE, ENFILE, ENOMEM), a call on the ring
	// failed with an errno that is not a refusal (EBUSY, EAGAIN), or the
	// completion it got was a resource error (-EMFILE, -ENOBUFS). Never
	// cached: the caller gets this run's negative for this engine and the
	// probe runs again at the next New. It costs microseconds (a failed
	// syscall), so the number of re-runs is not capped.
	probeNotRun probeOutcome = iota
	// probeExpired: the probe ran and the wait for its first completion used
	// up the whole budget, twice (probeRetryNoAnswer). The kernel took the
	// SQE and posted nothing, which more likely means a stalled process than
	// a kernel that never answers. Not cached for the first
	// probeMaxExpiredRuns-1 runs, final after that: each run costs two
	// budgets (1 s), so this is the one outcome whose re-runs are capped.
	probeExpired
	// probeAnswered: the kernel answered, with a completion or with a
	// refusal of the call (probeRefused). Cached for the process.
	probeAnswered
)

// probeMaxExpiredRuns is how many runs of one probe that end in probeExpired
// the process makes before the negative is kept. Each run costs two waits of
// probeFirstWaitBudget, so a kernel that never answers costs a probe 1 s per
// run, in the first probeMaxExpiredRuns Ns: 2 s per probe, 6 s for the three
// that wait, against 3 s when the first run's negative was final (celeris#1198).
const probeMaxExpiredRuns = 2

// probeCache holds a startup probe's answer for the process, and what is
// needed to say that it could not run. A mutex, not a sync.Once: a run that
// is not an answer must leave the next call free to run again. The lock is
// held for the run, so concurrent Ns wait for one probe, as they did on the
// Once.
type probeCache[R any] struct {
	name string
	mu   sync.Mutex
	// known says res and why are the answer (or the final negative): later
	// calls return them without a run.
	known bool
	res   R
	why   string
	// expired counts the runs that ended in probeExpired.
	expired int
	// failedWhy is the reason of the first run that was not an answer, until
	// it has been logged (logPending); logged says it was.
	failedWhy string
	logged    bool
}

// get returns the cached answer, or runs the probe and caches its outcome as
// the doc of probeOutcome says. A run that is not an answer returns its
// negative for this call.
func (c *probeCache[R]) get(run func() (R, string, probeOutcome)) (R, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.known {
		return c.res, c.why
	}
	res, why, out := run()
	switch out {
	case probeAnswered:
		c.res, c.why, c.known = res, why, true
	case probeExpired:
		c.expired++
		if c.expired >= probeMaxExpiredRuns {
			c.res, c.why, c.known = res, why, true
		}
		fallthrough
	default:
		if !c.logged && c.failedWhy == "" {
			c.failedWhy = why
		}
	}
	return res, why
}

// logPending logs, once per probe per process, that a run of the probe was not
// an answer: the engine built at that call has the feature off, and the cause
// is not the kernel's. Called by New with the engine's logger after the
// probes; the lock makes a concurrent New log it only once.
func (c *probeCache[R]) logPending(l *slog.Logger) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.logged || c.failedWhy == "" {
		return
	}
	c.logged = true
	l.Warn("io_uring startup probe could not run, so the feature is off for this engine; "+
		"the probe runs again when the next engine is built (a probe whose wait gets no answer in two runs is kept as unsupported)",
		"probe", c.name, "reason", c.failedWhy)
}

// logProbesNotRun is logPending for the four cached probes.
func logProbesNotRun(l *slog.Logger) {
	cachedSendZC.logPending(l)
	cachedFixedFiles.logPending(l)
	cachedPbufRing.logPending(l)
	cachedMultiAccept.logPending(l)
}

// probeRefused says err is the kernel refusing a call on the probe's ring:
// EINVAL (as a kernel without IORING_ENTER_EXT_ARG answers the wait),
// EOPNOTSUPP or ENOSYS. Only these three are the kernel's answer about its
// features. Every other errno on a ring call, and an error with no errno, is
// the probe failing to run: out of descriptors or memory (EMFILE, ENFILE,
// ENOMEM, also from the mmaps of the ring), busy (EBUSY, EAGAIN). EPERM and
// EACCES (io_uring_disabled, seccomp) are left out on purpose: they are
// refusals of the process, not of the kernel's feature set, a probe cannot
// tell a policy that lifts from one that does not, and re-running costs one
// failed syscall.
func probeRefused(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS)
}

// probeRingOutcome is the outcome of a failed call on the probe's ring.
func probeRingOutcome(err error) probeOutcome {
	if probeRefused(err) {
		return probeAnswered
	}
	return probeNotRun
}

// probeResOutcome is the outcome of a probe's first completion with result
// res: a result that reports the kernel out of a resource says nothing about
// the feature (an accept whose descriptor table is full completes with
// -EMFILE, a send that cannot allocate with -ENOBUFS or -ENOMEM), and any
// other result is the kernel's answer. Not an allowlist of refusals, as
// probeRefused: a completion is the kernel having run the op.
func probeResOutcome(res int32) probeOutcome {
	if res >= 0 {
		return probeAnswered
	}
	switch unix.Errno(-res) {
	case unix.EMFILE, unix.ENFILE, unix.ENOMEM, unix.ENOBUFS, unix.EAGAIN, unix.EINTR, unix.EBUSY:
		return probeNotRun
	}
	return probeAnswered
}

// probeSendZCCached returns the cached SendZC probe result, running the
// probe until the kernel has answered it.
func probeSendZCCached() (SendZCProbeResult, string) { return cachedSendZC.get(probeSendZCRun) }

// probeFixedFilesCached returns the cached FixedFiles probe result.
func probeFixedFilesCached() (bool, string) { return cachedFixedFiles.get(probeFixedFilesRun) }

// probeProvidedBuffersCached returns the cached ProvidedBuffers probe result.
func probeProvidedBuffersCached() (bool, string) {
	return cachedPbufRing.get(probeProvidedBuffersRun)
}

// probeMultishotAcceptCached returns the cached MultishotAccept probe result.
func probeMultishotAcceptCached() (bool, string) {
	return cachedMultiAccept.get(probeMultishotAcceptRun)
}

// probeAsyncCancelFlagsCached returns the async-cancel-flags probe's answer.
// Only the kernel's answer, accepted or rejected, is cached for the process
// (celeris#681 N1). A probe that got no answer says nothing lasting about the
// kernel: its private ring can fail to set up with EMFILE or ENOMEM when the
// adaptive engine builds its io_uring engine lazily under load, and its wait
// can come back without the cancel's completion. Cached, one such failure kept
// the hand-off's reap off for the life of the process. So a probe with no
// answer is not cached, and neither is an answer the probe does not recognise:
// the next New probes again, and logs again. Calls are serialised, so
// concurrent News run one probe at a time and never overwrite an answer.
func probeAsyncCancelFlagsCached() (asyncCancelProbe, string) {
	asyncCancelMu.Lock()
	defer asyncCancelMu.Unlock()
	if asyncCancelKnown {
		return cachedAsyncCancelRes, cachedAsyncCancelReas
	}
	res, reason := runAsyncCancelProbe()
	if res == asyncCancelAccepted || res == asyncCancelRejected {
		cachedAsyncCancelRes, cachedAsyncCancelReas, asyncCancelKnown = res, reason, true
	}
	return res, reason
}

// runAsyncCancelProbe is the probe probeAsyncCancelFlagsCached runs. A
// variable so a test can give each class of answer to the cache and to New.
//
// SEAM CONSTRAINT (celeris#681 N-a), shared with asyncCancelProbeSubmit,
// asyncCancelProbeWait and asyncCancelProbeTimeout: these four are READ
// inside probeAsyncCancelFlagsCached's asyncCancelMu critical section and
// WRITTEN by tests without it, so a test may only replace one while no
// other goroutine can reach a probe — that is, from its own test goroutine,
// never from a test that has called t.Parallel(), and never while an engine
// it does not own is being built. Nothing in this package calls t.Parallel()
// (git grep -n 't\.Parallel()' -- 'internal/engine/iouring/*_test.go' is empty, while
// the same grep finds internal/engine/provider_test.go), so the constraint holds
// today; it is documented rather than enforced because guarding the
// seams would mean a mutex-taking setter and a save/restore helper for each
// of the four, which is more machinery than a constraint one grep checks.
//
// The one exception is ForceAsyncCancelProbe (async_cancel_seam.go), for a
// test of another package: it replaces this variable and forgets the cached
// answer under asyncCancelMu, so it is not a race with a New on another
// goroutine; the process-wide answer it sets is the caller's to restore.
var runAsyncCancelProbe = probeAsyncCancelFlags

// probeRingWait is the wait of the SEND_ZC, fixed-files and multishot-accept
// probes: it submits what is pending and waits up to d for a completion. A
// variable so a test can answer it the way a wait a signal cut short does, with
// nil and no completion (celeris#1195). Replaced only from a test goroutine
// that no other probe can run against, as asyncCancelProbeWait is.
var probeRingWait = func(r *Ring, d time.Duration) error { return r.SubmitAndWaitTimeout(d) }

// probeNewRing makes the private ring of the SEND_ZC, fixed-files,
// provided-buffers and multishot-accept probes, and probeListen the loopback
// listener of the first, second and fourth. Variables so a test can make the
// setup fail the way a process out of file descriptors does (celeris#1200).
// They carry probeRingWait's constraint: replaced only from a test goroutine
// that no other probe can run against.
var (
	probeNewRing = NewRing
	probeListen  = func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
)

// How long the probes wait for their completion: probeFirstWaitBudget for the
// first one (the SEND_ZC send's, the multishot accepts'), which is normally
// there when the wait starts, and probeNotifWaitBudget for SEND_ZC's
// notification. Variables so a test can shorten them (celeris#1195). They carry
// probeRingWait's constraint: only from a test goroutine that no other probe can
// run against.
var (
	probeFirstWaitBudget = 500 * time.Millisecond
	probeNotifWaitBudget = 2 * time.Second
)

// probeWaitCQE waits on r until its CQ ring has a completion, or until budget
// has gone by, and returns the CQ ring's head and tail (equal when budget ran
// out with nothing to read) and how many waits came back with the ring empty.
// An error from the wait other than the two SubmitAndWaitTimeout turns into
// nil (ETIME, EINTR) is returned at once.
//
// One wait is not the answer. SubmitAndWaitTimeout returns nil when a signal
// ends the wait (EINTR; io_uring_enter reports it only when the call submitted
// nothing, so a call that also submitted returns the submit count) and when its
// timeout expires, and a wait with min_complete 1 can also be woken by a timer
// before the completion it stands for is posted (cq_timeouts; celeris#1183).
// Each of those leaves the CQ ring empty and says nothing about the kernel, and
// the probes' results are cached for the process: read as final, an empty ring
// switched SEND_ZC, fixed files or multishot accept off until the process
// ended (celeris#1195). So the wait is entered again, with the time that is
// left, each time it comes back with nothing to read. Each re-entry is a new
// io_uring_enter with GETEVENTS, which is what posts a completion that a
// DEFER_TASKRUN ring holds back; a look at the CQ ring alone would not.
//
// The budget is wall time, not a count of waits, and it bounds the call: every
// wait is given only what is left of it, so a probe takes budget and a
// scheduling delay at most, as one wait did. Startup only: the run loop has its
// own (Worker.run).
func probeWaitCQE(r *Ring, budget time.Duration) (head, tail uint32, empty int, err error) {
	deadline := time.Now().Add(budget)
	for left := budget; ; {
		if err = probeRingWait(r, left); err != nil {
			return 0, 0, empty, err
		}
		if head, tail = r.BeginCQ(); head != tail {
			return head, tail, empty, nil
		}
		empty++
		// left stays positive: a zero or negative timespec would make the
		// next wait return at once (or EINVAL), and the loop spin on it.
		if left = time.Until(deadline); left <= 0 {
			return head, tail, empty, nil
		}
	}
}

// probeRetryNoAnswer runs a probe, and runs it once more, with new rings and
// sockets, when the first run says it got no answer: the wait for its first
// completion used up its whole budget with nothing in the CQ ring (see
// probeWaitCQE). The result of a probe is cached for the process, and a
// completion that is normally there at submit and was not there for 500 ms is
// more likely a stalled process than a kernel that never posts it, so the
// second look costs one budget, and only when that happened. A second
// run with no answer is probeExpired: the negative for this engine, kept for
// the process after probeMaxExpiredRuns such runs. The wait for SEND_ZC's
// notification is not retried: the send completed with F_MORE and the
// notification did not come, which is how the AWS 6.17 kernel answers
// (SendZCBroken; commit 39a7ce1 recorded it on virtual and metal alike, and the
// probe sends over loopback, so no NIC takes part), and a retry would add its
// 2 s to every startup on such a host.
func probeRetryNoAnswer[R any](once func() (R, string, probeOutcome)) (R, string, probeOutcome) {
	res, why, out := once()
	if out == probeExpired {
		res, why, out = once()
	}
	return res, why, out
}

// sendZCAnswered is parseSendZCResult's result as probeSendZCOnce returns it:
// an answer, not a missing one. (A notification that did not come is an
// answer too, SendZCBroken.)
func sendZCAnswered(res SendZCProbeResult, why string) (SendZCProbeResult, string, probeOutcome) {
	return res, why, probeAnswered
}

// SEND_ZC ioprio flags and notification result values.
const (
	sendZCReportUsage         = 1 << 3  // IORING_SEND_ZC_REPORT_USAGE: request ZC usage info in notification
	notifUsageZCCopied uint32 = 1 << 31 // IORING_NOTIF_USAGE_ZC_COPIED: data was copied, not zero-copied
)

// SendZCProbeResult describes the outcome of the SEND_ZC runtime probe.
type SendZCProbeResult int

const (
	// SendZCUnsupported means the kernel doesn't support the SEND_ZC opcode.
	SendZCUnsupported SendZCProbeResult = iota
	// SendZCBroken means the kernel accepts SEND_ZC but the notification CQE
	// never arrives (e.g., ENA driver DMA completion bug).
	SendZCBroken
	// SendZCNoNotification means the kernel accepted SEND_ZC but the initial CQE
	// was missing CQE_F_MORE, so no notification followed. Notification delivery
	// was unobserved, so the feature cannot be treated as functional.
	SendZCNoNotification
	// SendZCCopyFallback means SEND_ZC opcode and notification delivery are functional,
	// but the kernel copied data instead of using DMA zero-copy (expected on loopback
	// or NICs without scatter-gather DMA).
	SendZCCopyFallback
	// SendZCTrueZeroCopy means SEND_ZC uses real DMA zero-copy. The notification
	// arrives and reports actual zero-copy usage. This is the optimal case.
	SendZCTrueZeroCopy
)

func (r SendZCProbeResult) String() string {
	switch r {
	case SendZCUnsupported:
		return "unsupported"
	case SendZCBroken:
		return "broken (notification missing)"
	case SendZCNoNotification:
		return "no notification (CQE_F_MORE missing)"
	case SendZCCopyFallback:
		return "copy fallback"
	case SendZCTrueZeroCopy:
		return "true zero-copy"
	default:
		return "unknown"
	}
}

// probeSendZC tests SEND_ZC behavior using IORING_SEND_ZC_REPORT_USAGE.
// Returns a detailed result describing whether SEND_ZC is functional and
// whether true zero-copy is achieved.
//
// The probe sends over loopback (127.0.0.1) and never through a NIC, so a
// working kernel copies the data (no DMA) and the result is SendZCCopyFallback.
// On the AWS 6.17 kernel (commit 39a7ce1) the result is SendZCBroken because the
// notification CQE is never posted: that is the kernel's behaviour, not a
// driver's.
func probeSendZC() (SendZCProbeResult, string) {
	res, why, _ := probeSendZCRun()
	return res, why
}

// probeSendZCRun is probeSendZC with the outcome of the run.
func probeSendZCRun() (SendZCProbeResult, string, probeOutcome) {
	return probeRetryNoAnswer(probeSendZCOnce)
}

// probeSendZCOnce is one run of probeSendZC. The outcome is probeExpired when the wait for its
// first completion used up the whole budget with nothing in the CQ ring, probeNotRun when the run
// failed before the kernel answered (see probeOutcome), and probeAnswered otherwise.
func probeSendZCOnce() (SendZCProbeResult, string, probeOutcome) {
	ln, err := probeListen()
	if err != nil {
		return SendZCUnsupported, "net.Listen failed: " + err.Error(), probeNotRun
	}
	defer func() { _ = ln.Close() }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return SendZCUnsupported, "net.Dial failed: " + err.Error(), probeNotRun
	}
	defer func() { _ = conn.Close() }()

	accepted, err := ln.Accept()
	if err != nil {
		return SendZCUnsupported, "ln.Accept failed: " + err.Error(), probeNotRun
	}
	defer func() { _ = accepted.Close() }()

	rawConn, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		return SendZCUnsupported, "SyscallConn failed: " + err.Error(), probeNotRun
	}

	var fd int
	if err := rawConn.Control(func(f uintptr) { fd = int(f) }); err != nil {
		return SendZCUnsupported, "SyscallConn.Control failed: " + err.Error(), probeNotRun
	}
	if fd <= 0 {
		return SendZCUnsupported, fmt.Sprintf("invalid socket fd from Control: %d", fd), probeNotRun
	}
	ring, err := probeNewRing(4, 0, 0)
	if err != nil {
		return SendZCUnsupported, "NewRing failed: " + err.Error(), probeRingOutcome(err)
	}
	defer func() { _ = ring.Close() }()

	// Prepare SEND_ZC with REPORT_USAGE flag so the notification CQE tells
	// us whether true zero-copy or copy fallback was used.
	payload := []byte("probe-send-zc-test-payload")
	sqe := ring.GetSQE()
	if sqe == nil {
		return SendZCUnsupported, "GetSQE returned nil", probeNotRun
	}
	prepSendZC(sqe, fd, payload, false)
	// Set IORING_SEND_ZC_REPORT_USAGE in ioprio field (offset 2).
	sqeBytes := (*[sqeSize]byte)(sqe)
	*(*uint16)(unsafe.Pointer(&sqeBytes[2])) = sendZCReportUsage
	setSQEUserData(sqe, 42)

	// Submit and wait for the first CQE.
	cqHead, cqTail, empty, err := probeWaitCQE(ring, probeFirstWaitBudget)
	if err != nil {
		return SendZCUnsupported, "SubmitAndWaitTimeout (initial) failed: " + err.Error(), probeRingOutcome(err)
	}
	if cqHead == cqTail {
		return SendZCUnsupported, fmt.Sprintf("no initial CQE produced after submit (waited %v, %d empty return(s))", probeFirstWaitBudget, empty), probeExpired
	}
	entry := ring.cqeAt(cqHead)
	initialRes := entry.Res
	initialFlags := entry.Flags
	ring.EndCQ(cqHead + 1)

	if res, why, out, done := sendZCFirstCQE(initialRes, initialFlags); done {
		return res, why, out
	}

	// Wait for the notification CQE.
	cqHead, cqTail, _, err = probeWaitCQE(ring, probeNotifWaitBudget)
	if err != nil {
		// A failed wait is the kernel's answer only when it refused the call.
		res, why := parseSendZCResult(initialRes, initialFlags, false, err, 0, 0)
		return res, why, probeRingOutcome(err)
	}
	if cqHead == cqTail {
		return sendZCAnswered(parseSendZCResult(initialRes, initialFlags, false, nil, 0, 0))
	}

	entry = ring.cqeAt(cqHead)
	notifFlags := entry.Flags
	notifRes := entry.Res
	ring.EndCQ(cqHead + 1)

	// Clean up: read the sent data on the receiver side.
	buf := make([]byte, 64)
	_ = accepted.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, _ = accepted.Read(buf)

	return sendZCAnswered(parseSendZCResult(initialRes, initialFlags, true, nil, notifRes, notifFlags))
}

// sendZCFirstCQE reads the completion of the probe's send. done says the probe
// is over: the result is negative (the kernel's refusal, or a resource error
// that is not an answer, probeResOutcome) or the CQE has no F_MORE, so that no
// notification follows. Not done, the probe goes on to wait for the
// notification.
func sendZCFirstCQE(res int32, flags uint32) (SendZCProbeResult, string, probeOutcome, bool) {
	if out := probeResOutcome(res); out != probeAnswered {
		return SendZCUnsupported, fmt.Sprintf("the SEND_ZC send completed with cqe.res=%d (%s): out of a resource, not the kernel's answer",
			res, errnoName(-res)), out, true
	}
	if res < 0 || flags&0x02 == 0 {
		r, why := parseSendZCResult(res, flags, false, nil, 0, 0)
		return r, why, probeAnswered, true
	}
	return 0, "", probeAnswered, false
}

// parseSendZCResult evaluates the CQE outcomes from the SEND_ZC probe.
// Factored out of probeSendZC so the evaluation logic can be verified
// against synthetic CQEs across all outcomes (celeris#465).
func parseSendZCResult(initialRes int32, initialFlags uint32, notifArrived bool, waitErr error, notifRes int32, notifFlags uint32) (SendZCProbeResult, string) {
	if initialRes < 0 {
		return SendZCUnsupported, fmt.Sprintf("kernel rejected SEND_ZC opcode: cqe.res=%d (likely -ENOSYS=-38 or -EINVAL=-22)", initialRes)
	}
	if initialFlags&0x02 == 0 {
		return SendZCNoNotification, "first CQE missing CQE_F_MORE flag (no notification will follow)"
	}
	if waitErr != nil {
		return SendZCBroken, "notification CQE wait failed: " + waitErr.Error()
	}
	if !notifArrived {
		return SendZCBroken, "no notification CQE produced (waited 2s)"
	}
	if notifFlags&cqeFNotif == 0 {
		return SendZCBroken, fmt.Sprintf("second CQE missing CQE_F_NOTIF flag (flags=%#x)", notifFlags)
	}
	if uint32(notifRes)&notifUsageZCCopied != 0 {
		return SendZCCopyFallback, "REPORT_USAGE notification reports IORING_NOTIF_USAGE_ZC_COPIED (kernel did the copy)"
	}
	return SendZCTrueZeroCopy, ""
}

// resolveSendZCPolicy evaluates the SEND_ZC policy given the functional probe result
// and the CELERIS_IOURING_SEND_ZC environment setting.
//
// Values:
//   - "on", "1", "true": force enabled if functional probe passed.
//   - "off", "0", "false": force disabled.
//   - "auto", "" (default): enabled when the functional probe passed. The on/off A/B of
//     celeris#585 (2026-09-27) kept this default: on neither arch did SEND_ZC carry enough
//     of a benchmark cell's bytes to vote, and the default changes only on a measured loss.
//   - any other value: falls back to auto behavior, and returns recognized=false.
//
// When the functional probe failed, the value is not examined: the result is
// (false, true) whatever it is, so an unrecognized value is reported only when
// the probe passed.
func resolveSendZCPolicy(functional bool, envVal string) (enabled, recognized bool) {
	if !functional {
		return false, true
	}
	switch strings.ToLower(strings.TrimSpace(envVal)) {
	case "1", "on", "true":
		return true, true
	case "0", "off", "false":
		return false, true
	case "auto", "":
		return functional, true
	default:
		return functional, false
	}
}

// probeFixedFiles tests whether ACCEPT_DIRECT (fixed files) works end-to-end.
// Registering the file table is not enough: some kernels (observed on
// 6.6.10-cix, ARM64) accept IORING_REGISTER_FILES_SPARSE but then fail the
// multishot-accept-direct SQE with EINVAL at runtime. When that happens
// every worker pays the cold-fallback cost on its very first accept and,
// critically, the ring is left in a mixed state with RegisterFiles succeeded
// but fixed files effectively disabled — which compounds the per-op overhead
// on subsequent recv/send SQEs that would otherwise have been optimized.
//
// To avoid that, submit an actual MULTISHOT ACCEPT_DIRECT against a
// temporary listen socket. If the kernel returns EINVAL (-22), we know
// fixed files are non-functional on this host and surface it as a probe
// miss so the engine takes the plain-fd path from the start.
func probeFixedFiles() (bool, string) {
	ok, why, _ := probeFixedFilesRun()
	return ok, why
}

// probeFixedFilesRun is probeFixedFiles with the outcome of the run.
func probeFixedFilesRun() (bool, string, probeOutcome) {
	return probeRetryNoAnswer(probeFixedFilesOnce)
}

// probeFixedFilesOnce is one run of probeFixedFiles; the outcome is as probeSendZCOnce's.
func probeFixedFilesOnce() (bool, string, probeOutcome) {
	ring, err := probeNewRing(8, 0, 0)
	if err != nil {
		return false, "NewRing failed: " + err.Error(), probeRingOutcome(err)
	}
	defer func() { _ = ring.Close() }()

	if err := ring.RegisterFiles(16); err != nil {
		return false, "RegisterFiles failed: " + err.Error(), probeRingOutcome(err)
	}

	ln, err := probeListen()
	if err != nil {
		return false, "net.Listen failed: " + err.Error(), probeNotRun
	}
	defer func() { _ = ln.Close() }()

	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		return false, "SyscallConn failed: " + err.Error(), probeNotRun
	}
	var listenFD int
	_ = rc.Control(func(fd uintptr) { listenFD = int(fd) })
	if listenFD <= 0 {
		return false, fmt.Sprintf("listen FD=%d <= 0", listenFD), probeNotRun
	}

	sqe := ring.GetSQE()
	if sqe == nil {
		return false, "GetSQE returned nil", probeNotRun
	}
	prepMultishotAcceptDirect(sqe, listenFD)
	setSQEUserData(sqe, 0xF17EDF11E) // distinct tag for this probe
	if _, err := ring.Submit(); err != nil {
		return false, "Submit failed: " + err.Error(), probeRingOutcome(err)
	}

	// Trigger one accept so the kernel produces a CQE for the multishot SQE.
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.Dial("tcp", ln.Addr().String())
	if err != nil {
		return false, "probe dial failed: " + err.Error(), probeNotRun
	}
	defer func() { _ = conn.Close() }()

	head, tail, empty, err := probeWaitCQE(ring, probeFirstWaitBudget)
	if err != nil {
		return false, "SubmitAndWaitTimeout failed: " + err.Error(), probeRingOutcome(err)
	}
	if head == tail {
		return false, fmt.Sprintf("no CQE produced after multishot accept-direct + dial (waited %v, %d empty return(s))", probeFirstWaitBudget, empty), probeExpired
	}
	cqe := ring.cqeAt(head)
	res := cqe.Res
	ring.EndCQ(head + 1)
	// Res < 0 means the kernel registered files but would not complete this
	// accept-direct. Historically this reported -EINVAL everywhere and was
	// attributed to the kernel ("seen on 6.6.10-cix aarch64"); the real cause
	// was our own SQE passing SOCK_CLOEXEC alongside a fixed file slot, which
	// io_accept_prep rejects by design. That is fixed (celeris#541), so a
	// rejection here is now genuinely the kernel's.
	return fixedFilesCQE(res)
}

// fixedFilesCQE reads the completion of the fixed-files probe's accept-direct:
// a resource error is not an answer (probeResOutcome), another negative result
// is the kernel's rejection, a result of 0 or more is a working feature.
func fixedFilesCQE(res int32) (bool, string, probeOutcome) {
	if out := probeResOutcome(res); out != probeAnswered {
		return false, fmt.Sprintf("ACCEPT_DIRECT completed with cqe.res=%d (%s): out of a resource, not the kernel's answer",
			res, errnoName(-res)), out
	}
	if res < 0 {
		return false, fmt.Sprintf("ACCEPT_DIRECT rejected by kernel: cqe.res=%d (likely -EINVAL=-22)", res), probeAnswered
	}
	return true, "", probeAnswered
}

// probeProvidedBuffers tests whether IORING_REGISTER_PBUF_RING works on this
// kernel. Some patched/embedded kernels report 5.19+ but reject the register
// call (e.g. ENOSYS or EINVAL). Without a working pbuf ring, multishot recv
// cannot be used — the engine must fall back to single-shot per-connection
// recv buffers regardless of the kernel-version-based tier.
func probeProvidedBuffers() (bool, string) {
	ok, why, _ := probeProvidedBuffersRun()
	return ok, why
}

// probeProvidedBuffersRun is probeProvidedBuffers with the outcome of the run:
// a ring that cannot be set up or mapped (EMFILE, ENOMEM: memlock already
// used by the engines of this process) is probeNotRun, a registration the
// kernel refuses is probeAnswered.
func probeProvidedBuffersRun() (bool, string, probeOutcome) {
	ring, err := probeNewRing(8, 0, 0)
	if err != nil {
		return false, "NewRing failed: " + err.Error(), probeRingOutcome(err)
	}
	defer func() { _ = ring.Close() }()

	// Allocate a tiny pbuf ring (8 entries × 16 bytes = 128 bytes via mmap).
	// We don't actually use the buffers — just verify the kernel accepts
	// the registration syscall and the matching unregister.
	const probeCount = 8
	region, err := unix.Mmap(-1, 0, probeCount*bufRingEntrySize,
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return false, "mmap probe ring: " + err.Error(), probeNotRun
	}
	defer func() { _ = unix.Munmap(region) }()

	if regErr := ring.RegisterPbufRing(0xFFFF, probeCount, unsafe.Pointer(&region[0])); regErr != nil {
		return false, "RegisterPbufRing rejected: " + regErr.Error(), probeRingOutcome(regErr)
	}
	if unregErr := ring.UnregisterPbufRing(0xFFFF); unregErr != nil {
		return false, "UnregisterPbufRing failed (ring left dirty): " + unregErr.Error(), probeRingOutcome(unregErr)
	}
	return true, "", probeAnswered
}

// probeMultishotAccept tests whether IORING_OP_ACCEPT with the multishot flag
// actually re-arms after the first completion on this kernel. Vendor kernels
// have been seen to advertise the feature (kernel ≥5.19) but silently degrade:
// the first accept lands fine, but CQE_F_MORE is never set, leaving workers
// in a non-rearming state that looks like "accept stalled" under churn.
//
// We submit a non-direct multishot accept against a temp listen socket, dial
// once, and check that (a) Res ≥ 0 and (b) CQE_F_MORE is set. If F_MORE is
// missing on the first CQE the multishot path is broken — fall back to
// single-shot accept which the worker re-arms explicitly on every CQE.
func probeMultishotAccept() (bool, string) {
	ok, why, _ := probeMultishotAcceptRun()
	return ok, why
}

// probeMultishotAcceptRun is probeMultishotAccept with the outcome of the run.
func probeMultishotAcceptRun() (bool, string, probeOutcome) {
	return probeRetryNoAnswer(probeMultishotAcceptOnce)
}

// probeMultishotAcceptOnce is one run of probeMultishotAccept; the outcome is as probeSendZCOnce's.
func probeMultishotAcceptOnce() (bool, string, probeOutcome) {
	ring, err := probeNewRing(8, 0, 0)
	if err != nil {
		return false, "NewRing failed: " + err.Error(), probeRingOutcome(err)
	}
	defer func() { _ = ring.Close() }()

	ln, err := probeListen()
	if err != nil {
		return false, "net.Listen failed: " + err.Error(), probeNotRun
	}
	defer func() { _ = ln.Close() }()

	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		return false, "SyscallConn failed: " + err.Error(), probeNotRun
	}
	var listenFD int
	_ = rc.Control(func(fd uintptr) { listenFD = int(fd) })
	if listenFD <= 0 {
		return false, fmt.Sprintf("listen FD=%d <= 0", listenFD), probeNotRun
	}

	sqe := ring.GetSQE()
	if sqe == nil {
		return false, "GetSQE returned nil", probeNotRun
	}
	prepMultishotAccept(sqe, listenFD)
	setSQEUserData(sqe, 0xACCE9701B0)
	if _, subErr := ring.Submit(); subErr != nil {
		return false, "Submit failed: " + subErr.Error(), probeRingOutcome(subErr)
	}

	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.Dial("tcp", ln.Addr().String())
	if err != nil {
		return false, "probe dial failed: " + err.Error(), probeNotRun
	}
	defer func() { _ = conn.Close() }()

	head, tail, empty, waitErr := probeWaitCQE(ring, probeFirstWaitBudget)
	if waitErr != nil {
		return false, "SubmitAndWaitTimeout failed: " + waitErr.Error(), probeRingOutcome(waitErr)
	}
	if head == tail {
		return false, fmt.Sprintf("no CQE produced after multishot accept + dial (waited %v, %d empty return(s))", probeFirstWaitBudget, empty), probeExpired
	}
	cqe := ring.cqeAt(head)
	res := cqe.Res
	flags := cqe.Flags
	ring.EndCQ(head + 1)

	// Close the accepted FD so it doesn't leak — we have its fd in res.
	if res > 0 {
		_ = unix.Close(int(res))
	}
	return multishotAcceptCQE(res, flags)
}

// multishotAcceptCQE reads the first completion of the multishot-accept probe.
// A resource error is not an answer (an accept whose descriptor table is full
// completes with -EMFILE, without F_MORE), another negative result is the
// kernel's rejection, and without F_MORE the kernel will not re-arm.
func multishotAcceptCQE(res int32, flags uint32) (bool, string, probeOutcome) {
	if out := probeResOutcome(res); out != probeAnswered {
		return false, fmt.Sprintf("multishot accept completed with cqe.res=%d (%s): out of a resource, not the kernel's answer",
			res, errnoName(-res)), out
	}
	if res < 0 {
		return false, fmt.Sprintf("multishot accept rejected by kernel: cqe.res=%d", res), probeAnswered
	}
	if flags&cqeFMore == 0 {
		return false, "first multishot accept CQE missing CQE_F_MORE (kernel won't re-arm)", probeAnswered
	}
	return true, "", probeAnswered
}

// The async-cancel-flags probe's own user_data values: the op it tries to
// cancel (nothing in its private ring carries it) and the cancel itself.
const (
	asyncCancelProbeTarget uint64 = 0xCA_4CE1_7A26_E7
	asyncCancelProbeTag    uint64 = 0xCA_4CE1_7A6
)

// asyncCancelProbe is what probeAsyncCancelFlags learned. Only
// asyncCancelAccepted turns the hand-off's reap on; the others keep it off,
// and they are told apart because they mean different things (celeris#681
// R2, N2): a rejection is the kernel's answer, a probe that got no answer
// says nothing about the kernel, and an answer the probe does not recognise
// is one no kernel measured gives.
//
// The zero value is asyncCancelNoAnswer (celeris#681 N3), so an answer that
// was never set reads as no answer and keeps the reap off, never as
// accepted.
type asyncCancelProbe uint8

const (
	// asyncCancelNoAnswer: the probe failed before the kernel answered. Its
	// ring could not be set up, the submit or the wait failed, no completion
	// came, or the completion was not the probe's cancel. The zero value.
	asyncCancelNoAnswer asyncCancelProbe = iota
	// asyncCancelAccepted: the kernel completed the probe's cancel and
	// accepted its IORING_ASYNC_CANCEL flags.
	asyncCancelAccepted
	// asyncCancelRejected: the kernel completed the probe's cancel without
	// accepting the flags: -EINVAL, as every kernel before 5.19 answers.
	asyncCancelRejected
	// asyncCancelUnexpected: the kernel completed the probe's cancel with a
	// result the probe does not recognise (neither an acceptance nor
	// -EINVAL). No kernel measured answers so; it keeps the reap off and New
	// warns with the errno (celeris#681 N2).
	asyncCancelUnexpected
)

func (p asyncCancelProbe) String() string {
	switch p {
	case asyncCancelNoAnswer:
		return "no answer"
	case asyncCancelAccepted:
		return "accepted"
	case asyncCancelRejected:
		return "rejected"
	case asyncCancelUnexpected:
		return "unexpected"
	}
	return fmt.Sprintf("asyncCancelProbe(%d)", uint8(p))
}

// newAsyncCancelProbeRing makes the probe's private ring. A variable so a
// test can make the probe fail before the kernel answers.
var newAsyncCancelProbeRing = func() (*Ring, error) { return NewRing(8, 0, 0) }

// The probe's submit and its wait for the cancel's completion, and how long
// that wait is. Variables so a test can hold the cancel back and cut the wait
// short (celeris#681 N1). They carry runAsyncCancelProbe's SEAM CONSTRAINT:
// read under asyncCancelMu, replaced by a test that holds no lock, so only
// from a test goroutine that no other probe can run against (celeris#681
// N-a).
var (
	asyncCancelProbeSubmit  = func(r *Ring) (int, error) { return r.Submit() }
	asyncCancelProbeWait    = func(r *Ring, d time.Duration) error { return r.SubmitAndWaitTimeout(d) }
	asyncCancelProbeTimeout = 500 * time.Millisecond
)

// probeAsyncCancelFlags tests whether the kernel accepts the
// IORING_ASYNC_CANCEL_* flags, which exist from Linux 5.19. The io_uring→epoll
// hand-off's REAP (celeris#657) cancels an armed recv with
// IORING_ASYNC_CANCEL_ALL, keyed on the recv's user_data
// (prepCancelUserDataReported). Through 5.18, io_async_cancel_prep rejects any
// non-zero cancel_flags with -EINVAL, and no IORING_FEAT bit reports the
// flags, so the kernel has to be asked. From 5.19, where New's decision
// is the kernel's answer (requireAsyncCancelFlags), a version is still no
// answer: a vendor kernel can claim one its feature surface does not match.
// Below 5.19 New does use the version (celeris#872): this
// probe sends only the user_data+CANCEL_ALL form, which a partial backport can
// accept while the engine's other cancel forms still fail, so no answer of it
// clears a kernel that predates the floor.
//
// The probe submits exactly the reap's SQE form against a user_data that
// nothing carries and reads the cancel's own completion; see
// classifyAsyncCancelProbe for how the result reads. The cancel runs inline
// at submit on every kernel measured, so its completion is normally there
// when Submit returns; a short wait covers any that is not. That wait is
// retried once if it comes back early with nothing to read: SubmitAndWaitTimeout
// returns nil both when its timeout expires and when a signal cuts the wait
// short (EINTR), and only an early return can be the latter (celeris#681 N1).
// A probe that fails before that completion is read reports
// asyncCancelNoAnswer, never a rejection.
func probeAsyncCancelFlags() (asyncCancelProbe, string) {
	return probeAsyncCancel(cancelAll)
}

// probeAsyncCancel is probeAsyncCancelFlags with the cancel_flags word
// given: the probe passes the reap's own (IORING_ASYNC_CANCEL_ALL), and a
// test passes a bit no kernel defines, which every kernel rejects, to drive
// the rejection through the same submit and completion path.
func probeAsyncCancel(cancelFlags uint32) (asyncCancelProbe, string) {
	ring, err := newAsyncCancelProbeRing()
	if err != nil {
		return asyncCancelNoAnswer, "NewRing failed: " + err.Error()
	}
	defer func() { _ = ring.Close() }()

	sqe := ring.GetSQE()
	if sqe == nil {
		return asyncCancelNoAnswer, "GetSQE returned nil"
	}
	prepCancelUserDataReported(sqe, asyncCancelProbeTarget)
	*(*uint32)(unsafe.Pointer(&(*[sqeSize]byte)(sqe)[28])) = cancelFlags
	setSQEUserData(sqe, asyncCancelProbeTag)
	if _, err := asyncCancelProbeSubmit(ring); err != nil {
		return asyncCancelNoAnswer, "Submit failed: " + err.Error()
	}
	head, tail := ring.BeginCQ()
	if head == tail {
		timeout := asyncCancelProbeTimeout
		deadline := time.Now().Add(timeout)
		for waits, left := 1, timeout; ; waits++ {
			if err := asyncCancelProbeWait(ring, left); err != nil {
				return asyncCancelNoAnswer, "SubmitAndWaitTimeout failed: " + err.Error()
			}
			if head, tail = ring.BeginCQ(); head != tail {
				break
			}
			// An early return with nothing to read is a wait a signal cut
			// short: wait once more, for the rest of the time. That time is
			// read once, checked and passed as it was checked: a zero or
			// negative timespec would make the wait return at once, or EINVAL
			// (probeWaitCQE does the same).
			if left = time.Until(deadline); waits == 2 || left <= 0 {
				return asyncCancelNoAnswer, fmt.Sprintf("no CQE produced for the cancel (waited %v, %d wait(s))", timeout, waits)
			}
		}
	}
	cqe := ring.cqeAt(head)
	ud, res := cqe.UserData, cqe.Res
	ring.EndCQ(head + 1)
	if ud != asyncCancelProbeTag {
		return asyncCancelNoAnswer, fmt.Sprintf("unexpected CQE user_data %#x (want the cancel's %#x)", ud, asyncCancelProbeTag)
	}
	return classifyAsyncCancelProbe(res)
}

// classifyAsyncCancelProbe reads the completion of the probe's cancel, a
// cancel with IORING_ASYNC_CANCEL_ALL of a user_data nothing carries:
//
//   - res >= 0: the flags were accepted. With CANCEL_ALL, res is the number
//     of ops cancelled, so the miss the probe makes is 0.
//   - -ENOENT: accepted too. It is how a cancel without CANCEL_ALL reports a
//     miss; no kernel measured answers the probe with it.
//   - -EINVAL: rejected. Measured on 5.15.0-191: every cancel form celeris
//     builds returns -EINVAL there and leaves its target running.
//   - anything else: unexpected, an answer the probe does not understand
//     (celeris#681 N2). It is neither the kernel's acceptance nor its
//     rejection, so it is a class of its own: the reap stays off, and the
//     reason names the errno, which New logs at Warn on every kernel.
//
// Split out of probeAsyncCancelFlags so every outcome can be checked against
// a synthetic result.
func classifyAsyncCancelProbe(res int32) (asyncCancelProbe, string) {
	switch {
	case res >= 0, res == -int32(unix.ENOENT):
		return asyncCancelAccepted, ""
	case res == -int32(unix.EINVAL):
		return asyncCancelRejected, "IORING_ASYNC_CANCEL flags rejected: cqe.res=-22 (EINVAL)"
	default:
		return asyncCancelUnexpected, fmt.Sprintf("the cancel completed with cqe.res=%d (%s), which the probe does not recognise",
			res, errnoName(-res))
	}
}

// errnoName names errno e for a log line: its symbol (EBADF), or its number
// when the platform has no name for it.
func errnoName(e int32) string {
	if name := unix.ErrnoName(unix.Errno(e)); name != "" {
		return name
	}
	return fmt.Sprintf("errno %d", e)
}
