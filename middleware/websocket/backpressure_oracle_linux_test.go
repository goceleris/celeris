//go:build linux

package websocket

// The client and the diagnostics shared by the two real-socket backpressure
// oracles, TestBackpressurePauseDoesNotCancelInflightSend (celeris#482) and
// TestBackpressureInboundSequenceIntegrity (celeris#484).
//
// # Why the client waits on progress, not on a clock
//
// Both tests flood a server whose handler echoes everything, from clients
// that never read during the flood and hold a 32 KiB receive buffer, so the
// server's echo SEND blocks and the engine pauses inbound delivery. That is
// the point of both tests and it is kept.
//
// After the flood the client used to finish its last frame and its Close
// frame with fixed deadlines (15 s and 10 s) and wait a fixed 10 s for the
// server to close. celeris#633 measured what those deadlines judge instead of
// the engine, on GitHub's runners (kernel 6.17):
//
//   - The server is often simply behind. Under coverage the echo runs at
//     0.05-0.19x, and 457 of 459 close-timeouts were still receiving bytes
//     when the 10 s ran out. With waits that give up only when nothing moves,
//     the same connections finished: frame writes after up to 48 s, close
//     handshakes after up to 150 s.
//   - A client that never reads while it waits can stall itself for good.
//     Its receive queue stays full, so since Linux 6.17 (tcp_sequence(),
//     9ca48d616ed7) its kernel drops the server's end-of-window segments
//     BEFORE processing the ACK they carry, and it never learns the server
//     reopened its window. In 70 of 70 such give-ups the handler sat idle
//     in ReadMessage, nothing buffered, not paused, server receive queue
//     empty. Emptying the client's receive queue between write slices took
//     those give-ups from 70 to 0.
//   - The server's write and read errors the tests counted were the echo of
//     the client's own give-up: a socket closed with unread data sends an
//     RST. None of 1,165 came before its own client's close.
//
// So the client writes in wsoSlice slices, empties its receive queue
// (without blocking) after every slice that times out, and gives up only
// after wsoWriteIdle with no progress (its send queue did not shrink and
// nothing was written), or at wsoWaitCap in total. The close wait is re-armed
// by every byte received and gives up after wsoCloseIdle of silence, or at
// wsoWaitCap. A real wedge still fails, and now it fails with its state: each
// give-up prints the connection's timeline from both ends (WSO-GIVEUP).
//
// # An engine that stops reading, which the drain would hide
//
// The drain has a cost. celeris#607 was a recv the engine chained behind a
// send (IOSQE_IO_LINK) on a connection whose client had stopped reading: the
// send waited on the client's closed window, so the connection could not
// read at all. The drain is exactly what reopens that window, so a client
// that drains gets through such a connection and never gives up.
//
// So the rig also watches the server side of every connection, every
// wsoWatchEvery, for its whole life (watch): a connection whose server
// socket holds unread bytes while nothing asked the engine to stop reading
// it (the chanReader is not paused and holds nothing, the handler has not
// exited) is one the engine should be reading. If the engine reads nothing
// from it for wsoRecvStall, and the process was not starved of CPU meanwhile,
// the test fails with that stretch's state at both ends (WSO-STALL), whatever
// the client does afterwards. That is the celeris#607 class judged by what it
// does, not by the mechanism: a chained recv and a recv arm passed over
// behind an outstanding send look the same here.
//
// # The test binary's deadline
//
// A wedge costs wsoWriteIdle or more per subtest, and CI runs both oracles
// in one binary under -timeout. So a rig takes a budget from t.Deadline():
// half of what is left after wsoReserve, which is kept for the verdicts, the
// engine shutdowns and the tests after it. A wait that runs into the budget
// gives up with reason "budget" and its WSO-GIVEUP record, so a run that
// would have timed out ends with each subtest's own verdict instead of a
// goroutine dump.
//
// # What the timeline shows
//
// The client joins itself to the handler that serves it by sending its local
// port as ?cid= and the handler reading it back (Conn.Query). Every sample
// reads, for that connection:
//
//   - the client's socket: send and receive queue, the windows it knows,
//     RTO backoff;
//   - the handler: where it is (ReadMessage, WriteMessage, exited), frames
//     echoed, time since the last echo;
//   - the chanReader: depth, spill, whether it holds the engine paused, and
//     how many pauses and resumes it applied (the callbacks are wrapped);
//   - the server's socket, read through the engine's own descriptor: bytes
//     received and still unread (so, what the engine has read), bytes the
//     kernel has accepted from the engine and how many the peer ACKed, the
//     windows, retransmissions;
//   - derived from those: the bytes the engine has read, and the bytes the
//     handler wrote that the engine has not yet handed to the kernel. Both
//     count from the connection's first byte, net of the upgrade request and
//     the 101 response, whose sizes the client reads from its own socket
//     once the handshake is done.
//
// That is the engine's recv and send progress, its resumes and its pending
// write bytes, observed from outside it, the same way on epoll and io_uring.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// wsoSlice is one write slice, and so how often a waiting client
	// empties its receive queue and records a timeline sample.
	wsoSlice = time.Second
	// wsoWriteIdle: a write gives up after this long without progress.
	// The longest healthy gap measured was 50.6 s (coverage, arm64 io_uring).
	wsoWriteIdle = 90 * time.Second
	// wsoCloseIdle: the close wait gives up after this long without a byte.
	wsoCloseIdle = 30 * time.Second
	// wsoWaitCap bounds any one wait, progress or not.
	wsoWaitCap = 240 * time.Second
	// wsoCloseSlow: a close wait longer than this is reported. It is the old
	// absolute deadline, so these are the waits the old oracle failed.
	wsoCloseSlow = 10 * time.Second
	// wsoFirstSamples and wsoLastSamples: the timeline samples kept per
	// connection, the first ones (the onset) and the most recent ones.
	wsoFirstSamples = 8
	wsoLastSamples  = 24
	// wsoWatchEvery: how often watch samples the server side of every
	// connection.
	wsoWatchEvery = 250 * time.Millisecond
	// wsoWatchGap: a watcher run this long after its tick was due says the
	// process was starved of CPU, and so perhaps the engine too: every open
	// stretch starts over (see watch).
	wsoWatchGap = time.Second
	// wsoRecvStall: a connection whose engine read nothing for this long
	// from a socket holding unread bytes, while nothing asked it to stop,
	// fails the test. celeris#607 held such a connection for up to 14 s.
	// A healthy engine's longest such stretch, measured on GitHub's runners
	// in CI's shape, was 2.3 s on epoll and 1.7 s on io_uring.
	wsoRecvStall = 5 * time.Second
	// wsoQuiet: after its flood the #482 test's client neither reads nor
	// writes for this long, holding the backpressure it built. A healthy
	// engine reads what the socket holds, or pauses; one that stopped
	// reading the connection behind its blocked echo (celeris#607) cannot,
	// so that stretch outlasts wsoRecvStall. With #607 re-introduced such a
	// stretch began up to 1.6 s after the flood ended; at 6 s of quiet the
	// shortest reached 4.5 s.
	wsoQuiet = 8 * time.Second
	// wsoReserve: what a rig leaves of the test binary's -timeout for the
	// verdicts, the engine shutdowns (up to 40 s) and the tests after it.
	wsoReserve = 60 * time.Second
)

