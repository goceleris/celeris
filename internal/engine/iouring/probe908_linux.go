//go:build linux

package iouring

// celeris#988 diagnostic probe. NOT FOR MERGE: this file and its hooks exist
// on the probe/908-zc-debug branch only.
//
// Off unless CELERIS_PROBE_908=1 is in the environment (read once, at package
// init), and even then it follows exactly ONE kind of connection: the one a
// worker adopts through AdoptConn with a non-empty carry (the
// TestIouringAdoptCarriedLarge*908 shape). Every other connection, and every
// other test, pays one nil check on Worker.probe908 per hook and logs
// nothing.
//
// What it records for the adopted conn, to stderr, one "PROBE908" line each:
//
//   - ADOPT / REPLAY lines: what the adopt replay did with the carried bytes
//     and whether the replay itself issued the first send (flushSend called
//     from attachAdoptedFD) or handed the conn to the normal send path
//     (promotion to the async dispatch goroutine, or a dirty-list retry).
//   - CQE lines: every CQE of the conn, rate limited (the first 200, then
//     every 1000th, then every one once the conn has stalled for 2 s), with
//     res, the raw flags and their names (F_MORE, F_NOTIF, F_SOCK_NONEMPTY,
//     F_BUFFER), what kind of CQE it is (SEND_ZC first CQE, SEND_ZC
//     notification, plain send, recv...), and the conn's send state before
//     and after the CQE was handled.
//   - EVT lines: each SEND/SEND_ZC/WRITEV submission (prepSendSQE, flushSend),
//     each recv arm and each markDirty, with the call chain, i.e. who re-arms.
//   - WAIT lines: the ring wait the worker is about to enter and how long it
//     took, with the SQ ring's placed/consumed counters, so "SQE never
//     consumed" can be told from "consumed, never completed".
//   - SOCK lines: the kernel's view of the server-side socket, read by a
//     helper goroutine (thread-safe getsockopt, independent of the worker, so
//     it still reports when the worker is wedged): TCP_INFO, TIOCOUTQ,
//     SIOCOUTQNSD, SO_SNDBUF, /proc/self/status VmLck and VmPin, plus the
//     shared ring's indexes (atomic loads). The helper never wakes the worker
//     unless CELERIS_PROBE_908_WAKE=1 (see probe908Wake): a wake could heal
//     the stall being studied.
//   - REQ lines: the test's snapshot requests (half-way, failure), answered by
//     the worker with its own state, or reported as unanswered.

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// probe908On is read once. Never re-read per CQE.
var probe908On = os.Getenv("CELERIS_PROBE_908") == "1"

// probe908Wake (CELERIS_PROBE_908_WAKE=1) lets the helper goroutine and a
// test's snapshot request kick the worker's wake eventfd. OFF by default: a
// kick ends an untimed ring wait and makes the worker run a loop pass, which
// could heal exactly the stall under study. With it off the probe never
// changes what the worker does; the worker answers a snapshot request at its
// next natural loop pass, the helper's socket and ring-counter lines (atomic
// reads, no wake) report even when the worker is wedged, and an unanswered
// request is reported as such.
var probe908Wake = os.Getenv("CELERIS_PROBE_908_WAKE") == "1"

const (
	probe908FirstN    = 200  // log the first N CQEs / events
	probe908EveryN    = 1000 // then every Nth
	probe908StallNs   = int64(2 * time.Second)
	probe908WaitFirst = 15 // wait lines logged unconditionally; later ones only when slow, untimed, every 1000th, or (stalled) 10 per second
	probe908MaxLife   = 40 * time.Second

	// IORING_CQE_F_SOCK_NONEMPTY is not named in consts.go (the engine does
	// not read it); bit 2 of cqe->flags in the uapi.
	probe908FSockNonEmpty = 1 << 2
	sioCOUTQNSD           = 0x894b // SIOCOUTQNSD: unsent bytes
)

// probe908HubT is the rendezvous between a test goroutine that wants a
// snapshot (ask) and the worker that owns the probed conn.
type probe908HubT struct {
	mu     sync.Mutex
	active []*probe908State
	seq    atomic.Uint64
	label  string
}

var probe908Hub probe908HubT

func (h *probe908HubT) add(p *probe908State) {
	h.mu.Lock()
	h.active = append(h.active, p)
	h.mu.Unlock()
}

