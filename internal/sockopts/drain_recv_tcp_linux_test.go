//go:build linux

package sockopts

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestDrainRecvBufferTCPTruthTable is Tier 0 of celeris#583: the kernel's
// FIN-vs-RST decision at close(2), measured on a real AF_INET loopback TCP
// connection instead of argued from tcp_close(). The AF_UNIX socketpair the
// sibling tests use has no FIN/RST semantics and cannot see any of this.
//
// Twelve cells — drain {on, off} x unread-at-close {0, 16 KiB (< cap),
// 128 KiB (> cap)} x server-outbound-staged {0, 64 KiB with a peer that is
// NOT reading} — each repeated t0Reps times. Every repetition runs exactly
// the engines' close sequence (shutdown(SHUT_WR) -> [DrainRecvBuffer] ->
// close(2)) and records, per connection:
//
//   - SIOCINQ before and after the drain, and the bytes the drain consumed;
//   - SIOCOUTQ immediately before close(2) — the bytes an RST purges (the
//     unacked FIN that SHUT_WR already sent counts as 1, so "staged data"
//     is outq > 1);
//   - the TcpExt TCPAbortOnClose / TCPAbortOnData deltas around close(2),
//     the kernel's own attribution of WHY it sent a reset;
//   - the peer's terminal read result (io.EOF / ECONNRESET / deadline), the
//     bytes it received of what the server's kernel had accepted, and its
//     SO_ERROR afterwards.
//
// What the first run of this table taught, and what the assertions encode:
// the engines send the FIN with shutdown(SHUT_WR) BEFORE the drain and the
// close, so on the path under test the peer always has the FIN queued ahead
// of any RST. Linux delivers that FIN as EOF (SOCK_DONE) even when an RST
// follows; the RST then only sets SO_ERROR=EPIPE (tcp_reset in CLOSE_WAIT).
// The reader observes ECONNRESET only when server->peer DATA is still queued
// ahead of the FIN — the staged cells — and then the RST also purges the
// unsent remainder, which is the loss the closure says the drain prevents.
//
// Negative controls: unread=0 gives EOF, SO_ERROR=0 and zero abort deltas in
// both arms (the RST detector does not fire spuriously); drain-off with
// 16 KiB unread must produce TCPAbortOnClose +1 on every repetition (the
// harness can see a reset at all); and post_close_write sends one byte from
// the peer AFTER a clean close, which is reset with TCPAbortOnData only while
// the orphan is still a full FIN_WAIT2 socket — at the default
// tcp_fin_timeout=60 tcp_close() converts it to a time-wait mini-socket on
// the spot (tmo == TCP_TIMEWAIT_LEN), whose reset carries no MIB counter, so
// that control asserts +1 only when /proc/sys/net/ipv4/tcp_fin_timeout > 60
// and asserts 0 otherwise. Run it both ways (docker --sysctl
// net.ipv4.tcp_fin_timeout=120) to see the two reset reasons separate.
func TestDrainRecvBufferTCPTruthTable(t *testing.T) {
	reps := t0Reps
	if testing.Short() {
		reps = 3
	}
	if v := os.Getenv("DRAIN583_REPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			reps = n
		}
	}
	finTimeout := readIntFile(t, "/proc/sys/net/ipv4/tcp_fin_timeout")
	t.Logf("TIER0-ENV kernel=%s tcp_fin_timeout=%d reps=%d cap=%d", kernelRelease(), finTimeout, reps, drainRecvBufSize*drainRecvMaxReads)

	capBytes := drainRecvBufSize * drainRecvMaxReads
	unreadSizes := []int{0, 16 << 10, 128 << 10}
	stagedSizes := []int{0, t0StagedBytes}

	for _, drain := range []bool{true, false} {
		for _, unread := range unreadSizes {
			for _, staged := range stagedSizes {
				cell := t0Cell{drain: drain, unread: unread, staged: staged}
				t.Run(cell.String(), func(t *testing.T) {
					var agg t0Aggregate
					for rep := range reps {
						r := runT0Cell(t, cell)
						agg.add(r)
						t.Logf("TIER0 drain=%t unread=%d staged=%d rep=%d peer=%s peerSoErr=%s peerBytes=%d serverAccepted=%d inq_before=%d drained=%d inq_after=%d outq=%d abortOnClose=%+d abortOnData=%+d",
							drain, unread, staged, rep, r.peerKind, r.peerSoErr, r.peerBytes, r.serverAccepted,
							r.inqBefore, r.drained, r.inqAfter, r.outq, r.abortOnClose, r.abortOnData)
					}
					t.Logf("TIER0-CELL drain=%t unread=%d staged=%d n=%d EOF=%d RST=%d deadline=%d other=%d soErrEPIPE=%d soErrECONNRESET=%d soErr0=%d peerBytesMin=%d peerBytesMax=%d serverAcceptedMin=%d serverAcceptedMax=%d inqBeforeMin=%d inqBeforeMax=%d drainedMin=%d drainedMax=%d inqAfterMin=%d inqAfterMax=%d inqAfterPos=%d outqMin=%d outqMax=%d outqDataPos=%d abortOnClose=%d abortOnData=%d",
						drain, unread, staged, reps, agg.eof, agg.rst, agg.deadline, agg.other,
						agg.soErrEPIPE, agg.soErrECONNRESET, agg.soErrNone,
						agg.peerBytesMin, agg.peerBytesMax, agg.serverAcceptedMin, agg.serverAcceptedMax,
						agg.inqBeforeMin, agg.inqBeforeMax, agg.drainedMin, agg.drainedMax,
						agg.inqAfterMin, agg.inqAfterMax, agg.inqAfterPos,
						agg.outqMin, agg.outqMax, agg.outqDataPos,
						agg.abortOnClose, agg.abortOnData)

					// The kernel's FIN/RST rule depends on nothing but whether
					// close(2) finds unread bytes. The peer stopped sending before
					// the close in every cell, so these rows are deterministic.
					resetSent := unread > 0 && (!drain || unread > capBytes)
					switch {
					case !resetSent:
						// unread=0 in both arms (negative control 1) and drain-on
						// with unread <= cap (the closure's mechanism): the queue is
						// empty at close(2), close sends FIN, nothing is reset.
						if agg.eof != reps || agg.abortOnClose != 0 || agg.abortOnData != 0 || agg.inqAfterPos != 0 || agg.soErrNone != reps {
							t.Errorf("drain=%t unread=%d: close(2) must find an empty queue and send FIN; got EOF=%d/%d SO_ERROR=0 on %d abortOnClose=%d abortOnData=%d inq_after>0 on %d",
								drain, unread, agg.eof, reps, agg.soErrNone, agg.abortOnClose, agg.abortOnData, agg.inqAfterPos)
						}
					default:
						// drain-off with unread>0 (negative control 3 at 16 KiB) and
						// drain-on above the cap: close(2) finds unread bytes and
						// resets, attributed to TCPAbortOnClose.
						if agg.abortOnClose != reps || agg.inqAfterPos != reps {
							t.Errorf("drain=%t unread=%d: close(2) must reset every time (TCPAbortOnClose +1 each, inq_after>0); got abortOnClose=%d/%d inq_after>0 on %d",
								drain, unread, agg.abortOnClose, reps, agg.inqAfterPos)
						}
						if staged == 0 {
							// The FIN from SHUT_WR is already at the peer: its
							// reader sees EOF and only SO_ERROR shows the reset.
							if agg.eof != reps || agg.soErrEPIPE != reps {
								t.Errorf("drain=%t unread=%d staged=0: the peer's reader must see the earlier FIN as EOF with SO_ERROR=EPIPE from the RST; got EOF=%d/%d EPIPE=%d RST=%d",
									drain, unread, agg.eof, reps, agg.soErrEPIPE, agg.rst)
							}
						} else if agg.rst != reps {
							t.Errorf("drain=%t unread=%d staged=%d: with data queued ahead of the FIN the peer must see ECONNRESET; got RST=%d/%d EOF=%d",
								drain, unread, staged, agg.rst, reps, agg.eof)
						}
					}
					// Outbound loss is the closure's stated reason for draining:
					// on the FIN path the peer eventually reads every byte the
					// kernel accepted; on the RST path the unsent remainder is
					// purged and the peer, whose window (16 KiB SO_RCVBUF) cannot
					// hold the 64 KiB, receives strictly less.
					if staged > 0 {
						if agg.outqDataPos != reps {
							t.Errorf("staged cell: a non-reading peer must leave outq>1 at close on every rep; got %d/%d", agg.outqDataPos, reps)
						}
						if !resetSent && agg.peerBytesMin != agg.serverAcceptedMin {
							t.Errorf("FIN path: peer must receive every accepted byte; peerBytesMin=%d serverAcceptedMin=%d", agg.peerBytesMin, agg.serverAcceptedMin)
						}
						if resetSent && agg.peerBytesMax >= agg.serverAcceptedMin {
							t.Errorf("RST path: the purge must be visible as peerBytes < serverAccepted; peerBytesMax=%d serverAcceptedMin=%d", agg.peerBytesMax, agg.serverAcceptedMin)
						}
					}
				})
			}
		}
	}

	// Negative control (2): data arriving AFTER a clean close(2) in both arms.
	// See the function comment for why TCPAbortOnData needs tcp_fin_timeout
	// > 60 to be observable; at the default the reset comes from the
	// time-wait mini-socket, which the peer still sees on its next write.
	for _, drain := range []bool{true, false} {
		t.Run(fmt.Sprintf("post_close_write/drain=%t", drain), func(t *testing.T) {
			var onData, onClose, secondWriteErr, rst int
			for rep := range reps {
				r := runT0PostCloseWrite(t, drain)
				onData += r.abortOnData
				onClose += r.abortOnClose
				if r.peerKind == "ECONNRESET" {
					rst++
				}
				if r.secondWriteErr != "" {
					secondWriteErr++
				}
				t.Logf("TIER0 post_close_write drain=%t rep=%d peer=%s peerSoErr=%s secondWrite=%q abortOnClose=%+d abortOnData=%+d", drain, rep, r.peerKind, r.peerSoErr, r.secondWriteErr, r.abortOnClose, r.abortOnData)
			}
			t.Logf("TIER0-CELL post_close_write drain=%t tcp_fin_timeout=%d n=%d RST=%d secondWriteErr=%d abortOnClose=%d abortOnData=%d", drain, finTimeout, reps, rst, secondWriteErr, onClose, onData)
			if onClose != 0 {
				t.Errorf("post-close write must never be attributed to TCPAbortOnClose; got %d over %d", onClose, reps)
			}
			if secondWriteErr != reps {
				t.Errorf("the peer's second write after the reset must fail (EPIPE/ECONNRESET) on every rep; got %d/%d", secondWriteErr, reps)
			}
			if finTimeout > 60 {
				if onData != reps {
					t.Errorf("tcp_fin_timeout=%d keeps the orphan a full FIN_WAIT2 socket, so post-close data must count TCPAbortOnData +1 per rep; got %d/%d", finTimeout, onData, reps)
				}
			} else if onData != 0 {
				t.Errorf("tcp_fin_timeout=%d converts the orphan to time-wait inside close(2), whose reset has no MIB counter; expected abortOnData=0, got %d", finTimeout, onData)
			}
		})
	}
}