// wsoRig joins each client connection to the handler that serves it and
// keeps what each side saw.
type wsoRig struct {
	t0       time.Time
	end      time.Time    // the wait budget from t.Deadline(); zero: none
	srvPort  atomic.Int32 // set once the server listens; read by handler goroutines
	srvs     sync.Map     // client local port -> *wsoSrv
	clisBy   sync.Map     // client local port -> *wsoCli
	mu       sync.Mutex
	clis     []*wsoCli
	hs       []*wsoSrv     // every handler, joined or not
	stallMax time.Duration // the longest stretch watch saw, set when it stops
	longest  wsoStall      // that stretch (see watch)
	// watchResets and watchMaxLag: how often, and for how long at most, the
	// watcher was not run when its tick was due (see watch).
	watchResets int
	watchMaxLag time.Duration
}

// newWSORig starts a subtest's rig. Its waits share one budget: half of the
// time the test binary has left after wsoReserve (see the file comment).
func newWSORig(t *testing.T) *wsoRig {
	g := &wsoRig{t0: time.Now()}
	if dl, ok := t.Deadline(); ok {
		g.end = g.t0.Add(max(time.Until(dl)-wsoReserve, 0) / 2)
	}
	return g
}

// overBudget reports whether now is past the rig's wait budget.
func (g *wsoRig) overBudget(now time.Time) bool { return !g.end.IsZero() && !now.Before(g.end) }

// budgetNote describes the budget for a verdict message.
func (g *wsoRig) budgetNote() string {
	if g.end.IsZero() {
		return "no -timeout budget"
	}
	return "the -timeout budget ended at +" + wsoSec(int64(g.end.Sub(g.t0)))
}

// since is the rig's clock: nanoseconds since the subtest started.
func (g *wsoRig) since() int64 { return int64(time.Since(g.t0)) }

func wsoSec(ns int64) string { return strconv.FormatFloat(float64(ns)/1e9, 'f', 3, 64) + "s" }

// setServer records the server's port from its ws:// address.
func (g *wsoRig) setServer(hostPort string) {
	if i := strings.LastIndexByte(hostPort, ':'); i >= 0 {
		p, _ := strconv.Atoi(hostPort[i+1:])
		g.srvPort.Store(int32(p))
	}
}

// ---------------------------------------------------------------- server side

type wsoSrvErr struct {
	kind string // "write" or "read"
	err  error
	ns   int64
}

// wsoSrv is one handler's view of its connection.
type wsoSrv struct {
	g                 *wsoRig
	port              int // the client's local port, or -1 when the client sent none
	r                 *chanReader
	frames            atomic.Int64
	wroteB            atomic.Int64 // bytes of every frame WriteMessage accepted
	phase             atomic.Int32 // 1 ReadMessage, 2 WriteMessage, 3 exited
	echoNs            atomic.Int64 // rig time of the last echo
	pauses, resumes   atomic.Int64
	pauseNs, resumeNs atomic.Int64

	mu     sync.Mutex
	exit   string
	exitNs int64
	errs   []wsoSrvErr
	fd     int
	ino    uint64
	gone   bool // the socket was found once and its descriptor no longer is it
}

// attach registers the handler's connection under the client's port and
// wraps the chanReader's pause and resume callbacks so each one applied to
// the engine is counted and timed. Call it first thing in the handler, on
// the handler goroutine: Read reads r.resume without the lock on that
// goroutine, and requestPause reads both under pausedMu, which is held here.
func (g *wsoRig) attach(c *Conn) *wsoSrv {
	s := &wsoSrv{g: g, port: -1, r: c.engineReader, fd: -1}
	s.phase.Store(1)
	if p, err := strconv.Atoi(c.Query("cid")); err == nil && p > 0 {
		s.port = p
		g.srvs.Store(p, s)
	}
	g.mu.Lock()
	g.hs = append(g.hs, s)
	g.mu.Unlock()
	if r := s.r; r != nil {
		r.pausedMu.Lock()
		if pause, resume := r.pause, r.resume; pause != nil && resume != nil {
			r.pause = func() {
				s.pauses.Add(1)
				s.pauseNs.Store(g.since())
				pause()
			}
			r.resume = func() {
				s.resumes.Add(1)
				s.resumeNs.Store(g.since())
				resume()
			}
		}
		r.pausedMu.Unlock()
	}
	g.srvSockFD(s) // find the engine's descriptor while the connection is young
	return s
}

// echoed records a frame the handler echoed.
func (s *wsoSrv) echoed(payload int) {
	s.frames.Add(1)
	s.wroteB.Add(int64(wsoFrameLen(payload)))
	s.echoNs.Store(s.g.since())
}

// wsoFrameLen is the size on the wire of an unmasked server frame.
func wsoFrameLen(payload int) int {
	switch {
	case payload < 126:
		return 2 + payload
	case payload < 1<<16:
		return 4 + payload
	}
	return 10 + payload
}

// noteErr records a server-side error with its time, to be judged against
// the time its own client closed (see serverErrs).
func (s *wsoSrv) noteErr(kind string, err error) {
	s.mu.Lock()
	s.errs = append(s.errs, wsoSrvErr{kind: kind, err: err, ns: s.g.since()})
	s.mu.Unlock()
}

// exited records how the handler's loop ended.
func (s *wsoSrv) exited(what string, err error) {
	s.mu.Lock()
	if s.exit == "" {
		s.exit, s.exitNs = what+": "+wsoErrStr(err), s.g.since()
	}
	s.mu.Unlock()
	s.phase.Store(3)
}