func (h *probe908HubT) remove(p *probe908State) {
	h.mu.Lock()
	for i, q := range h.active {
		if q == p {
			h.active = append(h.active[:i], h.active[i+1:]...)
			break
		}
	}
	h.mu.Unlock()
}

// probe908Ask asks every live probed conn's worker for its state, waking each
// worker through its eventfd, and returns what they answered, or says which
// did not answer within d. Safe from any goroutine. Empty when the probe is
// off.
func probe908Ask(label, peer string, d time.Duration) string {
	if !probe908On {
		return ""
	}
	h := &probe908Hub
	h.mu.Lock()
	var act []*probe908State
	for _, p := range h.active {
		if p.peer == peer && !p.done.Load() { // only the conn this test adopted, never a leftover of an earlier test
			act = append(act, p)
		}
	}
	h.label = label
	seq := h.seq.Add(1)
	h.mu.Unlock()
	if len(act) == 0 {
		return "engine: no live probed conn for peer " + peer + " (not adopted yet, or already gone from the conn table)"
	}
	if probe908Wake {
		for _, p := range act {
			p.w.wakeFD.Signal()
		}
	}
	deadline := time.Now().Add(d)
	var out []string
	for _, p := range act {
		for {
			p.reqMu.Lock()
			txt, ok := p.replyN[seq]
			p.reqMu.Unlock()
			if ok {
				out = append(out, txt)
				break
			}
			if p.done.Load() {
				out = append(out, fmt.Sprintf("engine peer=%s w=%d fd=%d: probe ended before it answered (conn gone)", p.peer, p.w.id, p.fd))
				break
			}
			if time.Now().After(deadline) {
				state := "running (not in a ring wait)"
				if since := p.inWaitSince.Load(); since != 0 {
					m := "?"
					if mp := p.waitModeA.Load(); mp != nil {
						m = *mp
					}
					state = fmt.Sprintf("parked in a ring wait of mode [%s] for %s without returning", m, time.Since(time.Unix(0, since)).Round(time.Millisecond))
				}
				out = append(out, fmt.Sprintf("engine peer=%s w=%d fd=%d: WORKER DID NOT ANSWER in %s (woke=%v): it is %s; ring(atomic): %s", p.peer, p.w.id, p.fd, d, probe908Wake, state, p.w.ring.probe908Counters()))
				break
			}
			if probe908Wake {
				p.w.wakeFD.Signal()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return strings.Join(out, "\n")
}

// probe908State is the probe of one adopted conn. Fields below "worker thread
// only" are touched by the worker alone; done is the one the helper goroutine
// shares.
type probe908State struct {
	w    *Worker
	cs   *connState
	fd   int
	gen  uint32
	peer string // client's address, the join key
	t0   time.Time
	done atomic.Bool
	// inWaitSince/waitModeA let a goroutine other than the worker say what the
	// worker is doing when it does not answer: the UnixNano at which it entered
	// the ring wait it has not returned from yet (0 when it is running), and
	// that wait's mode (a constant string, no allocation).
	inWaitSince atomic.Int64
	waitModeA   atomic.Pointer[string]
	reqMu       sync.Mutex
	replyN      map[uint64]string // seq -> engine snapshot text

	// worker thread only
	nCQE        uint64
	nEvt        uint64
	nWait       uint64
	stalled     bool
	lastCQE     int64 // UnixNano of the last CQE of this conn, or the adopt
	seenSeq     uint64
	inCQE       bool
	curSeq      uint64
	curEmit     bool
	preState    string
	curHead     string
	waitStart   time.Time
	waitMode    string
	waitLog     bool
	lastWaitLog time.Time
}

func (p *probe908State) ts() string {
	return fmt.Sprintf("t=+%.3fms", float64(time.Since(p.t0).Microseconds())/1000)
}

func (p *probe908State) logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "PROBE908 peer=%s w=%d fd=%d %s %s\n", p.peer, p.w.id, p.fd, p.ts(), fmt.Sprintf(format, a...))
}

// is reports whether cs is the probed conn.
func (p *probe908State) is(cs *connState) bool { return p != nil && cs == p.cs && !p.done.Load() }

func (p *probe908State) sendState(cs *connState) string {
	return fmt.Sprintf("sending=%v zcNotifPending=%v sendIsZC=%v zcSent=%d sendBuf=%d writeBuf=%d bodyBuf=%d sendBody=%d recvArmed=%v recvLinked=%v recvPaused=%v needsRecv=%v dirty=%v closing=%v inflight=%d held=%d",
		cs.sending, cs.zcNotifPending, cs.sendIsZC, cs.zcSentBytes, len(cs.sendBuf), len(cs.writeBuf), len(cs.bodyBuf), len(cs.sendBody),
		cs.recvArmed, cs.recvLinked, cs.recvPaused, cs.needsRecv, cs.dirty, cs.closing, cs.kernelInflight, len(cs.heldSends))
}

func (p *probe908State) ringState() string {
	r := p.w.ring
	h, t := r.BeginCQ()
	return fmt.Sprintf("sq_placed=%d sq_consumed=%d sq_unconsumed=%d pending=%d cq_head=%d cq_tail=%d sendsPending=%v",
		r.sqPlaced(), r.sqConsumed(), int32(r.sqPlaced()-r.sqConsumed()), r.Pending(), h, t, p.w.sendsPending)
}

func probe908Flags(f uint32) string {
	var s []string
	if f&cqeFBuffer != 0 {
		s = append(s, fmt.Sprintf("F_BUFFER(id=%d)", cqeBufferID(f)))
	}
	if f&cqeFMore != 0 {
		s = append(s, "F_MORE")
	}
	if f&probe908FSockNonEmpty != 0 {
		s = append(s, "F_SOCK_NONEMPTY")
	}
	if f&cqeFNotif != 0 {
		s = append(s, "F_NOTIF")
	}
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, "|")
}

