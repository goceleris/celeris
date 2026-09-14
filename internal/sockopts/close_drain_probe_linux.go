//go:build linux && celeris_closeprobe

package sockopts

import (
	"fmt"
	"os"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// Measurement variant of [CloseDrain] (celeris#583). Two gates, each read
// ONCE at process start so the close path never touches the environment:
//
//   - CELERIS_DEBUG_SKIP_CLOSE_DRAIN=1 — the drain-off arm: the recv queue is
//     left untouched between shutdown(SHUT_WR) and close(2), so close(2) sees
//     whatever the peer left queued. This is the ONLY way the drain-off arm
//     is produced; nothing else in the close sequence changes.
//   - CELERIS_DEBUG_CLOSE_PROBE=1 — one CLOSE-PROBE record per close: SIOCINQ
//     before and after the drain, the bytes the drain consumed, SIOCOUTQ
//     immediately before close(2), the call site, fd and peer address (the
//     join key to the peer's own record). One record per close, never per
//     byte (a per-byte probe perturbed the race it measured in celeris#562).
var (
	skipCloseDrain   = os.Getenv("CELERIS_DEBUG_SKIP_CLOSE_DRAIN") == "1"
	closeProbeActive = os.Getenv("CELERIS_DEBUG_CLOSE_PROBE") == "1"
)

// CloseProbe is what one probed close recorded.
type CloseProbe struct {
	Site      string // engine and function, e.g. "iouring/finishCloseDetached"
	FD        int
	RAddr     string
	Skipped   bool // drain-off arm
	InqBefore int  // SIOCINQ after SHUT_WR, before the drain
	Drained   int  // bytes DrainRecvBuffer consumed (0 on the drain-off arm)
	InqAfter  int  // SIOCINQ after the drain; >0 means close(2) will RST
	Outq      int  // SIOCOUTQ immediately before close(2)
}

// CloseProbeHook, when set, receives every CloseProbe record on the closing
// thread, immediately before close(2). A test uses it to join the server's
// view of a close to the peer's terminal read result without parsing logs.
// It must not block. Records are also written to stderr as CLOSE-PROBE lines.
var CloseProbeHook atomic.Pointer[func(CloseProbe)]

// CloseProbeEnabled reports whether the process was started with the probe
// gate on, so a test can refuse to run uninstrumented.
func CloseProbeEnabled() bool { return closeProbeActive }

// CloseDrainSkipped reports whether the process runs the drain-off arm.
func CloseDrainSkipped() bool { return skipCloseDrain }

// CloseDrain runs between shutdown(SHUT_WR) and close(2). See the release
// variant for the contract.
func CloseDrain(fd int, site, raddr string) int {
	if !closeProbeActive {
		if skipCloseDrain {
			return 0
		}
		return DrainRecvBuffer(fd)
	}
	rec := CloseProbe{Site: site, FD: fd, RAddr: raddr, Skipped: skipCloseDrain}
	rec.InqBefore, _ = unix.IoctlGetInt(fd, unix.SIOCINQ)
	if !skipCloseDrain {
		rec.Drained = DrainRecvBuffer(fd)
	}
	rec.InqAfter, _ = unix.IoctlGetInt(fd, unix.SIOCINQ)
	rec.Outq, _ = unix.IoctlGetInt(fd, unix.SIOCOUTQ)
	fmt.Fprintf(os.Stderr,
		"CLOSE-PROBE site=%s fd=%d raddr=%s skip=%t inq_before=%d drained=%d inq_after=%d outq=%d\n",
		rec.Site, rec.FD, rec.RAddr, rec.Skipped, rec.InqBefore, rec.Drained, rec.InqAfter, rec.Outq)
	if h := CloseProbeHook.Load(); h != nil {
		(*h)(rec)
	}
	return rec.Drained
}