func wsoErrStr(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

// ------------------------------------------------------------ kernel sockets

// wsoProcTCP finds the /proc/net/tcp row of the IPv4 socket with local port
// lport and remote port rport: a compact rendering and its inode.
func wsoProcTCP(lport, rport int) (string, uint64) {
	b, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return "proc-err", 0
	}
	wl, wr := fmt.Sprintf(":%04X", lport), fmt.Sprintf(":%04X", rport)
	lines := strings.Split(string(b), "\n")
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) < 10 || !strings.HasSuffix(f[1], wl) || !strings.HasSuffix(f[2], wr) {
			continue
		}
		q := strings.SplitN(f[4], ":", 2)
		tx, rx := int64(-1), int64(-1)
		if len(q) == 2 {
			tx, _ = strconv.ParseInt(q[0], 16, 64)
			rx, _ = strconv.ParseInt(q[1], 16, 64)
		}
		ino, _ := strconv.ParseUint(f[9], 10, 64)
		return fmt.Sprintf("st=%s tx=%d rx=%d tm=%s retr=%s", f[3], tx, rx, f[5], f[6]), ino
	}
	return "absent", 0
}

// wsoFDByInode finds this process's descriptor for socket inode ino.
func wsoFDByInode(ino uint64) int {
	if ino == 0 {
		return -1
	}
	want := "socket:[" + strconv.FormatUint(ino, 10) + "]"
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	for _, e := range ents {
		if l, err := os.Readlink("/proc/self/fd/" + e.Name()); err == nil && l == want {
			fd, _ := strconv.Atoi(e.Name())
			return fd
		}
	}
	return -1
}

// srvSockFD returns the engine's descriptor for this connection's server
// socket. It is found once through /proc and re-checked with fstat before
// every use, so a descriptor the engine closed and the kernel handed to
// another socket is not read (short of a reuse between the fstat and the
// read, which would only misreport a diagnostic). Once that check fails the
// socket is gone for good and is not searched for again (watch asks every
// wsoWatchEvery). Only read-only calls (fstat, getsockopt TCP_INFO, ioctl
// SIOCINQ/SIOCOUTQ) are made on it.
func (g *wsoRig) srvSockFD(s *wsoSrv) int {
	srvPort := int(g.srvPort.Load())
	if s.port <= 0 || srvPort <= 0 {
		return -1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone {
		return -1
	}
	if s.fd >= 0 {
		var st unix.Stat_t
		if unix.Fstat(s.fd, &st) == nil && st.Ino == s.ino {
			return s.fd
		}
		s.fd, s.gone = -1, true
		return -1
	}
	_, ino := wsoProcTCP(srvPort, s.port)
	if fd := wsoFDByInode(ino); fd >= 0 {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil && st.Ino == ino {
			s.fd, s.ino = fd, ino
			return fd
		}
	}
	return -1
}

func wsoSockInfoFD(fd int) (ti *unix.TCPInfo, inq, outq int, ok bool) {
	inq, outq = -1, -1
	v, err := unix.GetsockoptTCPInfo(fd, unix.SOL_TCP, unix.TCP_INFO)
	if err != nil {
		return nil, inq, outq, false
	}
	if n, err := unix.IoctlGetInt(fd, unix.SIOCINQ); err == nil {
		inq = n
	}
	if n, err := unix.IoctlGetInt(fd, unix.SIOCOUTQ); err == nil {
		outq = n
	}
	return v, inq, outq, true
}

func (g *wsoRig) srvSockInfo(s *wsoSrv) (*unix.TCPInfo, int, int, bool) {
	fd := g.srvSockFD(s)
	if fd < 0 {
		return nil, -1, -1, false
	}
	return wsoSockInfoFD(fd)
}

// wsoCliSockInfo reads the client socket's TCP_INFO, receive queue (SIOCINQ)
// and send queue (SIOCOUTQ: bytes the server's kernel has not ACKed).
func wsoCliSockInfo(c net.Conn) (ti *unix.TCPInfo, inq, outq int) {
	inq, outq = -1, -1
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return nil, inq, outq
	}
	sc, err := tc.SyscallConn()
	if err != nil {
		return nil, inq, outq
	}
	_ = sc.Control(func(fd uintptr) {
		ti, inq, outq, _ = wsoSockInfoFD(int(fd))
	})
	return ti, inq, outq
}

// wsoDrainNow empties the client's receive queue without blocking. It
// returns the bytes read and how the read ended: nil when the queue was
// empty, io.EOF when the server had closed its side, or the socket's error.
// A raw read clears the socket's pending error (a reset reads as
// ECONNRESET once and then no more), so it is returned here, where the
// caller can still name it.
func wsoDrainNow(c net.Conn, buf []byte) (int64, error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return 0, nil
	}
	sc, err := tc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var total int64
	var end error
	if err := sc.Read(func(fd uintptr) bool {
		for {
			n, e := unix.Read(int(fd), buf)
			switch {
			case n > 0:
				total += int64(n)
				continue
			case e == unix.EINTR:
				continue
			case e == nil:
				end = io.EOF
			case e != unix.EAGAIN:
				end = os.NewSyscallError("read", e)
			}
			return true // empty, EOF or an error: stop, never wait
		}
	}); err != nil && end == nil {
		end = err
	}
	return total, end
}

func wsoTCPShort(ti *unix.TCPInfo) string {
	if ti == nil {
		return "tcpinfo=nil"
	}
	return fmt.Sprintf("wnd=%d/%d unacked=%d notsent=%d backoff=%d probes=%d retr=%d ackAgo=%dms",
		ti.Snd_wnd, ti.Rcv_wnd, ti.Unacked, ti.Notsent_bytes, ti.Backoff, ti.Probes, ti.Total_retrans, ti.Last_ack_recv)
}

func wsoTCPFull(ti *unix.TCPInfo) string {
	if ti == nil {
		return "tcpinfo=nil"
	}
	return fmt.Sprintf("state=%d snd_wnd=%d rcv_wnd=%d rcv_space=%d rcv_ssthresh=%d snd_mss=%d rcv_mss=%d "+
		"unacked=%d notsent=%d probes=%d backoff=%d rto_ms=%d retrans=%d total_retrans=%d "+
		"bytes_sent=%d bytes_acked=%d bytes_received=%d data_segs_out=%d data_segs_in=%d "+
		"last_data_sent_ms=%d last_data_recv_ms=%d last_ack_recv_ms=%d rwnd_limited_ms=%d sndbuf_limited_ms=%d",
		ti.State, ti.Snd_wnd, ti.Rcv_wnd, ti.Rcv_space, ti.Rcv_ssthresh, ti.Snd_mss, ti.Rcv_mss,
		ti.Unacked, ti.Notsent_bytes, ti.Probes, ti.Backoff, ti.Rto/1000, ti.Retrans, ti.Total_retrans,
		ti.Bytes_sent, ti.Bytes_acked, ti.Bytes_received, ti.Data_segs_out, ti.Data_segs_in,
		ti.Last_data_sent, ti.Last_data_recv, ti.Last_ack_recv, ti.Rwnd_limited/1000, ti.Sndbuf_limited/1000)
}

