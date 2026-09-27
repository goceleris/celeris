//go:build linux

package websocket

// celeris#633 DIAGNOSTIC instrumentation and FAULT INJECTIONS for
// TestBackpressurePauseDoesNotCancelInflightSend. THROWAWAY: lives only on the tmp/633-inject-*
// branches, never in a pull request.
//
// It records, per connection, what the client and the server handler each saw and when, joined
// by the client's local port (the client's first frame carries it), and prints it (RULE 11: an
// oracle that counts an error and discards it blocks every diagnosis).
//
// The injection switches below are the ONLY thing that differs between the two commits of a
// pair (plus, for C1/C2, the source-level coverage instrumentation of this package's non-test
// files). With every switch off, the client does exactly what the original test does: the same
// flood, the same 15 s frame-completion write, the same 1 s drain heuristic, the same 10 s Close
// write and 10 s close wait.

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
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

// Fault-injection switches (celeris#633). C0: all false, no coverage. C1: C0 + atomic coverage
// baked into this package's non-test sources. C2: C1 + c633InjectProgress. C3: C0 +
// c633InjectAbort. C5: C0 + c633InjectReadWhileWaiting.
const (
	c633Variant = "C0"
	// H1 kill arm: the frame-completion write and the Close write give up only after
	// c633ProgWriteIdle with no shrink of the client's send queue (SIOCOUTQ: bytes the server's
	// kernel has not ACKed), and the close wait only after c633ProgCloseIdle with no byte
	// received; each capped at c633ProgCap in total.
	c633InjectProgress = false
	// H2 injection: every 4th client abandons its connection right after the flood (close with
	// the echo unread -> RST), with no slowness anywhere.
	c633InjectAbort = false
	// H3 injection: the frame-completion write and the Close write keep the original 15 s / 10 s
	// budgets but are issued in 1 s slices, and after each slice that times out the client
	// empties its receive queue (non-blocking) before retrying.
	c633InjectReadWhileWaiting = false
)

const (
	c633ProgWriteIdle = 90 * time.Second
	c633ProgCloseIdle = 30 * time.Second
	c633ProgCap       = 240 * time.Second
)

type c633Srv struct {
	c      *Conn
	frames atomic.Int64
	phase  atomic.Int32 // 1 in ReadMessage, 2 in WriteMessage, 3 exited
	lastNs atomic.Int64 // ns since t0 at the last sampled echoed frame (every 16th)
	mu     sync.Mutex
	exit   string // "read: <err>" or "write: <err>"
	exitNs int64
	wErr   error // a counted otherWriteErr, if any
	wErrNs int64
}

// c633Wait describes one of the client's two bounded writes (fc = frame completion, cw = Close
// write): how it was waited for and how it ended.
type c633Wait struct {
	mode        string // fixed | progress | draining
	ms          int64
	iters       int
	progEvents  int
	maxNoProgMs int64
	outq0       int
	outq1       int
	reason      string // "" (completed) | timeout | noprog | cap | err
	drains      int
	drained     int64
	h3Seen      int // slices that ended with client snd_wnd=0 and a non-empty client receive queue
	h3Opened    int // of those, the client's snd_wnd was > 0 within 200 ms of emptying its queue
	h3First     string
}

func (w *c633Wait) String() string {
	if w.mode == "" {
		return "-"
	}
	return fmt.Sprintf("mode=%s ms=%d iters=%d prog=%d maxNoProgMs=%d outq0=%d outq1=%d reason=%s drains=%d drained=%d h3Seen=%d h3Opened=%d",
		w.mode, w.ms, w.iters, w.progEvents, w.maxNoProgMs, w.outq0, w.outq1, w.reason, w.drains, w.drained, w.h3Seen, w.h3Opened)
}

type c633Cli struct {
	mu                          sync.Mutex
	port                        int
	wrote                       int
	floodEndNs                  int64
	srvFramesAtFloodEnd         int64
	fcNeed, fcRemain            int
	fcNs                        int64
	fcErr                       error
	fc, cw                      c633Wait
	drainBytes                  int64
	drainReads                  int
	drainEndNs                  int64
	drainErr                    error
	cwRemain                    int
	cwNs                        int64
	cwErr                       error
	closeSentNs                 int64
	waitBytes                   int64
	waitFirstNs, waitLastNs     int64
	waitEndNs                   int64
	waitOutcome                 string
	waitErr                     error
	giveUp                      string // "fc", "cw", "ct" (close timeout), "rst", "abort" (H2 injection), "" (clean EOF)
	gclass                      string // TCP state class at the give-up, see classify
	srvFramesAtGiveUp           int64
	snapSrv, snapTCPs, snapTCPc string
	closeNs                     int64
}