const (
	t0Reps        = 20
	t0PeerRcvBuf  = 16 << 10
	t0ServerRcv   = 256 << 10
	t0ServerSnd   = 64 << 10
	t0StagedBytes = 64 << 10
	// t0Timeout bounds every wait in a repetition: the peer's terminal read
	// and the poll for the unread bytes to land in the server's queue.
	t0Timeout = 3 * time.Second
)

type t0Cell struct {
	drain  bool
	unread int
	staged int
}

func (c t0Cell) String() string {
	arm := "drain_off"
	if c.drain {
		arm = "drain_on"
	}
	return fmt.Sprintf("%s/unread=%d/staged=%d", arm, c.unread, c.staged)
}

type t0Result struct {
	peerKind       string
	peerSoErr      string
	peerBytes      int
	serverAccepted int
	inqBefore      int
	drained        int
	inqAfter       int
	outq           int
	abortOnClose   int
	abortOnData    int
	secondWriteErr string
}

type t0Aggregate struct {
	eof, rst, deadline, other              int
	soErrEPIPE, soErrECONNRESET, soErrNone int
	peerBytesMin, peerBytesMax             int
	serverAcceptedMin, serverAcceptedMax   int
	inqBeforeMin, inqBeforeMax             int
	drainedMin, drainedMax                 int
	inqAfterMin, inqAfterMax               int
	outqMin, outqMax                       int
	inqAfterPos, outqDataPos               int
	abortOnClose, abortOnData              int
	n                                      int
}

