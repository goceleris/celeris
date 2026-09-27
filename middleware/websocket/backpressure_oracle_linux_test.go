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
//     handler wrote that the engine has not yet handed to the kernel.
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
)

// wsoRig joins each client connection to the handler that serves it and
// keeps what each side saw.
type wsoRig struct {
	t0      time.Time
	srvPort atomic.Int32 // set once the server listens; read by handler goroutines
	srvs    sync.Map     // client local port -> *wsoSrv
	mu      sync.Mutex
	clis    []*wsoCli
	hs      []*wsoSrv // every handler, joined or not
}

func newWSORig() *wsoRig { return &wsoRig{t0: time.Now()} }

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

	mu      sync.Mutex
	baseOut int64 // server socket: bytes ACKed + queued when the handler started
	baseIn  int64 // server socket: bytes received - unread when the handler started
	baseOK  bool
	exit    string
	exitNs  int64
	errs    []wsoSrvErr
	fd      int
	ino     uint64
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
	if ti, inq, outq, ok := g.srvSockInfo(s); ok {
		s.mu.Lock()
		s.baseOut = int64(ti.Bytes_acked) + int64(outq)
		s.baseIn = int64(ti.Bytes_received) - int64(inq)
		s.baseOK = true
		s.mu.Unlock()
	}
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
// read, which would only misreport a diagnostic). Only read-only calls
// (fstat, getsockopt TCP_INFO, ioctl SIOCINQ/SIOCOUTQ) are made on it.
func (g *wsoRig) srvSockFD(s *wsoSrv) int {
	srvPort := int(g.srvPort.Load())
	if s.port <= 0 || srvPort <= 0 {
		return -1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fd >= 0 {
		var st unix.Stat_t
		if unix.Fstat(s.fd, &st) == nil && st.Ino == s.ino {
			return s.fd
		}
		s.fd = -1
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

// wsoDrainNow empties the client's receive queue without blocking.
func wsoDrainNow(c net.Conn, buf []byte) int64 {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return 0
	}
	sc, err := tc.SyscallConn()
	if err != nil {
		return 0
	}
	var total int64
	_ = sc.Read(func(fd uintptr) bool {
		for {
			n, e := unix.Read(int(fd), buf)
			if n > 0 {
				total += int64(n)
				continue
			}
			if e == unix.EINTR {
				continue
			}
			return true // EAGAIN (empty), EOF or an error: stop, never wait
		}
	})
	return total
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
	engRead, pendingWrite int64
}

func (g *wsoRig) srvView(port int) wsoSrvView {
	var v wsoSrvView
	x, ok := g.srvs.Load(port)
	if !ok {
		return v
	}
	s := x.(*wsoSrv)
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
	baseOK, baseOut, baseIn := s.baseOK, s.baseOut, s.baseIn
	s.mu.Unlock()
	v.engRead, v.pendingWrite = -1, -1
	if v.sockOK && baseOK {
		v.engRead = int64(v.ti.Bytes_received) - int64(v.inq) - baseIn
		v.pendingWrite = wrote - (int64(v.ti.Bytes_acked) + int64(v.outq) - baseOut)
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
	g       *wsoRig
	port    int
	mu      sync.Mutex
	marks   []string
	first   []string
	last    [wsoLastSamples]string
	nSamp   int
	closeNs int64 // rig time the client closed its socket; 0 while open
	gaveUp  bool
	close   wsoClose
	fc, cw  wsoWrite
}

// client registers a dialed connection. Close it with closeClient, which
// records when the client let go (see serverErrs).
func (g *wsoRig) client(c net.Conn) *wsoCli {
	cl := &wsoCli{g: g}
	if a, ok := c.LocalAddr().(*net.TCPAddr); ok {
		cl.port = a.Port
	}
	g.mu.Lock()
	g.clis = append(g.clis, cl)
	g.mu.Unlock()
	return cl
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
	reason    string // "" (completed), "noprog", "cap", "err"
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
// It gives up after wsoWriteIdle without progress, at wsoWaitCap in total, or
// on any error other than the slice's own deadline.
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
		w.drained += wsoDrainNow(c, dbuf)
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
	outcome  string // "eof", "rst", "idle", "cap", "err"
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
// byte received: it comes after wsoCloseIdle without one, or at wsoWaitCap.
// While it waits in silence it records a timeline sample every wsoSlice.
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

// serverErrs splits every server-side error into those that came BEFORE
// their own client closed its socket, which the engine did, and those after,
// which are the client's own teardown echoing back (a socket closed with
// unread data sends an RST). An error whose connection the rig could not
// join is counted as before: it cannot be excused.
func (g *wsoRig) serverErrs() (before, after []wsoJudged) {
	closeAt := map[int]int64{}
	g.mu.Lock()
	for _, cl := range g.clis {
		cl.mu.Lock()
		closeAt[cl.port] = cl.closeNs
		cl.mu.Unlock()
	}
	hs := append([]*wsoSrv(nil), g.hs...)
	g.mu.Unlock()
	for _, s := range hs {
		s.mu.Lock()
		errs := append([]wsoSrvErr(nil), s.errs...)
		s.mu.Unlock()
		for _, e := range errs {
			ca, ok := closeAt[s.port]
			line := fmt.Sprintf("conn 127.0.0.1:%d %s error %q at %s", s.port, e.kind, wsoErrStr(e.err), wsoSec(e.ns))
			if ok && ca > 0 && e.ns >= ca {
				after = append(after, wsoJudged{e.kind, line + fmt.Sprintf(", %s after its client closed", wsoSec(e.ns-ca))})
				continue
			}
			if ok && ca > 0 {
				line += fmt.Sprintf(", %s BEFORE its client closed", wsoSec(ca-e.ns))
			} else {
				line += ", its client had not closed"
			}
			before = append(before, wsoJudged{e.kind, line})
		}
	}
	sort.Slice(before, func(i, j int) bool { return before[i].line < before[j].line })
	sort.Slice(after, func(i, j int) bool { return after[i].line < after[j].line })
	return before, after
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
}

func (g *wsoRig) waitStats() wsoWaitStats {
	var st wsoWaitStats
	var closes []time.Duration
	g.mu.Lock()
	defer g.mu.Unlock()
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
	return fmt.Sprintf("waits: conns=%d fcMax=%s cwMax=%s maxNoProg=%s fcSlices=%d drainedWhileWaiting=%d closeP50=%s closeP99=%s closeMax=%s closeOver%s=%d closeSilentOver%s=%d maxCloseGap=%s",
		st.conns, r(st.fcMax), r(st.cwMax), r(st.maxNoProg), st.fcSlices, st.drainedBytes, r(st.closeP50), r(st.closeP99), r(st.closeMax),
		wsoCloseSlow, st.closeSlow, wsoCloseSlow, st.closeSilentSlow, r(st.maxCloseGap))
}
