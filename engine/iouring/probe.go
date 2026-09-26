//go:build linux

package iouring

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Probe results are cached process-wide via sync.Once: kernel capabilities
// don't change inside a running process, but iouring.New() can be called
// hundreds-to-thousands of times in benchmark harnesses (a cross-framework
// matrix can run thousands of cells in one process with -race), and
// re-running every probe
// (each opens a temp ring + a TCP listener + a dial) on every New() is
// pure overhead. The async-cancel-flags probe is cached only once the kernel
// has answered it (probeAsyncCancelFlagsCached).
var (
	cachedSendZC          sync.Once
	cachedSendZCResult    SendZCProbeResult
	cachedSendZCReason    string
	cachedFixedFiles      sync.Once
	cachedFixedFilesOK    bool
	cachedFixedFilesReas  string
	cachedPbufRing        sync.Once
	cachedPbufRingOK      bool
	cachedPbufRingReason  string
	cachedMultiAccept     sync.Once
	cachedMultiAcceptOK   bool
	cachedMultiAcceptReas string
	// The async-cancel-flags probe's answer, once the kernel has given one:
	// asyncCancelKnown says whether cachedAsyncCancelRes/Reas hold it. All
	// three are guarded by asyncCancelMu.
	asyncCancelMu         sync.Mutex
	asyncCancelKnown      bool
	cachedAsyncCancelRes  asyncCancelProbe
	cachedAsyncCancelReas string
)

// probeSendZCCached returns the cached SendZC probe result, running the
// probe at most once per process.
func probeSendZCCached() (SendZCProbeResult, string) {
	cachedSendZC.Do(func() {
		cachedSendZCResult, cachedSendZCReason = probeSendZC()
	})
	return cachedSendZCResult, cachedSendZCReason
}

// probeFixedFilesCached returns the cached FixedFiles probe result.
func probeFixedFilesCached() (bool, string) {
	cachedFixedFiles.Do(func() {
		cachedFixedFilesOK, cachedFixedFilesReas = probeFixedFiles()
	})
	return cachedFixedFilesOK, cachedFixedFilesReas
}

// probeProvidedBuffersCached returns the cached ProvidedBuffers probe result.
func probeProvidedBuffersCached() (bool, string) {
	cachedPbufRing.Do(func() {
		cachedPbufRingOK, cachedPbufRingReason = probeProvidedBuffers()
	})
	return cachedPbufRingOK, cachedPbufRingReason
}