func minMax(first bool, v int, lo, hi *int) {
	if first || v < *lo {
		*lo = v
	}
	if first || v > *hi {
		*hi = v
	}
}

func (a *t0Aggregate) add(r t0Result) {
	switch r.peerKind {
	case "EOF":
		a.eof++
	case "ECONNRESET":
		a.rst++
	case "deadline":
		a.deadline++
	default:
		a.other++
	}
	switch r.peerSoErr {
	case "EPIPE":
		a.soErrEPIPE++
	case "ECONNRESET":
		a.soErrECONNRESET++
	case "0":
		a.soErrNone++
	}
	first := a.n == 0
	minMax(first, r.peerBytes, &a.peerBytesMin, &a.peerBytesMax)
	minMax(first, r.serverAccepted, &a.serverAcceptedMin, &a.serverAcceptedMax)
	minMax(first, r.inqBefore, &a.inqBeforeMin, &a.inqBeforeMax)
	minMax(first, r.drained, &a.drainedMin, &a.drainedMax)
	minMax(first, r.inqAfter, &a.inqAfterMin, &a.inqAfterMax)
	minMax(first, r.outq, &a.outqMin, &a.outqMax)
	if r.inqAfter > 0 {
		a.inqAfterPos++
	}
	// SIOCOUTQ counts the unacked FIN that SHUT_WR already sent as one
	// byte; data is anything beyond it.
	if r.outq > 1 {
		a.outqDataPos++
	}
	a.abortOnClose += r.abortOnClose
	a.abortOnData += r.abortOnData
	a.n++
}

