//go:build linux

package iouring

import (
	"net"
	"time"
	"unsafe"
)

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
func probeSendZC() SendZCProbeResult {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return SendZCUnsupported
	}
	defer func() { _ = ln.Close() }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return SendZCUnsupported
	}
	defer func() { _ = conn.Close() }()

	accepted, err := ln.Accept()
	if err != nil {
		return SendZCUnsupported
	}
	defer func() { _ = accepted.Close() }()

	rawConn, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		return SendZCUnsupported
	}

	var fd int
	_ = rawConn.Control(func(f uintptr) { fd = int(f) })

	ring, err := NewRing(4, 0, 0)
	if err != nil {
		return SendZCUnsupported
	}
	defer func() { _ = ring.Close() }()

	// Prepare SEND_ZC with REPORT_USAGE flag so the notification CQE tells
	// us whether true zero-copy or copy fallback was used.
	payload := []byte("probe-send-zc-test-payload")
	sqe := ring.GetSQE()
	if sqe == nil {
		return SendZCUnsupported
	}
	prepSendZC(sqe, fd, payload, false)
	// Set IORING_SEND_ZC_REPORT_USAGE in ioprio field (offset 2).
	sqeBytes := (*[sqeSize]byte)(sqe)
	*(*uint16)(unsafe.Pointer(&sqeBytes[2])) = sendZCReportUsage
	setSQEUserData(sqe, 42)

	// Submit and wait for the first CQE.
	if err := ring.SubmitAndWaitTimeout(500 * time.Millisecond); err != nil {
		return SendZCUnsupported
	}

	cqHead, cqTail := ring.BeginCQ()
	if cqHead == cqTail {
		return SendZCUnsupported
	}
	entry := ring.cqeAt(cqHead)
	if entry.Res < 0 {
		ring.EndCQ(cqHead + 1)
		return SendZCUnsupported
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
		return SendZCCopyFallback
	}

	// Wait for the notification CQE.
	if err := ring.SubmitAndWaitTimeout(2 * time.Second); err != nil {
		return SendZCBroken
	}

	cqHead, cqTail = ring.BeginCQ()
	if cqHead == cqTail {
		return SendZCBroken
	}

	entry = ring.cqeAt(cqHead)
	notifFlags := *(*uint32)(unsafe.Add(unsafe.Pointer(entry), 8))
	isNotif := notifFlags&0x04 != 0 // CQE_F_NOTIF
	ring.EndCQ(cqHead + 1)

	if !isNotif {
		return SendZCBroken
	}

	// Check the notification's res field for REPORT_USAGE result.
	if entry.Res&notifUsageZCCopied != 0 {
		return SendZCCopyFallback
	}

	// Clean up: read the sent data on the receiver side.
	buf := make([]byte, 64)
	_ = accepted.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, _ = accepted.Read(buf)

	return SendZCTrueZeroCopy
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
func probeFixedFiles() bool {
	ring, err := NewRing(8, 0, 0)
	if err != nil {
		return false
	}
	defer func() { _ = ring.Close() }()

	if err := ring.RegisterFiles(16); err != nil {
		return false
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return false
	}
	defer func() { _ = ln.Close() }()

	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		return false
	}
	var listenFD int
	_ = rc.Control(func(fd uintptr) { listenFD = int(fd) })
	if listenFD <= 0 {
		return false
	}

	sqe := ring.GetSQE()
	if sqe == nil {
		return false
	}
	prepMultishotAcceptDirect(sqe, listenFD)
	setSQEUserData(sqe, 0xF17EDF11E) // distinct tag for this probe
	if _, err := ring.Submit(); err != nil {
		return false
	}

	// Trigger one accept so the kernel produces a CQE for the multishot SQE.
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.Dial("tcp", ln.Addr().String())
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()

	if err := ring.SubmitAndWaitTimeout(500 * time.Millisecond); err != nil {
		return false
	}
	head, tail := ring.BeginCQ()
	if head == tail {
		return false
	}
	cqe := ring.cqeAt(head)
	ring.EndCQ(head + 1)
	// Res = -EINVAL (-22) means the kernel registered files but refuses
	// ACCEPT_DIRECT (seen on 6.6.10-cix aarch64). Treat as unsupported.
	return cqe.Res >= 0
}