func probe908OpName(op uint64) string {
	switch op {
	case udRecv:
		return "RECV"
	case udSend:
		return "SEND"
	case udClose:
		return "CLOSE"
	case udRecvCancel:
		return "RECV_CANCEL"
	case udHeaderTimer:
		return "HDR_TIMER"
	case udTransplantReap:
		return "REAP"
	}
	return fmt.Sprintf("op%#x", op>>56)
}

// callers returns "a <- b <- c": the function names of the n frames above the
// caller of the probe hook, skipping skip frames.
func probe908Callers(skip, n int) string {
	pcs := make([]uintptr, n+skip)
	got := runtime.Callers(2, pcs)
	if got <= skip {
		return "?"
	}
	frames := runtime.CallersFrames(pcs[skip:got])
	var names []string
	for len(names) < n {
		f, more := frames.Next()
		name := f.Function
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		names = append(names, fmt.Sprintf("%s:%d", name, f.Line))
		if !more {
			break
		}
	}
	return strings.Join(names, " <- ")
}

// ---- adopt ----------------------------------------------------------------

// probe908Adopt starts following cs. Worker thread, from attachAdoptedFD,
// after the conn is installed and before the carried bytes are replayed.
func (w *Worker) probe908Adopt(cs *connState, fd int, carry engine.Carryover) {
	p := &probe908State{
		w: w, cs: cs, fd: fd, gen: cs.generation, peer: carry.RemoteAddr,
		t0: time.Now(), replyN: map[uint64]string{}, lastCQE: time.Now().UnixNano(),
		seenSeq: probe908Hub.seq.Load(), // requests made before this conn existed are not its to answer
	}
	if old := w.probe908; old != nil {
		old.done.Store(true)
		probe908Hub.remove(old)
	}
	w.probe908 = p
	probe908Hub.add(p)
	var rl unix.Rlimit
	_ = unix.Getrlimit(unix.RLIMIT_MEMLOCK, &rl)
	p.logf("ADOPT gen=%d carried=%d peer=%s async=%v bufRing=%v fixedFiles=%v sqpoll=%v sendZC=%v zcHoldBytes=%d memlock_cur=%d memlock_max=%d wake=%v workers_conns=%d tcp_wmem=%q tcp_rmem=%q",
		cs.generation, len(carry.Buffered), carry.RemoteAddr, w.async, w.bufRing != nil, w.fixedFiles, w.sqpoll, w.sendZC, w.zcHoldBytes,
		rl.Cur, rl.Max, probe908Wake, w.connCount, probe908Proc("/proc/sys/net/ipv4/tcp_wmem"), probe908Proc("/proc/sys/net/ipv4/tcp_rmem"))
	p.logf("ADOPT state: %s | ring: %s", p.sendState(cs), p.ringState())
	go p.helper()
}