// runT0Cell executes one repetition of a matrix cell and returns what the
// three vantage points (server ioctls, kernel counters, peer) recorded.
func runT0Cell(t *testing.T, cell t0Cell) t0Result {
	t.Helper()
	srv, peer := loopbackTCPPair(t)
	var r t0Result

	// The peer's `unread` bytes must be IN the server's receive queue, with
	// nothing still in flight, before the close sequence runs: a peer that
	// is still sending gets the post-close reset in either arm, which is a
	// different row of the table (see the post_close_write control).
	if cell.unread > 0 {
		writeFully(t, peer, make([]byte, cell.unread))
		waitInq(t, srv, cell.unread)
	}
	if cell.staged > 0 {
		// Non-blocking so a full send buffer cannot wedge the test; what
		// the kernel accepted is what the RST path has to purge.
		r.serverAccepted = writeNonblock(srv, make([]byte, cell.staged))
		// Let the transmit settle into the peer's (16 KiB) window so outq
		// reflects the steady state rather than a snapshot mid-copy.
		time.Sleep(30 * time.Millisecond)
	}

	// The engines' close sequence, instrumented.
	if err := unix.Shutdown(srv, unix.SHUT_WR); err != nil {
		t.Fatalf("shutdown(SHUT_WR): %v", err)
	}
	r.inqBefore, _ = unix.IoctlGetInt(srv, unix.SIOCINQ)
	if cell.drain {
		r.drained = DrainRecvBuffer(srv)
	}
	r.inqAfter, _ = unix.IoctlGetInt(srv, unix.SIOCINQ)
	r.outq, _ = unix.IoctlGetInt(srv, unix.SIOCOUTQ)
	before := readTCPExt(t)
	_ = unix.Close(srv)
	after := readTCPExt(t)
	r.abortOnClose = int(after["TCPAbortOnClose"] - before["TCPAbortOnClose"])
	r.abortOnData = int(after["TCPAbortOnData"] - before["TCPAbortOnData"])

	r.peerKind, r.peerBytes = readToTerminal(peer)
	r.peerSoErr = soError(peer)
	_ = unix.Close(peer)
	return r
}