type c633Rig struct {
	t0        time.Time
	nHandlers atomic.Int64
	kernOnce  sync.Once
	kern0     map[string]int64
	kernFirst string
	srvPort   int
	srvs      sync.Map // client port -> *c633Srv
	mu        sync.Mutex
	clis      []*c633Cli
}

func (g *c633Rig) since() int64 { return int64(time.Since(g.t0)) }

func (g *c633Rig) srvFrames(port int) int64 {
	if v, ok := g.srvs.Load(port); ok {
		return v.(*c633Srv).frames.Load()
	}
	return -1
}

func c633Class(err error) string {
	if err == nil {
		return "nil"
	}
	var ne net.Error
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, syscall.EPIPE):
		return "EPIPE"
	case errors.Is(err, syscall.ECONNRESET):
		return "ECONNRESET"
	case errors.Is(err, syscall.ECANCELED):
		return "ECANCELED"
	case errors.Is(err, ErrWriteClosed):
		return "ErrWriteClosed"
	case errors.Is(err, io.EOF):
		return "EOF"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	}
	var en syscall.Errno
	if errors.As(err, &en) {
		return "errno=" + strconv.Itoa(int(en))
	}
	return "other"
}

func c633Err(err error) string {
	if err == nil {
		return "-"
	}
	return strconv.Quote(err.Error())
}

// c633ProcTCPInode reads /proc/net/tcp for the IPv4 socket with local port lport and remote
// port rport: its state, its tx/rx queues in bytes, its timer, and its inode.
func c633ProcTCPInode(lport, rport int) (string, string, int64) {
	b, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return "proc-err", "", -1
	}
	wl, wr := fmt.Sprintf(":%04X", lport), fmt.Sprintf(":%04X", rport)
	lines := strings.Split(string(b), "\n")
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) < 5 || !strings.HasSuffix(f[1], wl) || !strings.HasSuffix(f[2], wr) {
			continue
		}
		q := strings.SplitN(f[4], ":", 2)
		if len(q) != 2 {
			continue
		}
		tx, _ := strconv.ParseInt(q[0], 16, 64)
		rx, _ := strconv.ParseInt(q[1], 16, 64)
		tr, rtx, inode := "?", "?", ""
		if len(f) > 9 {
			inode = f[9]
		}
		if len(f) > 6 {
			// f[5] = "tr:tm->when" (timer: 0 none, 1 retransmit/TLP, 2 keepalive, 3 TIME_WAIT,
			// 4 zero-window probe); f[6] = unrecovered RTO timeouts / zero-window probes sent.
			tr, rtx = f[5], f[6]
		}
		return fmt.Sprintf("st=%s tx=%d rx=%d timer=%s rtx=%s", f[3], tx, rx, tr, rtx), inode, rx
	}
	return "absent", "", -1
}

// c633FDByInode finds this process's fd for a socket inode (the engine's server-side socket).
func c633FDByInode(inode string) int {
	if inode == "" || inode == "0" {
		return -1
	}
	want := "socket:[" + inode + "]"
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	for _, e := range ents {
		if l, err := os.Readlink("/proc/self/fd/" + e.Name()); err == nil && l == want {
			n, _ := strconv.Atoi(e.Name())
			return n
		}
	}
	return -1
}

func c633TCPInfoFmt(ti *unix.TCPInfo) string {
	return fmt.Sprintf("state=%d snd_wnd=%d rcv_wnd=%d rcv_space=%d rcv_ssthresh=%d probes=%d backoff=%d rto_ms=%d unacked=%d notsent=%d retrans=%d total_retrans=%d last_data_sent_ms=%d last_data_recv_ms=%d last_ack_recv_ms=%d rwnd_limited_ms=%d sndbuf_limited_ms=%d",
		ti.State, ti.Snd_wnd, ti.Rcv_wnd, ti.Rcv_space, ti.Rcv_ssthresh, ti.Probes, ti.Backoff, ti.Rto/1000, ti.Unacked, ti.Notsent_bytes,
		ti.Retrans, ti.Total_retrans, ti.Last_data_sent, ti.Last_data_recv, ti.Last_ack_recv, ti.Rwnd_limited/1000, ti.Sndbuf_limited/1000)
}