func probe908Proc(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return strings.Join(strings.Fields(string(b)), " ")
}

// replayDecision records what the adopt replay did about the send: called
// right after the replay's own flushSend (or in place of it, when the replay
// hands the conn to something else). where names the branch.
func (p *probe908State) replayDecision(where string, flushCalled bool, sqFull bool, beforeSending bool) {
	cs := p.cs
	issued := flushCalled && !beforeSending && cs.sending
	p.logf("REPLAY %s: flushSend called by replay=%v; replay issued the first send itself=%v (cs.sending %v->%v, sendIsZC=%v, sendBuf=%d, writeBuf=%d, bodyBuf=%d, sqFull=%v); %s",
		where, flushCalled, issued, beforeSending, cs.sending, cs.sendIsZC, len(cs.sendBuf), len(cs.writeBuf), len(cs.bodyBuf), sqFull, p.ringState())
	if !issued {
		p.logf("REPLAY %s: HANDED OFF, the first send is not the replay's: nothing queued or sending is left to the normal path (dirty list, dispatch goroutine, recv completion)", where)
	}
}

// ---- CQE ------------------------------------------------------------------

// probe908Match reports whether the CQE ud belongs to the probed conn.
func (p *probe908State) match(ud uint64) bool {
	return !p.done.Load() && int(ud&fdMask) == p.fd && decodeGen(ud) == p.gen
}

func (p *probe908State) rateOK(n uint64) bool {
	return p.stalled || n <= probe908FirstN || n%probe908EveryN == 0
}

// cqePre runs before the dispatch handles a CQE of the probed conn.
func (p *probe908State) cqePre(c *completionEntry) {
	p.nCQE++
	p.curSeq = p.nCQE
	p.curEmit = p.rateOK(p.nCQE)
	p.inCQE = true
	p.lastCQE = time.Now().UnixNano()
	cs := p.cs
	op := c.UserData & udMask
	kind := probe908OpName(op)
	if op == udSend {
		switch {
		case cqeIsNotif(c.Flags):
			kind = "SEND_ZC-NOTIF"
		case cqeHasMore(c.Flags):
			kind = "SEND_ZC-first(F_MORE)"
		case cs.sendIsZC:
			kind = "SEND_ZC-single(no F_MORE)"
		default:
			kind = "SEND(plain/WRITEV)"
		}
	}
	p.preState = p.sendState(cs)
	p.curHead = fmt.Sprintf("CQE#%d %s res=%d flags=%#x[%s] ZC=%v", p.curSeq, kind, c.Res, c.Flags, probe908Flags(c.Flags), kind != "RECV" && (cs.sendIsZC || cqeIsNotif(c.Flags)) && op == udSend)
	if p.curEmit {
		// NOTIF res: the kernel fills IORING_NOTIF_USAGE_ZC_COPIED (1<<31) only for a SEND_ZC armed
		// with IORING_SEND_ZC_REPORT_USAGE. The engine's prepSendZC does not set it (only the startup
		// probe does), so res=0 here means "usage not requested", never "really zero-copy".
		extra := ""
		if op == udSend && cqeIsNotif(c.Flags) {
			extra = fmt.Sprintf(" notif_res=%#x(usage=not-requested: the engine sets no REPORT_USAGE; bit31=%v)", uint32(c.Res), uint32(c.Res)&(1<<31) != 0)
		}
		p.logf("%s%s PRE: %s", p.curHead, extra, p.preState)
	}
}

// cqePost runs after the dispatch handled it.
func (p *probe908State) cqePost() {
	if !p.inCQE {
		return
	}
	p.inCQE = false
	if p.curEmit {
		p.logf("CQE#%d POST: %s", p.curSeq, p.sendState(p.cs))
	}
}

// ---- events (submit, arm, dirty) ----------------------------------------

func (p *probe908State) evtOK() bool {
	p.nEvt++
	return p.rateOK(p.nEvt)
}