// ------------------------------------------------------------ server snapshot

// wsoSrvView is one reading of a connection's server side.
type wsoSrvView struct {
	found                 bool
	phase                 int32
	frames                int64
	echoAgo               int64
	depth                 int
	spill                 int64
	paused, closed        bool
	pauses, resumes       int64
	pauseAgo, resumeAgo   int64
	exit                  string
	sockOK                bool
	ti                    *unix.TCPInfo
	inq, outq             int
	rawRead               int64 // bytes the engine has read from the socket, the upgrade request included
	engRead, pendingWrite int64
}

func (g *wsoRig) srvView(port int) wsoSrvView {
	x, ok := g.srvs.Load(port)
	if !ok {
		return wsoSrvView{}
	}
	return g.srvViewOf(x.(*wsoSrv))
}

func (g *wsoRig) srvViewOf(s *wsoSrv) wsoSrvView {
	var v wsoSrvView
	now := g.since()
	v.found = true
	v.phase = s.phase.Load()
	v.frames = s.frames.Load()
	v.echoAgo = -1
	if e := s.echoNs.Load(); e > 0 {
		v.echoAgo = now - e
	}
	v.pauses, v.resumes = s.pauses.Load(), s.resumes.Load()
	v.pauseAgo, v.resumeAgo = -1, -1
	if p := s.pauseNs.Load(); p > 0 {
		v.pauseAgo = now - p
	}
	if p := s.resumeNs.Load(); p > 0 {
		v.resumeAgo = now - p
	}
	v.depth, v.spill = -1, -1
	if r := s.r; r != nil {
		v.depth = len(r.ch)
		v.spill = r.spillLen.Load()
		r.pausedMu.Lock()
		v.paused = r.pausedState
		r.pausedMu.Unlock()
		v.closed = r.closed.Load()
	}
	wrote := s.wroteB.Load()
	v.ti, v.inq, v.outq, v.sockOK = g.srvSockInfo(s)
	s.mu.Lock()
	v.exit = s.exit
	s.mu.Unlock()
	v.rawRead, v.engRead, v.pendingWrite = -1, -1, -1
	if !v.sockOK {
		return v
	}
	// The server socket counts from its first payload byte (a passive open
	// starts snd_una and rcv_nxt past the SYN), so bytes received minus
	// unread is everything the engine has read, and bytes ACKed plus the
	// send queue is everything it has handed to the kernel. Net of the
	// upgrade request and the 101, both are the handler's frames.
	v.rawRead = int64(v.ti.Bytes_received) - int64(v.inq)
	if x, ok := g.clisBy.Load(s.port); ok {
		cl := x.(*wsoCli)
		if reqB, respB := cl.hsOut.Load(), cl.hsIn.Load(); reqB >= 0 && respB >= 0 {
			v.engRead = v.rawRead - reqB
			v.pendingWrite = wrote + respB - (int64(v.ti.Bytes_acked) + int64(v.outq))
		}
	}
	return v
}

func wsoAgo(ns int64) string {
	if ns < 0 {
		return "never"
	}
	return wsoSec(ns)
}

func (v wsoSrvView) String() string {
	if !v.found {
		return "srv{unjoined}"
	}
	ph := map[int32]string{1: "read", 2: "write", 3: "exited"}[v.phase]
	s := fmt.Sprintf("srv{%s frames=%d echoAgo=%s depth=%d spill=%d paused=%t closed=%t pauses=%d resumes=%d pauseAgo=%s resumeAgo=%s",
		ph, v.frames, wsoAgo(v.echoAgo), v.depth, v.spill, v.paused, v.closed, v.pauses, v.resumes, wsoAgo(v.pauseAgo), wsoAgo(v.resumeAgo))
	if v.exit != "" {
		s += " exit=" + strconv.Quote(v.exit)
	}
	s += "}"
	if !v.sockOK {
		return s + " sock{gone}"
	}
	return s + fmt.Sprintf(" sock{inq=%d outq=%d %s} eng{read=%d pendingWrite=%d}",
		v.inq, v.outq, wsoTCPShort(v.ti), v.engRead, v.pendingWrite)
}

// wsoShape names the state a give-up was in, from both ends. It is a hint for
// the reader; the timeline and the raw state are printed with it.
func wsoShape(cli *unix.TCPInfo, cliInq int, v wsoSrvView) string {
	switch {
	case !v.found:
		return "unjoined"
	case v.phase == 3:
		return "handler-exited"
	case v.paused && v.depth == 0 && v.spill > 0:
		return "WEDGE: engine paused, channel empty, chunks in the spill (celeris#705 shape)"
	case v.paused && v.depth == 0:
		return "WEDGE: engine paused with nothing buffered (celeris#672 shape)"
	case v.sockOK && v.inq > 0 && !v.paused && v.phase == 1 && v.depth == 0:
		return "WEDGE: socket readable, engine not paused, nothing delivered (recv not armed or not completing)"
	case v.sockOK && v.pendingWrite > 0 && v.outq == 0:
		return "WEDGE: handler bytes the engine never handed to the kernel (send not submitted or not completing)"
	case cli != nil && cli.Snd_wnd == 0 && cliInq > 0 && v.sockOK && v.inq == 0 && v.depth == 0 && !v.paused:
		return "lost window update: the client's full receive queue discards the server's ACKs (Linux >= 6.17 tcp_sequence)"
	case v.paused || v.depth > 0:
		return "server behind: handler has buffered input to echo"
	}
	return "unclassified"
}

// ------------------------------------------------------------- client side

// wsoCli is one client connection's record: milestones, the first timeline
// samples and a ring of the most recent ones.
type wsoCli struct {
	g    *wsoRig
	port int
	c    net.Conn
	// hsOut and hsIn: the upgrade request's and the 101 response's sizes,
	// read from the client's socket once the handshake is done; -1 before.
	hsOut, hsIn atomic.Int64
	mu          sync.Mutex
	marks       []string
	first       []string
	last        [wsoLastSamples]string
	nSamp       int
	closeNs     int64 // rig time the client closed its socket; 0 while open
	gaveUp      bool
	close       wsoClose
	fc, cw      wsoWrite
}