// probeMultishotAcceptCached returns the cached MultishotAccept probe result.
func probeMultishotAcceptCached() (bool, string) {
	cachedMultiAccept.Do(func() {
		cachedMultiAcceptOK, cachedMultiAcceptReas = probeMultishotAccept()
	})
	return cachedMultiAcceptOK, cachedMultiAcceptReas
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
// (git grep -n 't\.Parallel()' -- 'engine/iouring/*_test.go' is empty, while
// the same grep finds engine/provider_test.go), so the constraint holds
// today; it is documented rather than enforced because guarding the
// seams would mean a mutex-taking setter and a save/restore helper for each
// of the four, which is more machinery than a constraint one grep checks.
var runAsyncCancelProbe = probeAsyncCancelFlags

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
// On loopback, the result is always SendZCCopyFallback (kernel copies data,
// no DMA). On a real NIC with working zero-copy support, the result is
// SendZCTrueZeroCopy. On ENA (AWS), the result is SendZCBroken because
// the notification CQE never arrives.
func probeSendZC() (SendZCProbeResult, string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return SendZCUnsupported, "net.Listen failed: " + err.Error()
	}
	defer func() { _ = ln.Close() }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return SendZCUnsupported, "net.Dial failed: " + err.Error()
	}
	defer func() { _ = conn.Close() }()

	accepted, err := ln.Accept()
	if err != nil {
		return SendZCUnsupported, "ln.Accept failed: " + err.Error()
	}
	defer func() { _ = accepted.Close() }()

	rawConn, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		return SendZCUnsupported, "SyscallConn failed: " + err.Error()
	}

	var fd int
	if err := rawConn.Control(func(f uintptr) { fd = int(f) }); err != nil {
		return SendZCUnsupported, "SyscallConn.Control failed: " + err.Error()
	}
	if fd <= 0 {
		return SendZCUnsupported, fmt.Sprintf("invalid socket fd from Control: %d", fd)
	}
	ring, err := NewRing(4, 0, 0)
	if err != nil {
		return SendZCUnsupported, "NewRing failed: " + err.Error()
	}
	defer func() { _ = ring.Close() }()

	// Prepare SEND_ZC with REPORT_USAGE flag so the notification CQE tells
	// us whether true zero-copy or copy fallback was used.
	payload := []byte("probe-send-zc-test-payload")
	sqe := ring.GetSQE()
	if sqe == nil {
		return SendZCUnsupported, "GetSQE returned nil"
	}
	prepSendZC(sqe, fd, payload, false)
	// Set IORING_SEND_ZC_REPORT_USAGE in ioprio field (offset 2).
	sqeBytes := (*[sqeSize]byte)(sqe)
	*(*uint16)(unsafe.Pointer(&sqeBytes[2])) = sendZCReportUsage
	setSQEUserData(sqe, 42)

	// Submit and wait for the first CQE.
	if err := ring.SubmitAndWaitTimeout(500 * time.Millisecond); err != nil {
		return SendZCUnsupported, "SubmitAndWaitTimeout (initial) failed: " + err.Error()
	}

	cqHead, cqTail := ring.BeginCQ()
	if cqHead == cqTail {
		return SendZCUnsupported, "no initial CQE produced after submit"
	}
	entry := ring.cqeAt(cqHead)
	initialRes := entry.Res
	initialFlags := entry.Flags
	ring.EndCQ(cqHead + 1)

	if initialRes < 0 || initialFlags&0x02 == 0 {
		return parseSendZCResult(initialRes, initialFlags, false, nil, 0, 0)
	}

	// Wait for the notification CQE.
	if err := ring.SubmitAndWaitTimeout(2 * time.Second); err != nil {
		return parseSendZCResult(initialRes, initialFlags, false, err, 0, 0)
	}

	cqHead, cqTail = ring.BeginCQ()
	if cqHead == cqTail {
		return parseSendZCResult(initialRes, initialFlags, false, nil, 0, 0)
	}

	entry = ring.cqeAt(cqHead)
	notifFlags := entry.Flags
	notifRes := entry.Res
	ring.EndCQ(cqHead + 1)

	// Clean up: read the sent data on the receiver side.
	buf := make([]byte, 64)
	_ = accepted.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, _ = accepted.Read(buf)

	return parseSendZCResult(initialRes, initialFlags, true, nil, notifRes, notifFlags)
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
//   - "auto", "" (default): enabled when the functional probe passed. Whether that stays the
//     default is an open measurement owned by celeris#585 (SEND_ZC on/off A/B on the fabric).
//   - any other value: returns recognized=false and falls back to auto behavior.
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
	ring, err := NewRing(8, 0, 0)
	if err != nil {
		return false, "NewRing failed: " + err.Error()
	}
	defer func() { _ = ring.Close() }()

	if err := ring.RegisterFiles(16); err != nil {
		return false, "RegisterFiles failed: " + err.Error()
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return false, "net.Listen failed: " + err.Error()
	}
	defer func() { _ = ln.Close() }()

	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		return false, "SyscallConn failed: " + err.Error()
	}
	var listenFD int
	_ = rc.Control(func(fd uintptr) { listenFD = int(fd) })
	if listenFD <= 0 {
		return false, fmt.Sprintf("listen FD=%d <= 0", listenFD)
	}

	sqe := ring.GetSQE()
	if sqe == nil {
		return false, "GetSQE returned nil"
	}
	prepMultishotAcceptDirect(sqe, listenFD)
	setSQEUserData(sqe, 0xF17EDF11E) // distinct tag for this probe
	if _, err := ring.Submit(); err != nil {
		return false, "Submit failed: " + err.Error()
	}

	// Trigger one accept so the kernel produces a CQE for the multishot SQE.
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.Dial("tcp", ln.Addr().String())
	if err != nil {
		return false, "probe dial failed: " + err.Error()
	}
	defer func() { _ = conn.Close() }()

	if err := ring.SubmitAndWaitTimeout(500 * time.Millisecond); err != nil {
		return false, "SubmitAndWaitTimeout failed: " + err.Error()
	}
	head, tail := ring.BeginCQ()
	if head == tail {
		return false, "no CQE produced after multishot accept-direct + dial"
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
	if res < 0 {
		return false, fmt.Sprintf("ACCEPT_DIRECT rejected by kernel: cqe.res=%d (likely -EINVAL=-22)", res)
	}
	return true, ""
}