// sendSubmitted: prepSendSQE armed a SEND or SEND_ZC for the probed conn.
func (p *probe908State) sendSubmitted(linked bool) {
	if !p.evtOK() {
		return
	}
	cs := p.cs
	kind := "SEND"
	if cs.sendIsZC {
		kind = "SEND_ZC"
	}
	p.logf("EVT submit %s len=%d linked=%v w.sendZC=%v sq_placed(before this SQE is published)=%d | chain: %s",
		kind, len(cs.sendBuf), linked, p.w.sendZC, p.w.ring.sqPlaced(), probe908Callers(1, 6))
}

// flushSendDone: flushSend returned. before is the conn's state on entry.
func (p *probe908State) flushSendDone(name, before string, wasSending, wasNotif bool) {
	if !p.evtOK() {
		return
	}
	cs := p.cs
	p.logf("EVT %s done: entry{%s} exit{%s} newSubmit=%v | chain: %s | %s",
		name, before, p.sendState(cs), !wasSending && !wasNotif && cs.sending, probe908Callers(1, 6), p.ringState())
}

func (p *probe908State) recvArmed(multishot bool) {
	if !p.evtOK() {
		return
	}
	p.logf("EVT recv ARMED multishot=%v bufRing=%v w.fixedFiles=%v cs.fixedFile=%v (sqe flags use w.fixedFiles) | chain: %s",
		multishot, p.w.bufRing != nil, p.w.fixedFiles, p.cs.fixedFile, probe908Callers(1, 6))
}

func (p *probe908State) dirtyMarked() {
	if !p.evtOK() {
		return
	}
	p.logf("EVT markDirty | chain: %s | %s", probe908Callers(1, 6), p.sendState(p.cs))
}

// ---- wait -----------------------------------------------------------------

// preWait runs once per loop iteration before the ring wait. The mode is
// predicted from the same inputs the loop uses; the log says so.
func (w *Worker) probe908PreWait() {
	p := w.probe908
	if p == nil || p.done.Load() {
		return
	}
	hasPending := w.ring.Pending() > 0
	h, t := w.ring.BeginCQ()
	mode := "3b timed"
	switch {
	case w.sqpoll:
		mode = "sqpoll"
	case h != t && hasPending:
		mode = "1 submit+wait(CQEs ready)"
	case h != t:
		mode = "2 no syscall"
	case waitUntimed(hasPending, w.sendsPending, w.draining):
		mode = "3a UNTIMED submit+wait"
	}
	p.waitStart = time.Now()
	p.waitMode = mode
	p.waitModeA.Store(&mode)
	p.inWaitSince.Store(p.waitStart.UnixNano())
	p.nWait++
	now := time.Now()
	p.waitLog = p.nWait <= probe908WaitFirst || p.nWait%probe908EveryN == 0 || strings.HasPrefix(mode, "3a") ||
		(p.stalled && now.Sub(p.lastWaitLog) >= 100*time.Millisecond)
	if p.waitLog {
		p.lastWaitLog = now
	}
	if p.waitLog {
		p.logf("WAIT#%d pre: mode(predicted)=%s hasPending=%v cqReady=%d | %s", p.nWait, mode, hasPending, t-h, p.ringState())
	}
}

func (w *Worker) probe908PostWait(h, t uint32) {
	p := w.probe908
	if p == nil || p.done.Load() || p.waitStart.IsZero() {
		return
	}
	p.inWaitSince.Store(0)
	d := time.Since(p.waitStart)
	if p.waitLog || d >= 100*time.Millisecond {
		p.logf("WAIT#%d post: waited=%s mode=%s cqeBatch=%d | %s", p.nWait, d.Round(time.Microsecond), p.waitMode, t-h, p.ringState())
	}
	p.waitStart = time.Time{}
}

// ---- tick (worker thread, once per iteration after the CQE batch) ----------