// client registers a dialed connection. Close it with closeClient, which
// records when the client let go (see serverErrs).
func (g *wsoRig) client(c net.Conn) *wsoCli {
	cl := &wsoCli{g: g, c: c}
	cl.hsOut.Store(-1)
	cl.hsIn.Store(-1)
	if a, ok := c.LocalAddr().(*net.TCPAddr); ok {
		cl.port = a.Port
		g.clisBy.Store(a.Port, cl)
	}
	g.mu.Lock()
	g.clis = append(g.clis, cl)
	g.mu.Unlock()
	return cl
}

// handshake upgrades the connection with the join key in its path, and then
// records the upgrade's size in each direction from the client's socket: the
// server sends nothing past its 101 until frames arrive, and it ACKed the
// whole request with that 101, so here bytes received are the 101 and bytes
// ACKed are the request.
func (cl *wsoCli) handshake(c net.Conn, hostPort string) error {
	if err := wsHandshakePath(c, hostPort, cl.path()); err != nil {
		return err
	}
	if ti, _, _ := wsoCliSockInfo(c); ti != nil {
		cl.hsOut.Store(int64(ti.Bytes_acked))
		cl.hsIn.Store(int64(ti.Bytes_received))
	}
	return nil
}

func (g *wsoRig) closeClient(cl *wsoCli, c net.Conn) {
	cl.mu.Lock()
	cl.closeNs = g.since()
	cl.mu.Unlock()
	_ = c.Close()
}

// path is the upgrade path that carries the join key.
func (cl *wsoCli) path() string { return "/ws?cid=" + strconv.Itoa(cl.port) }

func (cl *wsoCli) mark(format string, args ...any) {
	m := "+" + wsoSec(cl.g.since()) + " " + fmt.Sprintf(format, args...)
	cl.mu.Lock()
	cl.marks = append(cl.marks, m)
	cl.mu.Unlock()
}

// sample appends one timeline sample: both ends of the connection, now.
func (cl *wsoCli) sample(c net.Conn, label string) {
	ti, inq, outq := wsoCliSockInfo(c)
	line := fmt.Sprintf("+%s %s cli{inq=%d outq=%d %s} %s", wsoSec(cl.g.since()), label, inq, outq, wsoTCPShort(ti), cl.g.srvView(cl.port))
	cl.mu.Lock()
	if len(cl.first) < wsoFirstSamples {
		cl.first = append(cl.first, line)
	} else {
		cl.last[(cl.nSamp-wsoFirstSamples)%wsoLastSamples] = line
	}
	cl.nSamp++
	cl.mu.Unlock()
}

// wsoWrite is how one bounded client write went.
type wsoWrite struct {
	site      string
	ok        bool
	need      int
	left      int
	err       error
	reason    string // "" (completed), "noprog", "cap", "budget", "rst", "eof", "err"
	dur       time.Duration
	slices    int
	progress  int
	maxNoProg time.Duration
	drained   int64
	outq0     int
	outq1     int
}

func (w wsoWrite) String() string {
	return fmt.Sprintf("%s: ok=%t left=%d/%d reason=%q err=%q dur=%s slices=%d progress=%d maxNoProg=%s drained=%d outq %d->%d",
		w.site, w.ok, w.left, w.need, w.reason, wsoErrStr(w.err), w.dur.Round(time.Millisecond), w.slices, w.progress,
		w.maxNoProg.Round(time.Millisecond), w.drained, w.outq0, w.outq1)
}

// write writes every byte of buf, waiting on progress: 1 s slices; after
// each slice that times out, a timeline sample and a non-blocking drain of
// the client's receive queue. Progress is a byte written or a shrink of the
// client's send queue (the server's kernel ACKed: the engine is reading).
// It gives up after wsoWriteIdle without progress, at wsoWaitCap in total,
// at the rig's budget, on any error other than the slice's own deadline, or
// when a drain finds the connection reset ("rst") or closed ("eof").
func (cl *wsoCli) write(c net.Conn, site string, buf, dbuf []byte) wsoWrite {
	w := wsoWrite{site: site, need: len(buf)}
	// The drain below goes through the conn's poller, which refuses at once
	// under an expired read deadline, and the caller's read loop leaves one
	// behind: clear it, or every drain is a no-op.
	_ = c.SetReadDeadline(time.Time{})
	start := time.Now()
	lastProg := start
	_, _, w.outq0 = wsoCliSockInfo(c)
	lastQ := w.outq0
	for len(buf) > 0 {
		_ = c.SetWriteDeadline(time.Now().Add(wsoSlice))
		n, err := c.Write(buf)
		buf = buf[n:]
		w.slices++
		if len(buf) == 0 {
			break
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			w.err, w.reason = err, "err"
			if errors.Is(err, syscall.ECONNRESET) {
				w.reason = "rst"
			}
			break
		}
		now := time.Now()
		_, _, q := wsoCliSockInfo(c)
		if n > 0 || (q >= 0 && lastQ >= 0 && q < lastQ) {
			if gap := now.Sub(lastProg); gap > w.maxNoProg {
				w.maxNoProg = gap
			}
			lastProg = now
			w.progress++
		}
		lastQ = q
		cl.sample(c, site+" slice "+strconv.Itoa(w.slices))
		if now.Sub(lastProg) >= wsoWriteIdle {
			w.err, w.reason = err, "noprog"
			break
		}
		if now.Sub(start) >= wsoWaitCap {
			w.err, w.reason = err, "cap"
			break
		}
		if cl.g.overBudget(now) {
			w.err, w.reason = err, "budget"
			break
		}
		n2, derr := wsoDrainNow(c, dbuf)
		w.drained += n2
		if derr != nil {
			w.err, w.reason = derr, "err"
			switch {
			case errors.Is(derr, syscall.ECONNRESET):
				w.reason = "rst"
			case errors.Is(derr, io.EOF):
				w.reason = "eof"
			}
			break
		}
	}
	_ = c.SetWriteDeadline(time.Time{})
	w.left = len(buf)
	w.ok = w.left == 0
	w.dur = time.Since(start)
	if gap := time.Since(lastProg); !w.ok && gap > w.maxNoProg {
		w.maxNoProg = gap
	}
	_, _, w.outq1 = wsoCliSockInfo(c)
	cl.mu.Lock()
	if site == "fc" {
		cl.fc = w
	} else {
		cl.cw = w
	}
	cl.mu.Unlock()
	return w
}