// c633TCPInfo reads getsockopt(TCP_INFO) of fd (read-only; safe on the engine's fd).
func c633TCPInfo(fd int) (*unix.TCPInfo, string) {
	if fd < 0 {
		return nil, "nofd"
	}
	ti, err := unix.GetsockoptTCPInfo(fd, unix.SOL_TCP, unix.TCP_INFO)
	if err != nil {
		return nil, "err=" + err.Error()
	}
	return ti, c633TCPInfoFmt(ti)
}

// c633ConnInfo reads the client socket's TCP_INFO, receive queue (SIOCINQ) and send queue
// (SIOCOUTQ) through the conn's own fd.
func c633ConnInfo(c net.Conn) (ti *unix.TCPInfo, inq, outq int) {
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
		if v, err := unix.GetsockoptTCPInfo(int(fd), unix.SOL_TCP, unix.TCP_INFO); err == nil {
			ti = v
		}
		if v, err := unix.IoctlGetInt(int(fd), unix.SIOCINQ); err == nil {
			inq = v
		}
		if v, err := unix.IoctlGetInt(int(fd), unix.SIOCOUTQ); err == nil {
			outq = v
		}
	})
	return ti, inq, outq
}

// c633DrainNow empties the client's receive queue without blocking (H3 injection).
func c633DrainNow(c net.Conn, buf []byte) int64 {
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
			return true // EAGAIN (queue empty), EOF or an error: stop, never wait
		}
	})
	return total
}

func (g *c633Rig) srvState(port int) string {
	v, ok := g.srvs.Load(port)
	if !ok {
		return "nosrv"
	}
	s := v.(*c633Srv)
	depth, spill, paused, closed := -1, int64(-1), false, false
	if r := s.c.engineReader; r != nil {
		depth = len(r.ch)
		spill = r.spillLen.Load()
		r.pausedMu.Lock()
		paused = r.pausedState
		r.pausedMu.Unlock()
		closed = r.closed.Load()
	}
	s.mu.Lock()
	exit, exitNs := s.exit, s.exitNs
	s.mu.Unlock()
	return fmt.Sprintf("phase=%d frames=%d sinceProgMs=%d depth=%d spill=%d rpaused=%t rclosed=%t exit=%q exitMs=%d",
		s.phase.Load(), s.frames.Load(), (g.since()-s.lastNs.Load())/1e6, depth, spill, paused, closed, exit, exitNs/1e6)
}

// c633Kern reads the kernel's TCP memory and every TcpExt counter.
func c633Kern() map[string]int64 {
	out := map[string]int64{}
	if b, err := os.ReadFile("/proc/net/sockstat"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "TCP: ") {
				f := strings.Fields(line)
				for i := 1; i+1 < len(f); i += 2 {
					v, _ := strconv.ParseInt(f[i+1], 10, 64)
					out["sockstat."+f[i]] = v
				}
			}
		}
	}
	if b, err := os.ReadFile("/proc/sys/net/ipv4/tcp_mem"); err == nil {
		f := strings.Fields(string(b))
		for i, n := range []string{"tcp_mem.low", "tcp_mem.pressure", "tcp_mem.high"} {
			if i < len(f) {
				v, _ := strconv.ParseInt(f[i], 10, 64)
				out[n] = v
			}
		}
	}
	if b, err := os.ReadFile("/proc/net/netstat"); err == nil {
		ls := strings.Split(string(b), "\n")
		for i := 0; i+1 < len(ls); i += 2 {
			h, v := strings.Fields(ls[i]), strings.Fields(ls[i+1])
			if len(h) == 0 || h[0] != "TcpExt:" || len(h) != len(v) {
				continue
			}
			for j := 1; j < len(h); j++ {
				n, _ := strconv.ParseInt(v[j], 10, 64)
				out["TcpExt."+h[j]] = n
			}
		}
	}
	return out
}