func (w *Worker) probe908Tick() {
	p := w.probe908
	if p == nil {
		return
	}
	if p.done.Load() {
		w.probe908 = nil
		probe908Hub.remove(p)
		return
	}
	if w.conns[p.fd] != p.cs || p.cs.generation != p.gen {
		p.logf("CONN GONE from the conn table: %d CQEs seen, stalled=%v; final ring: %s", p.nCQE, p.stalled, p.ringState())
		p.done.Store(true)
		w.probe908 = nil
		probe908Hub.remove(p)
		return
	}
	now := time.Now().UnixNano()
	if !p.stalled && now-p.lastCQE >= probe908StallNs {
		p.stalled = true
		p.logf("STALL: no CQE for this conn for %s (after %d CQEs); every CQE is logged from here. state: %s | %s",
			time.Duration(now-p.lastCQE).Round(time.Millisecond), p.nCQE, p.sendState(p.cs), p.ringState())
	}
	if seq := probe908Hub.seq.Load(); seq != p.seenSeq {
		p.seenSeq = seq
		probe908Hub.mu.Lock()
		label := probe908Hub.label
		probe908Hub.mu.Unlock()
		txt := fmt.Sprintf("engine peer=%s w=%d fd=%d stalled=%v nCQE=%d | %s | %s | zcHoldBytes=%d draining=%v",
			p.peer, p.w.id, p.fd, p.stalled, p.nCQE, p.sendState(p.cs), p.ringState(), p.w.zcHoldBytes, p.w.draining)
		p.logf("REQ#%d %q answered: %s", seq, label, txt)
		p.reqMu.Lock()
		p.replyN[seq] = txt
		p.reqMu.Unlock()
	}
}

// ---- helper goroutine: thread-safe socket view + worker kicks -------------

func (p *probe908State) helper() {
	start := time.Now()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	n := 0
	for range tick.C {
		if p.done.Load() || time.Since(start) > probe908MaxLife {
			p.done.Store(true)
			return
		}
		if probe908Wake {
			p.w.wakeFD.Signal() // end an untimed ring wait so the tick runs
		}
		n++
		if n <= 8 || n%4 == 0 {
			snap := probe908SockSnap(p.fd, p.peer)
			fmt.Fprintf(os.Stderr, "PROBE908 peer=%s w=%d fd=%d %s SOCK server %s | ring(atomic, no wake): %s\n", p.peer, p.w.id, p.fd, p.ts(), snap, p.w.ring.probe908Counters())
			if strings.HasPrefix(snap, "fd ") || strings.HasPrefix(snap, "getpeername") {
				p.done.Store(true) // the descriptor is closed or reused: nothing left to look at
				return
			}
		}
	}
}

// ---- socket view (thread-safe: getsockopt/ioctl on a descriptor number) ----

func probe908SockaddrString(sa unix.Sockaddr) string {
	switch a := sa.(type) {
	case *unix.SockaddrInet4:
		return net.JoinHostPort(net.IP(a.Addr[:]).String(), fmt.Sprint(a.Port))
	case *unix.SockaddrInet6:
		return net.JoinHostPort(net.IP(a.Addr[:]).String(), fmt.Sprint(a.Port))
	}
	return fmt.Sprintf("%T", sa)
}

