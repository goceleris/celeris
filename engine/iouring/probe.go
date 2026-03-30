//go:build linux

package iouring

import (
	"net"
	"time"
	"unsafe"
)

// SEND_ZC ioprio flags and notification result values.
const (
	sendZCReportUsage = 1 << 3 // IORING_SEND_ZC_REPORT_USAGE: request ZC usage info in notification
	notifUsageZCCopied = 2     // IORING_NOTIF_USAGE_ZC_COPIED: data was copied, not zero-copied
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
	defer ln.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return SendZCUnsupported
	}
	defer conn.Close()

	accepted, err := ln.Accept()
	if err != nil {
		return SendZCUnsupported
	}
	defer accepted.Close()

	rawConn, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		return SendZCUnsupported
	}

	var fd int
	rawConn.Control(func(f uintptr) { fd = int(f) })

	ring, err := NewRing(4, 0, 0)
	if err != nil {
		return SendZCUnsupported
	}
	defer ring.Close()

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

// probeFixedFiles tests whether ACCEPT_DIRECT (fixed files) works by
// registering a fixed file table on a temporary ring.
func probeFixedFiles() bool {
	ring, err := NewRing(4, 0, 0)
	if err != nil {
		return false
	}
	defer ring.Close()

	if err := ring.RegisterFiles(16); err != nil {
		return false
	}
	return true
}