// wsoClose is how the wait for the server's close went.
type wsoClose struct {
	outcome  string // "eof", "rst", "idle", "cap", "budget", "err"
	err      error
	dur      time.Duration // Close sent -> outcome
	bytes    int64         // received during the wait
	lastByte time.Duration // Close sent -> last byte received
	maxGap   time.Duration // the longest stretch with no byte, up to the outcome
}

func (r wsoClose) String() string {
	return fmt.Sprintf("close wait: outcome=%s err=%q dur=%s bytes=%d lastByte=%s maxGap=%s",
		r.outcome, wsoErrStr(r.err), r.dur.Round(time.Millisecond), r.bytes, r.lastByte.Round(time.Millisecond), r.maxGap.Round(time.Millisecond))
}

// closeWait reads until the server closes. The give-up is re-armed by every
// byte received: it comes after wsoCloseIdle without one, at wsoWaitCap, or
// at the rig's budget. While it waits in silence it records a timeline
// sample every wsoSlice.
func (cl *wsoCli) closeWait(c net.Conn, buf []byte) wsoClose {
	var r wsoClose
	start := time.Now()
	last := start
	for {
		dl := time.Now().Add(wsoSlice)
		if lim := last.Add(wsoCloseIdle); dl.After(lim) {
			dl = lim
		}
		if lim := start.Add(wsoWaitCap); dl.After(lim) {
			dl = lim
		}
		if lim := cl.g.end; !lim.IsZero() && dl.After(lim) {
			dl = lim
		}
		_ = c.SetReadDeadline(dl)
		n, err := c.Read(buf)
		now := time.Now()
		if n > 0 {
			if gap := now.Sub(last); gap > r.maxGap {
				r.maxGap = gap
			}
			last = now
			r.bytes += int64(n)
		}
		if err == nil {
			continue
		}
		switch {
		case errors.Is(err, io.EOF):
			r.outcome = "eof"
		case errors.Is(err, syscall.ECONNRESET):
			r.outcome = "rst"
		case errors.Is(err, os.ErrDeadlineExceeded):
			switch {
			case now.Sub(start) >= wsoWaitCap:
				r.outcome = "cap"
			case now.Sub(last) >= wsoCloseIdle:
				r.outcome = "idle"
			case cl.g.overBudget(now):
				r.outcome = "budget"
			default:
				cl.sample(c, "close wait")
				continue
			}
		default:
			r.outcome = "err"
		}
		r.err = err
		if gap := now.Sub(last); gap > r.maxGap {
			r.maxGap = gap
		}
		r.dur = now.Sub(start)
		r.lastByte = last.Sub(start)
		cl.mu.Lock()
		cl.close = r
		cl.mu.Unlock()
		return r
	}
}

// giveUp prints one WSO-GIVEUP record: the connection's milestones, its
// timeline, and the full state of both ends now, with the shape it is in.
// One t.Logf call, so records from concurrent clients never interleave, and
// printed at once, so it survives a test binary that later times out.
func (cl *wsoCli) giveUp(t *testing.T, c net.Conn, kind, what string) {
	cl.mu.Lock()
	cl.gaveUp = true
	cl.mu.Unlock()
	ti, inq, outq := wsoCliSockInfo(c)
	v := cl.g.srvView(cl.port)
	srvPort := int(cl.g.srvPort.Load())
	srvRow, _ := wsoProcTCP(srvPort, cl.port)
	cliRow, _ := wsoProcTCP(cl.port, srvPort)
	var b strings.Builder
	fmt.Fprintf(&b, "WSO-GIVEUP %s conn=127.0.0.1:%d %s\n", kind, cl.port, what)
	fmt.Fprintf(&b, "  shape: %s\n", wsoShape(ti, inq, v))
	cl.mu.Lock()
	fmt.Fprintf(&b, "  milestones: %s\n", strings.Join(cl.marks, "; "))
	n := cl.nSamp
	fmt.Fprintf(&b, "  timeline (%d samples, one per second spent waiting; the first %d and the last %d are kept):\n",
		n, wsoFirstSamples, wsoLastSamples)
	for _, l := range cl.first {
		fmt.Fprintf(&b, "    %s\n", l)
	}
	from := wsoFirstSamples
	if n-wsoLastSamples > from {
		from = n - wsoLastSamples
		fmt.Fprintf(&b, "    ... %d samples not kept ...\n", from-wsoFirstSamples)
	}
	for i := from; i < n; i++ {
		fmt.Fprintf(&b, "    %s\n", cl.last[(i-wsoFirstSamples)%wsoLastSamples])
	}
	cl.mu.Unlock()
	fmt.Fprintf(&b, "  now: cli{inq=%d outq=%d %s} /proc{%s}\n", inq, outq, wsoTCPFull(ti), cliRow)
	fmt.Fprintf(&b, "  now: %s /proc{%s}", v, srvRow)
	if v.ti != nil {
		fmt.Fprintf(&b, "\n  now: srv tcpinfo{%s}", wsoTCPFull(v.ti))
	}
	t.Log(b.String())
}

// --------------------------------------------------------------- the verdicts

// wsoJudged is one server-side error, rendered with its timing against its
// own client's close.
type wsoJudged struct {
	kind string // "write" or "read"
	line string
}

// serverErrs splits every server-side error into the judged and the
// excused. An error is excused only when its own client gave up on the
// connection or was reset (giveUp), which fails the test on its own, and the
// error came after that client closed its socket: then it is the client's
// teardown echoing back (a socket closed with unread data sends an RST;
// celeris#633: none of 1,165 such errors came before their client's close).
// Every other error is judged, whenever it came: on a connection whose
// client read the server's close (EOF), the client's receive queue was
// empty and its close sent a FIN, so nothing it did can explain a server
// error; and a handler notes an engine error only when its next read or
// write returns, so the time it was noted says nothing about its cause. An
// error whose connection the rig could not join is judged.
func (g *wsoRig) serverErrs() (judged, excused []wsoJudged) {
	type end struct {
		closeNs int64
		gaveUp  bool
	}
	ends := map[int]end{}
	g.mu.Lock()
	for _, cl := range g.clis {
		cl.mu.Lock()
		ends[cl.port] = end{cl.closeNs, cl.gaveUp}
		cl.mu.Unlock()
	}
	hs := append([]*wsoSrv(nil), g.hs...)
	g.mu.Unlock()
	for _, s := range hs {
		s.mu.Lock()
		errs := append([]wsoSrvErr(nil), s.errs...)
		s.mu.Unlock()
		for _, e := range errs {
			en, ok := ends[s.port]
			line := fmt.Sprintf("conn 127.0.0.1:%d %s error %q at %s", s.port, e.kind, wsoErrStr(e.err), wsoSec(e.ns))
			switch {
			case ok && en.gaveUp && en.closeNs > 0 && e.ns >= en.closeNs:
				excused = append(excused, wsoJudged{e.kind, line + fmt.Sprintf(", %s after its client gave up and closed", wsoSec(e.ns-en.closeNs))})
				continue
			case !ok:
				line += ", its connection was never joined"
			case en.closeNs == 0:
				line += ", its client had not closed"
			case e.ns < en.closeNs:
				line += fmt.Sprintf(", %s BEFORE its client closed", wsoSec(en.closeNs-e.ns))
			default:
				line += fmt.Sprintf(", %s after its client closed on the server's close (EOF)", wsoSec(e.ns-en.closeNs))
			}
			judged = append(judged, wsoJudged{e.kind, line})
		}
	}
	sort.Slice(judged, func(i, j int) bool { return judged[i].line < judged[j].line })
	sort.Slice(excused, func(i, j int) bool { return excused[i].line < excused[j].line })
	return judged, excused
}