// probe908SockSnap describes the TCP socket behind fd: TCP_INFO (the fields
// that say whether bytes moved and who is limiting), TIOCOUTQ (unsent plus
// unacked), SIOCOUTQNSD (unsent), TIOCINQ, the socket buffers and the
// process's locked/pinned memory. wantPeer, when set, must equal the socket's
// peer address: the descriptor number can be reused after a close, and a
// snapshot of another socket is reported as such.
func probe908SockSnap(fd int, wantPeer string) string {
	if fd < 0 {
		return "no fd"
	}
	var b strings.Builder
	if sa, err := unix.Getpeername(fd); err != nil {
		return fmt.Sprintf("getpeername(%d): %v (closed or not connected)", fd, err)
	} else if peer := probe908SockaddrString(sa); wantPeer != "" && peer != wantPeer {
		return fmt.Sprintf("fd %d is another socket now (peer %s, want %s): closed and reused", fd, peer, wantPeer)
	}
	ti, err := unix.GetsockoptTCPInfo(fd, unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil {
		fmt.Fprintf(&b, "TCP_INFO err=%v", err)
	} else {
		fmt.Fprintf(&b, "TCP_INFO state=%d ca_state=%d retrans=%d unacked=%d sacked=%d lost=%d snd_mss=%d snd_cwnd=%d snd_ssthresh=%d snd_wnd=%d rcv_wnd=%d rcv_space=%d rcv_ssthresh=%d "+
			"notsent=%d bytes_sent=%d bytes_acked=%d bytes_received=%d bytes_retrans=%d segs_out=%d segs_in=%d delivered=%d rto=%d rtt=%d backoff=%d probes=%d total_retrans=%d "+
			"busy_us=%d rwnd_limited_us=%d sndbuf_limited_us=%d last_data_sent_ms=%d last_data_recv_ms=%d last_ack_recv_ms=%d pacing=%d",
			ti.State, ti.Ca_state, ti.Retransmits, ti.Unacked, ti.Sacked, ti.Lost, ti.Snd_mss, ti.Snd_cwnd, ti.Snd_ssthresh, ti.Snd_wnd, ti.Rcv_wnd, ti.Rcv_space, ti.Rcv_ssthresh,
			ti.Notsent_bytes, ti.Bytes_sent, ti.Bytes_acked, ti.Bytes_received, ti.Bytes_retrans, ti.Segs_out, ti.Segs_in, ti.Delivered, ti.Rto, ti.Rtt, ti.Backoff, ti.Probes, ti.Total_retrans,
			ti.Busy_time, ti.Rwnd_limited, ti.Sndbuf_limited, ti.Last_data_sent, ti.Last_data_recv, ti.Last_ack_recv, ti.Pacing_rate)
	}
	if v, err := unix.IoctlGetInt(fd, unix.TIOCOUTQ); err == nil {
		fmt.Fprintf(&b, " | SIOCOUTQ=%d", v)
	} else {
		fmt.Fprintf(&b, " | SIOCOUTQ err=%v", err)
	}
	if v, err := unix.IoctlGetInt(fd, sioCOUTQNSD); err == nil {
		fmt.Fprintf(&b, " SIOCOUTQNSD=%d", v)
	} else {
		fmt.Fprintf(&b, " SIOCOUTQNSD err=%v", err)
	}
	if v, err := unix.IoctlGetInt(fd, unix.TIOCINQ); err == nil {
		fmt.Fprintf(&b, " SIOCINQ=%d", v)
	}
	if v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF); err == nil {
		fmt.Fprintf(&b, " SO_SNDBUF=%d", v)
	}
	if v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF); err == nil {
		fmt.Fprintf(&b, " SO_RCVBUF=%d", v)
	}
	fmt.Fprintf(&b, " | proc: %s", probe908ProcStatus())
	return b.String()
}

// probe908ProcStatus returns the VmLck and VmPin lines of /proc/self/status:
// SEND_ZC pins pages against RLIMIT_MEMLOCK, and a pin that never drops
// would show here.
func probe908ProcStatus() string {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return err.Error()
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, "VmLck:") || strings.HasPrefix(ln, "VmPin:") {
			out = append(out, strings.Join(strings.Fields(ln), " "))
		}
	}
	return strings.Join(out, " ")
}

// probe908Note logs a decision line for cs when it is the probed conn.
func (w *Worker) probe908Note(cs *connState, format string, a ...any) {
	if p := w.probe908; p.is(cs) {
		p.logf(format, a...)
	}
}

// probe908ReplayFlush is flushSend as the adopt replay calls it, logging
// whether the replay itself issued the first send.
func (w *Worker) probe908ReplayFlush(cs *connState, where string) bool {
	p := w.probe908
	if !p.is(cs) {
		return w.flushSend(cs)
	}
	before := cs.sending
	sqFull := w.flushSend(cs)
	p.replayDecision(where, true, sqFull, before)
	return sqFull
}

// probe908Counters reads the shared ring indexes with atomic loads, safe from
// any goroutine: cq_head<cq_tail with the worker silent is a completion nobody
// consumed; sq_unconsumed>0 is an SQE the kernel was never asked to take.
func (r *Ring) probe908Counters() string {
	r.probe908Mu.RLock()
	defer r.probe908Mu.RUnlock()
	if r.probe908Gone {
		return "ring closed"
	}
	sqT := atomic.LoadUint32((*uint32)(r.sqTail))
	sqH := atomic.LoadUint32((*uint32)(r.sqHead))
	cqH := atomic.LoadUint32((*uint32)(r.cqHead))
	cqT := atomic.LoadUint32((*uint32)(r.cqTail))
	return fmt.Sprintf("sq_placed=%d sq_consumed=%d sq_unconsumed=%d cq_head=%d cq_tail=%d cq_unread=%d", sqT, sqH, int32(sqT-sqH), cqH, cqT, int32(cqT-cqH))
}