// c633KernStr prints the TcpExt deltas that are non-zero (every counter), plus sockstat/tcp_mem.
func c633KernStr(now, base map[string]int64) string {
	keys := make([]string, 0, len(now))
	for k := range now {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if strings.HasPrefix(k, "TcpExt.") {
			if base == nil {
				continue
			}
			if d := now[k] - base[k]; d != 0 {
				fmt.Fprintf(&b, "%s=+%d ", strings.TrimPrefix(k, "TcpExt."), d)
			}
		} else {
			fmt.Fprintf(&b, "%s=%d ", k, now[k])
		}
	}
	return strings.TrimSpace(b.String())
}

// c633IDFrame is a masked text frame of exactly 126 bytes (like every flood frame) whose
// payload is "cid=<port>;" padded with 'x': the handler's join key.
func c633IDFrame(port int) []byte {
	p := []byte("cid=" + strconv.Itoa(port) + ";")
	for len(p) < 120 {
		p = append(p, 'x')
	}
	m := [4]byte{0x11, 0x22, 0x33, 0x44}
	out := []byte{0x81, 0x80 | 120, m[0], m[1], m[2], m[3]}
	for j, ch := range p {
		out = append(out, ch^m[j%4])
	}
	return out
}

// classify names the TCP state of a connection at the client's give-up:
//
//	srv-window-closed: the server socket advertises rcv_wnd=0 (its engine is not reading: paused
//	                   or behind) -- flow control working as designed;
//	lost-window-update: the server advertises rcv_wnd>0 with nothing unread (rx=0) while the
//	                   client still believes snd_wnd=0 (the H3 signature);
//	other.
func c633ClassifyTCP(srv, cli *unix.TCPInfo, srvRx int64) string {
	switch {
	case srv == nil || cli == nil:
		return "noinfo"
	case srv.Rcv_wnd == 0:
		return "srv-window-closed"
	case cli.Snd_wnd == 0 && srvRx == 0:
		return "lost-window-update"
	case cli.Snd_wnd == 0:
		return "cli-sndwnd0-srv-rx>0"
	}
	return "other"
}

// snapshot is taken on the client goroutine at the moment it gives up, BEFORE it closes.
func (g *c633Rig) snapshot(r *c633Cli) {
	s := g.srvState(r.port)
	ts, sInode, sRx := c633ProcTCPInode(g.srvPort, r.port)
	tc, cInode, _ := c633ProcTCPInode(r.port, g.srvPort)
	sti, sInfo := c633TCPInfo(c633FDByInode(sInode))
	cti, cInfo := c633TCPInfo(c633FDByInode(cInode))
	ts += " | tcpinfo " + sInfo
	k := c633Kern()
	tc += fmt.Sprintf(" sockstat.mem=%d tcp_mem.pressure=%d", k["sockstat.mem"], k["tcp_mem.pressure"]) + " | tcpinfo " + cInfo
	g.kernOnce.Do(func() {
		str := fmt.Sprintf("first-give-up +%dms %s", g.since()/1e6, c633KernStr(k, g.kern0))
		g.mu.Lock()
		g.kernFirst = str
		g.mu.Unlock()
	})
	cls := c633ClassifyTCP(sti, cti, sRx)
	fr := g.srvFrames(r.port)
	r.mu.Lock()
	r.snapSrv, r.snapTCPs, r.snapTCPc, r.gclass, r.srvFramesAtGiveUp = s, ts, tc, cls, fr
	r.mu.Unlock()
}

// stateLine is a one-line joined state of both sockets and the handler (H3 injection record).
func (g *c633Rig) stateLine(port int) string {
	ts, sInode, sRx := c633ProcTCPInode(g.srvPort, port)
	tc, cInode, _ := c633ProcTCPInode(port, g.srvPort)
	sti, sInfo := c633TCPInfo(c633FDByInode(sInode))
	cti, cInfo := c633TCPInfo(c633FDByInode(cInode))
	return fmt.Sprintf("class=%s srv{%s} tcpSrv{%s | tcpinfo %s} tcpCli{%s | tcpinfo %s}",
		c633ClassifyTCP(sti, cti, sRx), g.srvState(port), ts, sInfo, tc, cInfo)
}