// ------------------------------------------------------------------ the watch

// wsoStall is one stretch in which a connection's engine read nothing from
// a server socket holding unread bytes while nothing asked it to stop.
type wsoStall struct {
	port          int
	fromNs, toNs  int64 // rig time of the stretch's first and last sample
	samples       int
	onset, last   string // the server side at those samples
	onsetC, lastC string // the client side at those samples
	marks         string // the client's milestones at the last sample
	open          bool   // still going when the watch stopped
}

func (st *wsoStall) String() string {
	still := ""
	if st.open {
		still = ", still going when the watch stopped"
	}
	return fmt.Sprintf("WSO-STALL conn=127.0.0.1:%d the engine read nothing for %s (%d samples, +%s to +%s%s) "+
		"from a server socket holding unread bytes, while the chanReader was neither paused nor holding "+
		"anything and the handler had not exited\n  onset: %s\n         %s\n  last:  %s\n         %s\n  client milestones: %s",
		st.port, wsoSec(st.toNs-st.fromNs), st.samples, wsoSec(st.fromNs), wsoSec(st.toNs), still,
		st.onset, st.onsetC, st.last, st.lastC, st.marks)
}

// longestStretch renders the longest stretch watch saw, for the verdict
// line's neighbour: how close a run came to wsoRecvStall.
func (g *wsoRig) longestStretch() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.longest.samples == 0 {
		return "longest unread stretch: none"
	}
	st := g.longest
	return fmt.Sprintf("longest unread stretch (limit %v): %s", wsoRecvStall, strings.TrimPrefix(st.String(), "WSO-STALL "))
}

// wsoStallable reports whether the engine should be reading a connection
// now: its server socket holds unread bytes, and nothing asked the engine to
// stop (the chanReader is not paused, is not closed and holds nothing), and
// the handler has not exited.
func wsoStallable(v wsoSrvView) bool {
	return v.found && v.sockOK && v.inq > 0 && !v.paused && !v.closed && v.depth == 0 && v.spill == 0 && v.phase != 3
}

// cliNow renders a joined connection's client side now, for a WSO-STALL.
func (g *wsoRig) cliNow(port int) string {
	x, ok := g.clisBy.Load(port)
	if !ok {
		return "cli{unjoined}"
	}
	cl := x.(*wsoCli)
	ti, inq, outq := wsoCliSockInfo(cl.c)
	cl.mu.Lock()
	last := "none"
	if n := len(cl.marks); n > 0 {
		last = strconv.Quote(cl.marks[n-1])
	}
	cl.mu.Unlock()
	return fmt.Sprintf("cli{inq=%d outq=%d %s lastMilestone=%s}", inq, outq, wsoTCPShort(ti), last)
}

func (g *wsoRig) cliMarks(port int) string {
	x, ok := g.clisBy.Load(port)
	if !ok {
		return ""
	}
	cl := x.(*wsoCli)
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return strings.Join(cl.marks, "; ")
}

// watch samples the server side of every joined connection every
// wsoWatchEvery until stop is called. A stretch is a run of samples in which
// the connection is wsoStallable and the engine read nothing (bytes received
// minus unread did not move) and the chanReader applied no pause or resume.
// Every stretch that reaches wsoRecvStall is recorded, from its first sample
// to its last. stop returns those. The longest stretch seen on any
// connection is kept too, whether or not it reached the limit
// (longestStretch), so a healthy run shows how close it came.
//
// The samples between a stretch's first and last are not what proves it:
// bytes read, pauses and resumes only ever grow, a chanReader holding
// nothing that is handed nothing stays empty, and an unread socket that is
// not read stays unread, so equal readings at both ends mean the engine read
// nothing in between while nothing asked it to stop. What the samples guard
// against is the process itself not running: if the watcher waits more than
// wsoWatchGap past a due tick, the process was starved of CPU, the engine
// may have been too, and every open stretch starts over (counted as
// watchResets on the waits line).
//
// Start it once the server listens and stop it once the clients are done:
// the verdict is the test's, on the test goroutine.
func (g *wsoRig) watch() (stop func() []*wsoStall) {
	type episode struct {
		fromNs, raw, pauses, resumes int64
		samples                      int
		onset, onsetC                string
		rec                          *wsoStall
	}
	done, fin := make(chan struct{}), make(chan struct{})
	var stalls []*wsoStall
	var longest int64
	go func() {
		defer close(fin)
		eps := map[*wsoSrv]*episode{}
		tick := time.NewTicker(wsoWatchEvery)
		defer tick.Stop()
		idleFrom := g.since() // when the watcher last started waiting for a tick
		for {
			select {
			case <-done:
				for _, ep := range eps {
					if ep.rec != nil {
						ep.rec.open = true
					}
				}
				return
			case <-tick.C:
			}
			// A tick is due at most wsoWatchEvery after the watcher starts
			// waiting (sooner when a pass overran). Waiting longer than that
			// plus wsoWatchGap means this goroutine was runnable and not run.
			if lag := time.Duration(g.since()-idleFrom) - wsoWatchEvery; lag > 0 {
				g.mu.Lock()
				g.watchMaxLag = max(g.watchMaxLag, lag)
				if lag > wsoWatchGap {
					g.watchResets++
					clear(eps)
				}
				g.mu.Unlock()
			}
			g.mu.Lock()
			hs := append([]*wsoSrv(nil), g.hs...)
			g.mu.Unlock()
			for _, s := range hs {
				if s.port <= 0 || s.phase.Load() == 3 {
					delete(eps, s)
					continue
				}
				v := g.srvViewOf(s)
				now := g.since()
				if !wsoStallable(v) {
					delete(eps, s)
					continue
				}
				ep := eps[s]
				if ep == nil || v.rawRead != ep.raw || v.pauses != ep.pauses || v.resumes != ep.resumes {
					eps[s] = &episode{fromNs: now, raw: v.rawRead, pauses: v.pauses, resumes: v.resumes,
						samples: 1, onset: v.String(), onsetC: g.cliNow(s.port)}
					continue
				}
				ep.samples++
				d := now - ep.fromNs
				if d > longest {
					longest = d
					st := wsoStall{port: s.port, fromNs: ep.fromNs, toNs: now, samples: ep.samples,
						onset: ep.onset, onsetC: ep.onsetC, last: v.String(), lastC: g.cliNow(s.port), marks: g.cliMarks(s.port)}
					g.mu.Lock()
					g.longest = st
					g.mu.Unlock()
				}
				if d < int64(wsoRecvStall) {
					continue
				}
				if ep.rec == nil {
					ep.rec = &wsoStall{port: s.port, fromNs: ep.fromNs, onset: ep.onset, onsetC: ep.onsetC}
					stalls = append(stalls, ep.rec)
				}
				ep.rec.toNs, ep.rec.samples = now, ep.samples
				ep.rec.last, ep.rec.lastC, ep.rec.marks = v.String(), g.cliNow(s.port), g.cliMarks(s.port)
			}
			idleFrom = g.since()
		}
	}()
	return func() []*wsoStall {
		close(done)
		<-fin
		g.mu.Lock()
		g.stallMax = time.Duration(longest)
		g.mu.Unlock()
		return stalls
	}
}