// probeProvidedBuffers tests whether IORING_REGISTER_PBUF_RING works on this
// kernel. Some patched/embedded kernels report 5.19+ but reject the register
// call (e.g. ENOSYS or EINVAL). Without a working pbuf ring, multishot recv
// cannot be used — the engine must fall back to single-shot per-connection
// recv buffers regardless of the kernel-version-based tier.
func probeProvidedBuffers() (bool, string) {
	ring, err := NewRing(8, 0, 0)
	if err != nil {
		return false, "NewRing failed: " + err.Error()
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
		return false, "mmap probe ring: " + err.Error()
	}
	defer func() { _ = unix.Munmap(region) }()

	if regErr := ring.RegisterPbufRing(0xFFFF, probeCount, unsafe.Pointer(&region[0])); regErr != nil {
		return false, "RegisterPbufRing rejected: " + regErr.Error()
	}
	if unregErr := ring.UnregisterPbufRing(0xFFFF); unregErr != nil {
		return false, "UnregisterPbufRing failed (ring left dirty): " + unregErr.Error()
	}
	return true, ""
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
	ring, err := NewRing(8, 0, 0)
	if err != nil {
		return false, "NewRing failed: " + err.Error()
	}
	defer func() { _ = ring.Close() }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return false, "net.Listen failed: " + err.Error()
	}
	defer func() { _ = ln.Close() }()

	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		return false, "SyscallConn failed: " + err.Error()
	}
	var listenFD int
	_ = rc.Control(func(fd uintptr) { listenFD = int(fd) })
	if listenFD <= 0 {
		return false, fmt.Sprintf("listen FD=%d <= 0", listenFD)
	}

	sqe := ring.GetSQE()
	if sqe == nil {
		return false, "GetSQE returned nil"
	}
	prepMultishotAccept(sqe, listenFD)
	setSQEUserData(sqe, 0xACCE9701B0)
	if _, subErr := ring.Submit(); subErr != nil {
		return false, "Submit failed: " + subErr.Error()
	}

	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.Dial("tcp", ln.Addr().String())
	if err != nil {
		return false, "probe dial failed: " + err.Error()
	}
	defer func() { _ = conn.Close() }()

	if waitErr := ring.SubmitAndWaitTimeout(500 * time.Millisecond); waitErr != nil {
		return false, "SubmitAndWaitTimeout failed: " + waitErr.Error()
	}
	head, tail := ring.BeginCQ()
	if head == tail {
		return false, "no CQE produced after multishot accept + dial"
	}
	cqe := ring.cqeAt(head)
	res := cqe.Res
	flags := cqe.Flags
	ring.EndCQ(head + 1)

	if res < 0 {
		return false, fmt.Sprintf("multishot accept rejected by kernel: cqe.res=%d", res)
	}
	// Close the accepted FD so it doesn't leak — we have its fd in res.
	if res > 0 {
		_ = unix.Close(int(res))
	}
	if flags&cqeFMore == 0 {
		return false, "first multishot accept CQE missing CQE_F_MORE (kernel won't re-arm)"
	}
	return true, ""
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
// flags, so the kernel has to be asked. Version-based selection is no answer
// either: the Base tier covers every 5.10-5.18 kernel, and a vendor kernel can
// claim a version its feature surface does not match.
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
		for waits := 1; ; waits++ {
			if err := asyncCancelProbeWait(ring, time.Until(deadline)); err != nil {
				return asyncCancelNoAnswer, "SubmitAndWaitTimeout failed: " + err.Error()
			}
			if head, tail = ring.BeginCQ(); head != tail {
				break
			}
			// An early return with nothing to read is a wait a signal cut
			// short: wait once more, for the rest of the time.
			if waits == 2 || !time.Now().Before(deadline) {
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
		return asyncCancelRejected, "IORING_ASYNC_CANCEL flags rejected: cqe.res=-22 (EINVAL); the kernel predates Linux 5.19"
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
