//go:build linux

package iouring

import (
	"fmt"
	"net"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Probe results are cached process-wide via sync.Once: kernel capabilities
// don't change inside a running process, but iouring.New() can be called
// hundreds-to-thousands of times in benchmark harnesses (perfmatrix runs
// 1980 cells in one process with -race), and re-running every probe
// (each opens a temp ring + a TCP listener + a dial) on every New() is
// pure overhead.
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

// SEND_ZC ioprio flags and notification result values.
const (
	sendZCReportUsage  = 1 << 3 // IORING_SEND_ZC_REPORT_USAGE: request ZC usage info in notification
	notifUsageZCCopied = 2      // IORING_NOTIF_USAGE_ZC_COPIED: data was copied, not zero-copied
)

// SendZCProbeResult describes the outcome of the SEND_ZC runtime probe.
type SendZCProbeResult int

const (
	// SendZCUnsupported means the kernel doesn't support the SEND_ZC opcode.
	SendZCUnsupported SendZCProbeResult = iota
	// SendZCBroken means the kernel accepts SEND_ZC but the notification CQE
	// never arrives (e.g., ENA driver DMA completion bug).
	SendZCBroken
	// SendZCCopyFallback means SEND_ZC works but the kernel copies data instead
	// of using DMA zero-copy. The notification arrives correctly. This happens
	// on loopback or NICs without scatter-gather DMA. SEND_ZC is functional
	// but provides no performance benefit over regular SEND.
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
	_ = rawConn.Control(func(f uintptr) { fd = int(f) })

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
	if entry.Res < 0 {
		res := entry.Res
		ring.EndCQ(cqHead + 1)
		return SendZCUnsupported, fmt.Sprintf("kernel rejected SEND_ZC opcode: cqe.res=%d (likely -ENOSYS=-38 or -EINVAL=-22)", res)
	}

	flags := *(*uint32)(unsafe.Add(unsafe.Pointer(entry), 8))
	hasMore := flags&0x02 != 0 // CQE_F_MORE
	ring.EndCQ(cqHead + 1)

	if !hasMore {
		// Kernel accepted SEND_ZC but fell back to copy without entering the
		// zero-copy path at all. No notification CQE will follow. Buffer is
		// safe to reuse immediately. On mainline kernels this shouldn't happen
		// (CQE_F_MORE is always set), but some patched kernels skip it.
		// Treat as copy fallback — SEND_ZC works but has no ZC benefit.
		return SendZCCopyFallback, "first CQE missing CQE_F_MORE flag (no notification will follow)"
	}

	// Wait for the notification CQE.
	if err := ring.SubmitAndWaitTimeout(2 * time.Second); err != nil {
		return SendZCBroken, "notification CQE wait timed out: " + err.Error()
	}

	cqHead, cqTail = ring.BeginCQ()
	if cqHead == cqTail {
		return SendZCBroken, "no notification CQE produced (waited 2s)"
	}

	entry = ring.cqeAt(cqHead)
	notifFlags := *(*uint32)(unsafe.Add(unsafe.Pointer(entry), 8))
	isNotif := notifFlags&0x04 != 0 // CQE_F_NOTIF
	notifRes := entry.Res
	ring.EndCQ(cqHead + 1)

	if !isNotif {
		return SendZCBroken, fmt.Sprintf("second CQE missing CQE_F_NOTIF flag (flags=%#x)", notifFlags)
	}

	// Check the notification's res field for REPORT_USAGE result.
	if notifRes&notifUsageZCCopied != 0 {
		return SendZCCopyFallback, "REPORT_USAGE notification reports IORING_NOTIF_USAGE_ZC_COPIED (kernel did the copy)"
	}

	// Clean up: read the sent data on the receiver side.
	buf := make([]byte, 64)
	_ = accepted.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, _ = accepted.Read(buf)

	return SendZCTrueZeroCopy, ""
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
	// Res = -EINVAL (-22) means the kernel registered files but refuses
	// ACCEPT_DIRECT (seen on 6.6.10-cix aarch64). Treat as unsupported.
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
	flags := *(*uint32)(unsafe.Add(unsafe.Pointer(cqe), 8))
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