// wsoAssertNoStalls prints every WSO-STALL record and fails the test if
// there is one (see watch).
func wsoAssertNoStalls(t *testing.T, name string, stalls []*wsoStall) {
	t.Helper()
	for _, st := range stalls {
		t.Log(st.String())
	}
	if len(stalls) != 0 {
		t.Errorf("%s: %d connection(s) left unread for %v or longer: the server socket held bytes, nothing asked the "+
			"engine to stop reading (the chanReader was neither paused nor holding anything) and the engine read "+
			"nothing. That is the celeris#607 class, a recv held back behind a send the client's closed window "+
			"blocks; the client's own reads get it through, so no give-up shows it. Each WSO-STALL record above has "+
			"the stretch's state at both ends", name, len(stalls), wsoRecvStall)
	}
}

// wsoAssertNoLinkedRecv judges io_uring's linked-recv witness. These servers
// serve nothing but WebSocket connections, and each is detached by its
// upgrade (Context.Detach publishes it before the upgrade handler returns),
// before the engine flushes anything on it. So no recv may ever be chained
// behind a send (IOSQE_IO_LINK): on a detached connection that chain is
// celeris#607 itself (TestFlushSendLinkNeverChainsOnDetachedConn guards the
// function; this guards every path that reaches it).
func wsoAssertNoLinkedRecv(t *testing.T, name string, arms, blockedMaxNs uint64) {
	t.Helper()
	if arms != 0 {
		t.Errorf("%s: the engine chained a recv behind a send %d time(s) (the longest waited %d ms) on a server "+
			"whose every connection is a detached WebSocket: celeris#607's mechanism", name, arms, blockedMaxNs/1e6)
	}
}

// wsoWaitStats summarises every connection's waits for the verdict line.
type wsoWaitStats struct {
	conns                  int
	fcMax, cwMax, closeMax time.Duration
	closeP50, closeP99     time.Duration
	closeSlow              int // close waits over wsoCloseSlow: the old oracle failed these
	closeSilentSlow        int // close waits with a silent stretch over wsoCloseSlow
	maxCloseGap, maxNoProg time.Duration
	fcSlices, drainedBytes int64
	recvStallMax           time.Duration // the longest stretch watch saw (see watch)
	watchResets            int
	watchMaxLag            time.Duration
}

func (g *wsoRig) waitStats() wsoWaitStats {
	var st wsoWaitStats
	var closes []time.Duration
	g.mu.Lock()
	defer g.mu.Unlock()
	st.recvStallMax, st.watchResets, st.watchMaxLag = g.stallMax, g.watchResets, g.watchMaxLag
	for _, cl := range g.clis {
		cl.mu.Lock()
		st.conns++
		st.fcMax = max(st.fcMax, cl.fc.dur)
		st.cwMax = max(st.cwMax, cl.cw.dur)
		st.maxNoProg = max(st.maxNoProg, cl.fc.maxNoProg, cl.cw.maxNoProg)
		st.fcSlices += int64(cl.fc.slices)
		st.drainedBytes += cl.fc.drained + cl.cw.drained
		if cl.close.outcome != "" {
			closes = append(closes, cl.close.dur)
			st.closeMax = max(st.closeMax, cl.close.dur)
			st.maxCloseGap = max(st.maxCloseGap, cl.close.maxGap)
			if cl.close.dur > wsoCloseSlow {
				st.closeSlow++
			}
			if cl.close.maxGap > wsoCloseSlow {
				st.closeSilentSlow++
			}
		}
		cl.mu.Unlock()
	}
	if len(closes) > 0 {
		sort.Slice(closes, func(i, j int) bool { return closes[i] < closes[j] })
		st.closeP50 = closes[len(closes)/2]
		st.closeP99 = closes[(len(closes)*99)/100]
	}
	return st
}

func (st wsoWaitStats) String() string {
	r := func(d time.Duration) string { return d.Round(time.Millisecond).String() }
	return fmt.Sprintf("waits: conns=%d fcMax=%s cwMax=%s maxNoProg=%s fcSlices=%d drainedWhileWaiting=%d closeP50=%s closeP99=%s closeMax=%s closeOver%s=%d closeSilentOver%s=%d maxCloseGap=%s recvStallMax=%s watchResets=%d watchMaxLag=%s",
		st.conns, r(st.fcMax), r(st.cwMax), r(st.maxNoProg), st.fcSlices, st.drainedBytes, r(st.closeP50), r(st.closeP99), r(st.closeMax),
		wsoCloseSlow, st.closeSlow, wsoCloseSlow, st.closeSilentSlow, r(st.maxCloseGap), r(st.recvStallMax), st.watchResets, r(st.watchMaxLag))
}