// writeAllErr is writeAll with the same semantics that also returns what was left and why.
func writeAllErr(c net.Conn, buf []byte, within time.Duration) (bool, int, error) {
	end := time.Now().Add(within)
	for len(buf) > 0 {
		_ = c.SetWriteDeadline(end)
		n, err := c.Write(buf)
		buf = buf[n:]
		if err != nil {
			return len(buf) == 0, len(buf), err
		}
	}
	return true, 0, nil
}

// clientWrite performs one of the client's two bounded writes in the mode the switches select.
func (g *c633Rig) clientWrite(c net.Conn, r *c633Cli, site string, buf []byte, within time.Duration) (bool, int, error) {
	var w c633Wait
	var ok bool
	var left int
	var err error
	start := time.Now()
	switch {
	case c633InjectProgress:
		ok, left, err = g.writeProgress(c, buf, &w)
	case c633InjectReadWhileWaiting:
		ok, left, err = g.writeDraining(c, r.port, buf, within, &w)
	default:
		w.mode = "fixed"
		ok, left, err = writeAllErr(c, buf, within)
		w.iters = 1
		if !ok {
			w.reason = c633Class(err)
		}
	}
	w.ms = int64(time.Since(start) / time.Millisecond)
	r.mu.Lock()
	if site == "fc" {
		r.fc = w
	} else {
		r.cw = w
	}
	r.mu.Unlock()
	return ok, left, err
}

// writeProgress: H1 kill arm. 1 s write slices; progress = the client's send queue shrank (the
// server's kernel ACKed bytes, i.e. its engine read); give up after c633ProgWriteIdle without
// progress or c633ProgCap in total.
func (g *c633Rig) writeProgress(c net.Conn, buf []byte, w *c633Wait) (bool, int, error) {
	w.mode = "progress"
	start := time.Now()
	lastProg := start
	_, _, q0 := c633ConnInfo(c)
	w.outq0 = q0
	lastQ := q0
	var err error
	var maxGap time.Duration
	for len(buf) > 0 {
		_ = c.SetWriteDeadline(time.Now().Add(time.Second))
		n, werr := c.Write(buf)
		buf = buf[n:]
		w.iters++
		now := time.Now()
		if werr == nil || len(buf) == 0 {
			continue
		}
		if !errors.Is(werr, os.ErrDeadlineExceeded) {
			err, w.reason = werr, "err"
			break
		}
		_, _, q := c633ConnInfo(c)
		if n > 0 || (q >= 0 && lastQ >= 0 && q < lastQ) {
			if gap := now.Sub(lastProg); gap > maxGap {
				maxGap = gap
			}
			lastProg = now
			w.progEvents++
		}
		lastQ = q
		if now.Sub(lastProg) >= c633ProgWriteIdle {
			err, w.reason = werr, "noprog"
			break
		}
		if now.Sub(start) >= c633ProgCap {
			err, w.reason = werr, "cap"
			break
		}
	}
	if gap := time.Since(lastProg); gap > maxGap {
		maxGap = gap
	}
	w.maxNoProgMs = int64(maxGap / time.Millisecond)
	_, _, w.outq1 = c633ConnInfo(c)
	return len(buf) == 0, len(buf), err
}

// writeDraining: H3 injection. The original budget, in 1 s slices; after each slice that times
// out, record whether the client sits at snd_wnd=0 with unread bytes queued, empty its receive
// queue, and check whether its snd_wnd opens within 200 ms.
func (g *c633Rig) writeDraining(c net.Conn, port int, buf []byte, within time.Duration, w *c633Wait) (bool, int, error) {
	w.mode = "draining"
	end := time.Now().Add(within)
	_, _, w.outq0 = c633ConnInfo(c)
	dbuf := make([]byte, 64<<10)
	var err error
	for len(buf) > 0 {
		dl := time.Now().Add(time.Second)
		if dl.After(end) {
			dl = end
		}
		_ = c.SetWriteDeadline(dl)
		n, werr := c.Write(buf)
		buf = buf[n:]
		w.iters++
		if werr == nil || len(buf) == 0 {
			continue
		}
		if !errors.Is(werr, os.ErrDeadlineExceeded) {
			err, w.reason = werr, "err"
			break
		}
		if !time.Now().Before(end) {
			err, w.reason = werr, "timeout"
			break
		}
		ti, inq, _ := c633ConnInfo(c)
		h3 := ti != nil && ti.Snd_wnd == 0 && inq > 0
		if h3 {
			w.h3Seen++
			if w.h3First == "" {
				w.h3First = g.stateLine(port)
			}
		}
		w.drained += c633DrainNow(c, dbuf)
		w.drains++
		if h3 {
			for i := 0; i < 20; i++ {
				time.Sleep(10 * time.Millisecond)
				if ti2, _, _ := c633ConnInfo(c); ti2 != nil && ti2.Snd_wnd > 0 {
					w.h3Opened++
					break
				}
			}
		}
	}
	_, _, w.outq1 = c633ConnInfo(c)
	return len(buf) == 0, len(buf), err
}