// runT0PostCloseWrite is negative control (2): a clean close with nothing
// unread, then one byte from the peer, then a second write to observe the
// reset the first one provoked.
func runT0PostCloseWrite(t *testing.T, drain bool) t0Result {
	t.Helper()
	srv, peer := loopbackTCPPair(t)
	var r t0Result
	if err := unix.Shutdown(srv, unix.SHUT_WR); err != nil {
		t.Fatalf("shutdown(SHUT_WR): %v", err)
	}
	if drain {
		r.drained = DrainRecvBuffer(srv)
	}
	before := readTCPExt(t)
	_ = unix.Close(srv)
	// Let the FIN reach the peer before its byte goes out.
	time.Sleep(10 * time.Millisecond)
	if _, err := unix.Write(peer, []byte{'x'}); err != nil {
		t.Fatalf("post-close write: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	after := readTCPExt(t)
	r.abortOnClose = int(after["TCPAbortOnClose"] - before["TCPAbortOnClose"])
	r.abortOnData = int(after["TCPAbortOnData"] - before["TCPAbortOnData"])
	if _, err := unix.SendmsgN(peer, []byte{'y'}, nil, nil, unix.MSG_NOSIGNAL); err != nil {
		r.secondWriteErr = err.Error()
	}
	r.peerKind, r.peerBytes = readToTerminal(peer)
	r.peerSoErr = soError(peer)
	_ = unix.Close(peer)
	return r
}

// loopbackTCPPair returns an accepted server fd and its connected peer over
// 127.0.0.1. Buffer sizes are set on the LISTENER (the accepted socket
// inherits them, and the window scale is negotiated from them at SYN):
// server SO_RCVBUF 256 KiB so 128 KiB of unread fits, SO_SNDBUF 64 KiB so a
// non-reading peer leaves outq>0; peer SO_RCVBUF 16 KiB so its window cannot
// hold the 64 KiB the server stages. Both ends TCP_NODELAY so nothing sits
// in a Nagle timer between the peer's write and the server's queue.
func loopbackTCPPair(t *testing.T) (srv, peer int) {
	t.Helper()
	ln, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer func() { _ = unix.Close(ln) }()
	mustSetsockopt(t, ln, unix.SOL_SOCKET, unix.SO_RCVBUF, t0ServerRcv)
	mustSetsockopt(t, ln, unix.SOL_SOCKET, unix.SO_SNDBUF, t0ServerSnd)
	mustSetsockopt(t, ln, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
	if err := unix.Bind(ln, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := unix.Listen(ln, 1); err != nil {
		t.Fatalf("listen: %v", err)
	}
	sa, err := unix.Getsockname(ln)
	if err != nil {
		t.Fatalf("getsockname: %v", err)
	}
	addr := sa.(*unix.SockaddrInet4)

	peer, err = unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("peer socket: %v", err)
	}
	mustSetsockopt(t, peer, unix.SOL_SOCKET, unix.SO_RCVBUF, t0PeerRcvBuf)
	mustSetsockopt(t, peer, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
	if err := unix.Connect(peer, &unix.SockaddrInet4{Addr: addr.Addr, Port: addr.Port}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	srv, _, err = unix.Accept4(ln, unix.SOCK_CLOEXEC)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	return srv, peer
}

func mustSetsockopt(t *testing.T, fd, level, opt, val int) {
	t.Helper()
	if err := unix.SetsockoptInt(fd, level, opt, val); err != nil {
		t.Fatalf("setsockopt(%d,%d,%d): %v", level, opt, val, err)
	}
}

func writeFully(t *testing.T, fd int, b []byte) {
	t.Helper()
	for len(b) > 0 {
		n, err := unix.Write(fd, b)
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		b = b[n:]
	}
}

// writeNonblock writes as much of b as the kernel takes without blocking.
func writeNonblock(fd int, b []byte) int {
	total := 0
	for len(b) > 0 {
		n, err := unix.SendmsgN(fd, b, nil, nil, unix.MSG_DONTWAIT|unix.MSG_NOSIGNAL)
		if n > 0 {
			total += n
			b = b[n:]
		}
		if err != nil {
			return total
		}
	}
	return total
}

// waitInq polls SIOCINQ on fd until it reports at least want bytes.
func waitInq(t *testing.T, fd, want int) {
	t.Helper()
	deadline := time.Now().Add(t0Timeout)
	for {
		n, err := unix.IoctlGetInt(fd, unix.SIOCINQ)
		if err != nil {
			t.Fatalf("SIOCINQ: %v", err)
		}
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d peer bytes reached the server's receive queue within %v", n, want, t0Timeout)
		}
		time.Sleep(time.Millisecond)
	}
}

// readToTerminal reads fd until it hits a terminal condition and classifies
// it exactly as the WS oracle does: ECONNRESET (RST), "EOF" (FIN), or
// "deadline" when SO_RCVTIMEO expires with the connection still open.
func readToTerminal(fd int) (kind string, bytesRead int) {
	tv := unix.NsecToTimeval(t0Timeout.Nanoseconds())
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	buf := make([]byte, 64<<10)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if n > 0 {
			bytesRead += n
			continue
		}
		switch {
		case err == nil:
			return "EOF", bytesRead
		case errors.Is(err, unix.ECONNRESET):
			return "ECONNRESET", bytesRead
		case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EWOULDBLOCK):
			return "deadline", bytesRead
		case errors.Is(err, unix.EINTR):
			continue
		default:
			return err.Error(), bytesRead
		}
	}
}

// soError returns the socket's pending SO_ERROR as an errno name ("0" when
// none). A reset that lands after the FIN was already consumed is visible
// here (tcp_reset sets EPIPE in CLOSE_WAIT) even though the reader saw EOF.
func soError(fd int) string {
	v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
	if err != nil {
		return "getsockopt:" + err.Error()
	}
	switch unix.Errno(v) {
	case 0:
		return "0"
	case unix.EPIPE:
		return "EPIPE"
	case unix.ECONNRESET:
		return "ECONNRESET"
	default:
		return unix.Errno(v).Error()
	}
}

func readIntFile(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return n
}

func kernelRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "?"
	}
	return unix.ByteSliceToString(u.Release[:])
}

// readTCPExt parses the TcpExt block of /proc/net/netstat. The file is per
// network namespace, so inside the test container the counters are
// exclusive to this process.
func readTCPExt(t *testing.T) map[string]int64 {
	t.Helper()
	f, err := os.Open("/proc/net/netstat")
	if err != nil {
		t.Fatalf("open /proc/net/netstat: %v", err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]int64{}
	sc := bufio.NewScanner(f)
	var names []string
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "TcpExt:") {
			continue
		}
		fields := strings.Fields(line)[1:]
		if names == nil {
			names = fields
			continue
		}
		for i, v := range fields {
			if i < len(names) {
				n, _ := strconv.ParseInt(v, 10, 64)
				out[names[i]] = n
			}
		}
		break
	}
	if _, ok := out["TCPAbortOnClose"]; !ok {
		t.Fatalf("TcpExt TCPAbortOnClose not found in /proc/net/netstat")
	}
	return out
}