// wsHandshakeCID is wsHandshake with the client's local port in the query (?cid=), percent-
// encoded so the parsed value is a fresh string (Conn.Query aliases the engine's recv buffer).
func wsHandshakeCID(c net.Conn, hostPort string, cid int) error {
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	enc := ""
	for _, ch := range strconv.Itoa(cid) {
		enc += fmt.Sprintf("%%%02X", ch)
	}
	req := "GET /ws?cid=" + enc + " HTTP/1.1\r\nHost: " + hostPort + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = c.SetDeadline(time.Time{}) }()
	if _, err := c.Write([]byte(req)); err != nil {
		return err
	}
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.Contains(line, " 101 ") {
		return fmt.Errorf("no 101: %q", strings.TrimSpace(line))
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if l == "\r\n" {
			return nil
		}
	}
}

func ms(ns int64) int64 { return ns / 1e6 }

// report prints one C633CONN line per connection plus aggregates. Called after wg.Wait.
func (g *c633Rig) report(t *testing.T, kind string) {
	g.mu.Lock()
	clis := append([]*c633Cli(nil), g.clis...)
	kernFirst := g.kernFirst
	g.mu.Unlock()
	sort.Slice(clis, func(i, j int) bool { return clis[i].port < clis[j].port })
	t.Logf("C633VARIANT kind=%s variant=%s progress=%t abort=%t readWhileWaiting=%t progWriteIdle=%s progCloseIdle=%s progCap=%s",
		kind, c633Variant, c633InjectProgress, c633InjectAbort, c633InjectReadWhileWaiting, c633ProgWriteIdle, c633ProgCloseIdle, c633ProgCap)
	agg := map[string]int{}
	werr := map[string]int{}
	for _, r := range clis {
		r.mu.Lock()
		var srv *c633Srv
		if v, ok := g.srvs.Load(r.port); ok {
			srv = v.(*c633Srv)
		}
		wErrS, wErrMs, wAfterClose := "-", int64(-1), "-"
		srvFinal := "nosrv"
		if srv != nil {
			srv.mu.Lock()
			if srv.wErr != nil {
				wErrS, wErrMs = c633Err(srv.wErr), ms(srv.wErrNs)
				if r.closeNs > 0 {
					wAfterClose = strconv.FormatBool(srv.wErrNs >= r.closeNs)
				} else {
					wAfterClose = "cli-open"
				}
				werr[c633Class(srv.wErr)+" "+srv.wErr.Error()+" afterClientClose="+wAfterClose+" giveUp="+r.giveUp]++
			}
			srv.mu.Unlock()
			srvFinal = g.srvState(r.port)
		}
		gu := r.giveUp
		if gu == "" {
			gu = "none"
		}
		agg["giveUp="+gu]++
		if r.giveUp != "" && r.giveUp != "rst" {
			agg["giveUp="+gu+" class="+r.gclass]++
			prog := "srvProgressed"
			if r.srvFramesAtGiveUp >= 0 && r.srvFramesAtGiveUp == r.srvFramesAtFloodEnd {
				prog = "srvNoProgressSinceFloodEnd"
			}
			agg["giveUp="+gu+" "+prog]++
		}
		switch r.giveUp {
		case "fc":
			agg["ccf fc/"+c633Class(r.fcErr)+" reason="+r.fc.reason]++
		case "cw":
			agg["ccf cw/"+c633Class(r.cwErr)+" reason="+r.cw.reason]++
		case "ct":
			if r.waitBytes > 0 {
				agg["ct waitBytes>0"]++
			} else {
				agg["ct waitBytes=0"]++
			}
		}
		if r.fc.h3Seen > 0 {
			agg["fc h3Seen>0"]++
			if r.fc.h3Opened > 0 {
				agg["fc h3Opened>0"]++
			}
		}
		if r.cw.h3Seen > 0 {
			agg["cw h3Seen>0"]++
		}
		agg["drainEnd="+c633Class(r.drainErr)]++
		t.Logf("C633CONN kind=%s variant=%s port=%d giveUp=%s gclass=%s wrote=%d floodEndMs=%d srvFramesFloodEnd=%d srvFramesGiveUp=%d fcNeed=%d fcRemain=%d fcMs=%d fcErr=%s fcClass=%s fc{%s} "+
			"drainB=%d drainReads=%d drainEndMs=%d drainErr=%s drainClass=%s cwRemain=%d cwMs=%d cwErr=%s cwClass=%s cw{%s} "+
			"closeSentMs=%d waitB=%d waitFirstMs=%d waitLastMs=%d waitEndMs=%d wait=%s waitErr=%s cliCloseMs=%d "+
			"srvWErr=%s srvWErrMs=%d srvWErrAfterCliClose=%s | atGiveUp srv{%s} tcpSrv{%s} tcpCli{%s} | final srv{%s}",
			kind, c633Variant, r.port, gu, r.gclass, r.wrote, ms(r.floodEndNs), r.srvFramesAtFloodEnd, r.srvFramesAtGiveUp, r.fcNeed, r.fcRemain, ms(r.fcNs), c633Err(r.fcErr), c633Class(r.fcErr), r.fc.String(),
			r.drainBytes, r.drainReads, ms(r.drainEndNs), c633Err(r.drainErr), c633Class(r.drainErr), r.cwRemain, ms(r.cwNs), c633Err(r.cwErr), c633Class(r.cwErr), r.cw.String(),
			ms(r.closeSentNs), r.waitBytes, ms(r.waitFirstNs), ms(r.waitLastNs), ms(r.waitEndNs), r.waitOutcome, c633Err(r.waitErr), ms(r.closeNs),
			wErrS, wErrMs, wAfterClose, r.snapSrv, r.snapTCPs, r.snapTCPc, srvFinal)
		if r.fc.h3First != "" {
			t.Logf("C633H3 kind=%s port=%d site=fc h3Seen=%d h3Opened=%d first{%s}", kind, r.port, r.fc.h3Seen, r.fc.h3Opened, r.fc.h3First)
		}
		if r.cw.h3First != "" {
			t.Logf("C633H3 kind=%s port=%d site=cw h3Seen=%d h3Opened=%d first{%s}", kind, r.port, r.cw.h3Seen, r.cw.h3Opened, r.cw.h3First)
		}
		r.mu.Unlock()
	}
	keys := make([]string, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("C633AGG kind=%s variant=%s %s count=%d", kind, c633Variant, k, agg[k])
	}
	keys = keys[:0]
	for k := range werr {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("C633WERR kind=%s count=%d %s", kind, werr[k], k)
	}
	// Handlers still running once every client has finished: wedge candidates.
	phases := map[int32]int{}
	var echoed int64
	g.srvs.Range(func(k, v any) bool {
		s := v.(*c633Srv)
		echoed += s.frames.Load()
		ph := s.phase.Load()
		phases[ph]++
		if ph != 3 {
			t.Logf("C633LIVE kind=%s port=%d %s", kind, k.(int), g.srvState(k.(int)))
		}
		return true
	})
	if kernFirst != "" {
		t.Logf("C633KERN kind=%s %s", kind, kernFirst)
	}
	t.Logf("C633KERN kind=%s end %s", kind, c633KernStr(c633Kern(), g.kern0))
	el := g.since()
	t.Logf("C633THRU kind=%s variant=%s echoedFrames=%d elapsedMs=%d echoedPerSec=%d", kind, c633Variant, echoed, ms(el), echoed*1e9/max(el, 1))
	t.Logf("C633HANDLERS kind=%s handlers-that-read-a-frame=%d", kind, g.nHandlers.Load())
	t.Logf("C633PHASES kind=%s reading=%d writing=%d exited=%d handlers=%d clients=%d", kind, phases[1], phases[2], phases[3],
		phases[1]+phases[2]+phases[3], len(clis))
}
