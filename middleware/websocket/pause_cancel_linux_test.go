//go:build linux

package websocket

import (
	"bufio"
	"bytes"
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

	"github.com/goceleris/celeris"
)

// --- celeris#607 close-handshake probe -------------------------------------
//
// closeTimeout records only that the close handshake did not finish inside
// the client's window. It says nothing about WHERE it stopped, and three
// separate investigations (celeris#482, #519, #566) have started from that
// counter and gone in different directions. The probe below closes that gap
// by recording, per connection and joined across both sides by the client's
// local address:
//
//	client side  — when it sent Close, how many bytes it received afterwards,
//	               when the LAST of those bytes arrived, whether the server's
//	               Close frame (88 02 03 E8) was the final thing on the wire,
//	               and the error that ended the wait;
//	server side  — when the handler first and last read, how many bytes it
//	               echoed, when it observed the peer's Close, whether the
//	               automatic Close echo hit a write error, and when the
//	               handler goroutine exited.
//
// The two halves join through the ?id= query parameter carried in the
// handshake: Conn.RemoteAddr() is nil on the engine path, so the handler
// cannot learn the peer address, and the engine's own logs print
// raddr=cs.remoteAddr, which matches the client's LocalAddr. ADDRMAP lines
// carry that mapping so an engine-side log can be joined in too.
//
// Discipline (celeris#484, and the io_uring defect an every-event log took
// from 8/24 to 0/24): everything here is COLD. Per-read work is one integer
// add and — only inside the post-Close window — one time.Now() and an
// 8-byte copy. Nothing is logged until the subtest is over, and only for
// connections that did not close cleanly and promptly.
//
// The distinguishing question this answers: a connection whose bytes are
// still arriving when the window expires is a server that is BUSY (the
// deadline is too tight for the backlog this workload builds on purpose),
// whereas one that fell silent early and then waited is a server that is
// STUCK. Those want opposite fixes.

// closeBudget is how long the client waits for the server's FIN after
// sending its own Close, and closeSlow is the point past which that wait is
// worth a per-connection record. The defaults reproduce the shipped
// behaviour exactly (a 10s read deadline, no grace), so the probe does not
// move the failure it is measuring; WS482_CLOSE_BUDGET raises the ceiling
// for a measurement run, turning the boolean into a latency.
func closeBudget() time.Duration { return envDur("WS482_CLOSE_BUDGET", 10*time.Second) }
func closeSlow() time.Duration   { return envDur("WS482_CLOSE_SLOW", 10*time.Second) }

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// clientProbe is one connection's client-side timeline. Written only by that
// connection's own goroutine, read only after wg.Wait().
type clientProbe struct {
	laddr    string
	outcome  string // closedOK | rst | closeTimeout | tailFixFail | closeWriteFail | dialFail | hsFail
	dialedAt time.Duration
	floodEnd time.Duration
	// preDrain is the "a real WS client reads" loop that runs before Close.
	preDrainBytes int64
	preDrainEnd   time.Duration
	closeSentAt   time.Duration // absolute, since t0
	// Everything below is measured from the instant the Close frame was written.
	postBytes int64
	postReads int64
	// firstByteAt/lastByteAt bracket the post-Close stream. Both are
	// needed: lastByteAt alone cannot tell a connection that trickled for
	// twelve seconds from one that sat silent for eleven and then took a
	// burst, and those are opposite verdicts (busy server versus stalled
	// one). -1 when no byte ever arrived after Close.
	firstByteAt time.Duration
	lastByteAt  time.Duration
	endAt       time.Duration
	finalErr    string
	tail        [8]byte
	tailLen     int
	writeErr    string // the error that ended a failed writeAll, if any
	// Snapshots taken at the instant the wait ended, while both sockets
	// are still open. Post-mortem is useless here: the client defers
	// Close, so by dump time the kernel has forgotten the connection and
	// the reader has been torn down.
	sock sockPair
	rdr  readerState
}

// sockPair is what the kernel says about both ends of one connection,
// read from /proc/net/tcp. It is the layer below every counter in this
// test: when a peer reports that the close handshake never finished, the
// first fork in the road is whether the bytes are still sitting in the
// SERVER's receive queue -- the engine is not reading a socket that has
// data -- or whether that queue is empty, in which case nothing was lost
// below the middleware. No counter in this oracle could see that.
type sockPair struct {
	ok                       bool
	clientState, serverState string
	clientTx, clientRx       int64
	serverTx, serverRx       int64
}

// tcpState names the /proc/net/tcp st column.
var tcpState = map[string]string{
	"01": "ESTABLISHED", "02": "SYN_SENT", "03": "SYN_RECV", "04": "FIN_WAIT1",
	"05": "FIN_WAIT2", "06": "TIME_WAIT", "07": "CLOSE", "08": "CLOSE_WAIT",
	"09": "LAST_ACK", "0A": "LISTEN", "0B": "CLOSING",
}

// snapshotSockets reads /proc/net/tcp and returns both ends of the
// connection whose CLIENT side is bound to localPort. Cold: called only
// for a connection whose close wait already went wrong.
func snapshotSockets(localPort int) sockPair {
	var sp sockPair
	b, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return sp
	}
	want := fmt.Sprintf(":%04X", localPort)
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] == "sl" {
			continue
		}
		local, rem, st, q := f[1], f[2], f[3], f[4]
		tx, rx := int64(-1), int64(-1)
		if i := strings.IndexByte(q, ':'); i > 0 {
			tx, _ = strconv.ParseInt(q[:i], 16, 64)
			rx, _ = strconv.ParseInt(q[i+1:], 16, 64)
		}
		name := tcpState[strings.ToUpper(st)]
		if name == "" {
			name = st
		}
		switch {
		case strings.HasSuffix(local, want):
			sp.ok, sp.clientState, sp.clientTx, sp.clientRx = true, name, tx, rx
		case strings.HasSuffix(rem, want):
			sp.ok, sp.serverState, sp.serverTx, sp.serverRx = true, name, tx, rx
		}
	}
	return sp
}

func (p *clientProbe) sawServerClose() bool {
	t := p.tail[:p.tailLen]
	return bytes.HasSuffix(t, []byte{0x88, 0x02, 0x03, 0xE8}) || bytes.HasSuffix(t, []byte{0x88, 0x00})
}

// noteTail keeps the last 8 bytes of the post-Close stream. The server's
// Close reply is the last frame it writes, so the tail says whether the
// reply arrived — without parsing the stream on the hot path.
func (p *clientProbe) noteTail(b []byte) {
	if len(b) >= len(p.tail) {
		copy(p.tail[:], b[len(b)-len(p.tail):])
		p.tailLen = len(p.tail)
		return
	}
	keep := p.tailLen + len(b)
	if keep > len(p.tail) {
		copy(p.tail[:], p.tail[keep-len(p.tail):p.tailLen])
		p.tailLen = len(p.tail) - len(b)
	}
	copy(p.tail[p.tailLen:], b)
	p.tailLen += len(b)
}

// handlerProbe is one connection's server-side timeline, recorded by the
// handler goroutine at exit. Handler goroutines outlive the subtest, so the
// record is published under a mutex and never touches *testing.T.
type handlerProbe struct {
	set         bool
	firstReadAt time.Duration
	lastReadAt  time.Duration
	reads       int64
	echoBytes   int64
	readErr     string
	readErrAt   time.Duration
	sawClose    bool
	closeEcho   string // error from the automatic Close-frame echo, "" = none
	exitAt      time.Duration
}

// readerState is the middleware's own view of a connection's inbound
// backpressure, sampled cold at dump time. It is the line between two
// verdicts that closeTimeout cannot tell apart: an EMPTY channel with
// paused=true means the chanReader never lifted its own pause, while an
// empty channel with paused=false means the middleware did ask the engine
// to resume and no byte followed — the engine dropped the resume.
type readerState struct {
	ok              bool
	depth, capacity int
	spill           int64
	paused          bool
	closed          bool
	dropped         uint64
	spilled         uint64
	high, low       int
	wsClosed        bool
	wsCloseSent     bool
}

func snapshotReader(c *Conn) readerState {
	var s readerState
	if c == nil {
		return s
	}
	s.wsClosed, s.wsCloseSent = c.closed.Load(), c.closeSent.Load()
	r := c.engineReader
	if r == nil {
		return s
	}
	s.ok = true
	s.depth, s.capacity = len(r.ch), cap(r.ch)
	s.spill = r.spillLen.Load()
	s.closed = r.closed.Load()
	s.dropped, s.spilled = r.dropped.Load(), r.spilled.Load()
	s.high, s.low = r.highWater, r.lowWater
	r.pausedMu.Lock()
	s.paused = r.pausedState
	r.pausedMu.Unlock()
	return s
}

// TestBackpressurePauseDoesNotCancelInflightSend is the celeris#482
// regression guard.
//
// The engine pauses inbound delivery for a WebSocket conn by cancelling its
// armed recv. On io_uring that cancel used to be keyed by RAW FD with
// IORING_ASYNC_CANCEL_FD|CANCEL_ALL, which matches EVERY op on the socket --
// including a poll-armed SEND blocked on a full peer buffer. handleSend has
// no -ECANCELED case, so the healthy connection was closed mid-write with
// syscall.ECANCELED surfacing to the handler; on the SEND_ZC path the close
// then stalled and the conn leaked as a paused ESTAB socket that never saw
// the peer's FIN.
//
// The workload is the one that reproduces it on the cluster: clients that
// blast small frames and NEVER read (so the server's echo send blocks and
// goes poll-armed) while inbound outpaces the echo handler (so chanReader
// crosses high-water and requests the pause). A small backpressure buffer
// makes the pause fire often per burst.
//
// Three oracles, all must hold on every engine:
//  1. the handler never observes ECANCELED from WriteMessage;
//  2. every conn still completes a Close handshake afterwards (a paused
//     conn whose recv was never re-armed cannot read the Close frame and
//     the client times out instead of seeing EOF);
//  3. engine shutdown completes within a bound (paused zombies block it).
//
// NOTE: inbound stream integrity is verified by the sequence-oracle test
// (TestBackpressureInboundSequenceIntegrity, #484); this test asserts the #482
// fix: no ECANCELED on in-flight sends, clean close handshakes, and clean shutdown.
func TestBackpressurePauseDoesNotCancelInflightSend(t *testing.T) {
	if testing.Short() {
		t.Skip("needs ~20s of loopback flood")
	}
	conns := envInt("WS482_CONNS", 96)
	bpBuf := envInt("WS482_BP", 256)
	bursts := envInt("WS482_BURSTS", 4)
	perBurst := envInt("WS482_BURST_BYTES", 2<<20)
	budget, slow := closeBudget(), closeSlow()

	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			t0 := time.Now()
			since := func() time.Duration { return time.Since(t0) }

			var ecanceled, otherWriteErr, protoErr atomic.Int64
			// protoErr counts every read error that is not a close, so on its
			// own it cannot distinguish a frame the engine mis-delivered from
			// a chanReader that hit ErrReadLimit because the asynchronous
			// pause overshot its buffer. Those want opposite fixes. Record
			// the errors and print them with the verdict; handler goroutines
			// outlive the subtest, so they must never touch t directly.
			var readErrMu sync.Mutex
			readErrs := map[string]int{}
			noteReadErr := func(err error) {
				readErrMu.Lock()
				if len(readErrs) < 16 {
					readErrs[err.Error()]++
				}
				readErrMu.Unlock()
			}

			// Server-side half of the celeris#607 probe.
			var hpMu sync.Mutex
			hprobe := make([]handlerProbe, conns)
			hconn := make([]*Conn, conns)
			var handlersLive atomic.Int64
			register := func(id int, c *Conn) {
				if id < 0 || id >= conns {
					return
				}
				hpMu.Lock()
				hconn[id] = c
				hpMu.Unlock()
			}
			publish := func(id int, hp handlerProbe) {
				if id < 0 || id >= conns {
					return
				}
				hp.set = true
				hpMu.Lock()
				hprobe[id] = hp
				hpMu.Unlock()
			}

			addr, shutdownEngine := startNativeServer(t, kind, Config{
				CheckOrigin:           func(*celeris.Context) bool { return true },
				ReadLimit:             256 * 1024,
				MaxBackpressureBuffer: bpBuf, // realistic buffer; headroom (cap-highWater) must exceed async pause-apply latency, else Append drops a chunk (ErrReadLimit) -- a config artifact, not engine reordering
				Handler: func(c *Conn) {
					// Conn.RemoteAddr() is nil on the engine path, so the
					// join key travels in the handshake query instead.
					id, _ := strconv.Atoi(c.Query("id"))
					handlersLive.Add(1)
					register(id, c)
					var hp handlerProbe
					hp.firstReadAt = -1
					defer func() {
						hp.exitAt = since()
						if v := c.closeEchoErr.Load(); v != nil {
							hp.closeEcho = v.(storedWriteErr).err.Error()
						}
						publish(id, hp)
						handlersLive.Add(-1)
					}()
					for {
						mt, msg, err := c.ReadMessage()
						if err != nil {
							hp.readErrAt = since()
							hp.readErr = err.Error()
							hp.sawClose = isCloseErr(err)
							if !isCloseErr(err) {
								protoErr.Add(1)
								noteReadErr(err)
							}
							return
						}
						if hp.firstReadAt < 0 {
							hp.firstReadAt = since()
						}
						hp.reads++
						if err := c.WriteMessage(mt, msg); err != nil {
							hp.readErrAt = since()
							hp.readErr = "write: " + err.Error()
							if errors.Is(err, syscall.ECANCELED) {
								ecanceled.Add(1)
							} else if !errors.Is(err, ErrWriteClosed) {
								otherWriteErr.Add(1)
							}
							return
						}
						// Sampled, not per-frame: this loop runs ~15k times
						// per connection and a clock read on every pass is
						// the hot-path probe that took a prior io_uring
						// defect from 8/24 to 0/24 (celeris#484). Every 64th
						// frame is sub-millisecond resolution on a workload
						// whose stalls are measured in seconds.
						if hp.reads&63 == 0 {
							hp.lastReadAt = since()
						}
						hp.echoBytes += int64(len(msg))
					}
				},
			})
			// Shutdown is asserted, not deferred: a set of paused zombie conns
			// whose recv was never re-armed also blocks graceful engine
			// shutdown, so an unbounded deferred shutdown turns the failure
			// into a whole-binary timeout instead of a precise assertion.
			shutdownDone := make(chan struct{})
			defer func() {
				go func() { shutdownEngine(); close(shutdownDone) }()
				select {
				case <-shutdownDone:
				case <-time.After(20 * time.Second):
					t.Errorf("engine shutdown did not complete within 20s: paused connections " +
						"that were never re-armed are blocking graceful shutdown (celeris#482)")
				}
			}()

			hostPort := strings.TrimPrefix(addr, "ws://")
			hostPort = strings.TrimSuffix(hostPort, "/ws")

			var closedOK, closeTimeout, dialFail, hsFail, clientCloseFail, clientMisaligned, framesSent atomic.Int64
			var slowClose atomic.Int64
			// An RST is NOT a clean close (celeris#530). Linux emits one when
			// a socket is closed with unread data still in its receive queue,
			// which is the signature of a connection torn down mid-stream —
			// one of the failure modes this oracle exists to catch. Folding
			// it into closedOK scored a hard teardown as success.
			var clientRST atomic.Int64
			cprobe := make([]clientProbe, conns)
			var wg sync.WaitGroup
			batch := maskedTextFrames(2048, 120)
			dialer := net.Dialer{Timeout: 3 * time.Second, Control: func(_, _ string, rc syscall.RawConn) error {
				var serr error
				_ = rc.Control(func(fd uintptr) {
					// Slow consumer: a tiny receive buffer makes the server's
					// echo SEND block (poll-armed) almost immediately.
					serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 32<<10)
				})
				return serr
			}}
			for i := 0; i < conns; i++ {
				i := i
				wg.Add(1)
				go func() {
					defer wg.Done()
					p := &cprobe[i]
					p.firstByteAt, p.lastByteAt = -1, -1
					c, err := dialer.Dial("tcp", hostPort)
					if err != nil {
						dialFail.Add(1)
						p.outcome, p.writeErr = "dialFail", err.Error()
						return
					}
					defer func() { _ = c.Close() }()
					p.laddr = c.LocalAddr().String()
					p.dialedAt = since()
					if err := wsHandshakeID(c, hostPort, i); err != nil {
						hsFail.Add(1)
						p.outcome, p.writeErr = "hsFail", err.Error()
						return
					}
					wrote := 0
					writeSome := func(deadline time.Duration) (int, error) {
						_ = c.SetWriteDeadline(time.Now().Add(deadline))
						n, err := c.Write(batch[wrote%len(batch):])
						wrote += n
						return n, err
					}
					// Flood without reading: builds backpressure so the server's echo SEND goes
					// poll-armed. Partial writes are fine -- wrote tracks the exact wire position.
					for b := 0; b < bursts; b++ {
						target := wrote + perBurst
						for wrote < target {
							if _, err := writeSome(2 * time.Second); err != nil {
								break
							}
						}
						time.Sleep(200 * time.Millisecond)
					}
					p.floodEnd = since()
					// Complete the current frame so the wire is a whole number of frames. writeAll
					// retries to completion: once the flood stops the server drains and the send
					// buffer empties. A conn the server wrongly killed surfaces as an error here.
					if rem := wrote % 126; rem != 0 {
						need := 126 - rem
						start := wrote % len(batch)
						if err := writeAllErr(c, batch[start:start+need], 15*time.Second); err != nil {
							clientCloseFail.Add(1)
							p.outcome, p.writeErr = "tailFixFail", err.Error()
							if _, ps, perr := net.SplitHostPort(p.laddr); perr == nil {
								if pn, cerr := strconv.Atoi(ps); cerr == nil {
									p.sock = snapshotSockets(pn)
								}
							}
							hpMu.Lock()
							if i < len(hconn) {
								p.rdr = snapshotReader(hconn[i])
							}
							hpMu.Unlock()
							return
						}
						wrote += need
					}
					// Drain the echo backlog (a real WS client reads); relieves backpressure.
					buf := make([]byte, 64<<10)
					for {
						_ = c.SetReadDeadline(time.Now().Add(1 * time.Second))
						n, err := c.Read(buf)
						p.preDrainBytes += int64(n)
						if err != nil {
							break
						}
					}
					p.preDrainEnd = since()
					if wrote%126 != 0 {
						clientMisaligned.Add(1)
					}
					framesSent.Add(int64(wrote / 126))
					if err := writeAllErr(c, maskedCloseFrame(), 10*time.Second); err != nil {
						clientCloseFail.Add(1)
						p.outcome, p.writeErr = "closeWriteFail", err.Error()
						return
					}
					_ = c.SetReadDeadline(time.Now().Add(budget))
					tClose := time.Now()
					p.closeSentAt = since()
					for {
						n, err := c.Read(buf)
						if n > 0 {
							p.postBytes += int64(n)
							p.postReads++
							if p.firstByteAt < 0 {
								p.firstByteAt = time.Since(tClose)
							}
							p.lastByteAt = time.Since(tClose)
							p.noteTail(buf[:n])
						}
						if err == nil {
							continue
						}
						d := time.Since(tClose)
						p.endAt, p.finalErr = d, err.Error()
						if errors.Is(err, syscall.ECONNRESET) {
							clientRST.Add(1)
							p.outcome = "rst"
						} else if errors.Is(err, io.EOF) {
							closedOK.Add(1)
							p.outcome = "closedOK"
						} else {
							closeTimeout.Add(1)
							p.outcome = "closeTimeout"
							t.Logf("close-timeout %s: %v after Close sent", c.LocalAddr(), d.Round(time.Millisecond))
						}
						if d > slow && p.outcome != "closeTimeout" {
							slowClose.Add(1)
						}
						// Cold: only a connection that already went wrong
						// pays for this, and both sockets are still open
						// here, which is the only moment the kernel can
						// still answer.
						if p.outcome != "closedOK" || d > slow {
							if _, ps, perr := net.SplitHostPort(p.laddr); perr == nil {
								if pn, cerr := strconv.Atoi(ps); cerr == nil {
									p.sock = snapshotSockets(pn)
								}
							}
							hpMu.Lock()
							if i < len(hconn) {
								p.rdr = snapshotReader(hconn[i])
							}
							hpMu.Unlock()
						}
						return
					}
				}()
			}
			wg.Wait()

			// Drain the handlers before reading the server-side half of the
			// probe, and report how long that took: #568 established that a
			// handler finishing IS the server closing its side, so "every
			// handler completed" is itself evidence (celeris#566).
			drainStart := time.Now()
			for handlersLive.Load() > 0 && time.Since(drainStart) < 25*time.Second {
				time.Sleep(20 * time.Millisecond)
			}
			handlerDrain := time.Since(drainStart)

			t.Logf("%s: clientMisaligned=%d framesSent=%d (if misaligned>0 the client truncated; if 0 while protocol errors>0 the engine mis-delivered)",
				kind, clientMisaligned.Load(), framesSent.Load())
			if clientMisaligned.Load() > 0 {
				t.Errorf("%d client conn(s) ended mid-frame -- test client bug, not a server verdict", clientMisaligned.Load())
			}

			t.Logf("%s: conns=%d protoErr=%d clientCloseFail=%d ecanceled=%d otherWriteErr=%d closedOK=%d clientRST=%d closeTimeout=%d dialFail=%d hsFail=%d",
				kind, conns, protoErr.Load(), clientCloseFail.Load(), ecanceled.Load(), otherWriteErr.Load(), closedOK.Load(), clientRST.Load(), closeTimeout.Load(), dialFail.Load(), hsFail.Load())
			t.Logf("%s: closeBudget=%v closeSlow=%v slowClose=%d handlersLive=%d after %v",
				kind, budget, slow, slowClose.Load(), handlersLive.Load(), handlerDrain.Round(time.Millisecond))
			hpMu.Lock()
			dumpCloseProbe(t, kind.String(), cprobe, hprobe, hconn, slow)
			hpMu.Unlock()

			if dialFail.Load()+hsFail.Load() > 0 {
				t.Fatalf("%d conns failed to dial/handshake -- environment problem, not a verdict", dialFail.Load()+hsFail.Load())
			}
			if n := ecanceled.Load(); n != 0 {
				t.Errorf("%d WebSocket handler(s) observed ECANCELED from WriteMessage: the recv-pause "+
					"cancel killed an in-flight SEND (celeris#482)", n)
			}
			// protoErr and otherWriteErr were logged but never asserted, so a
			// server that killed a healthy connection mid-write passed on
			// these counters alone (celeris#530). Measured zero on both
			// engines across repeated runs before this assertion was added.
			if n := protoErr.Load(); n != 0 {
				readErrMu.Lock()
				for msg, count := range readErrs {
					t.Logf("%s: read error x%d: %s", kind, count, msg)
				}
				readErrMu.Unlock()
				t.Errorf("%d handler(s) saw a protocol error on the read side: the engine "+
					"mis-delivered or tore down a healthy connection", n)
			}
			// Measured zero on both engines across repeated runs before this
			// was asserted: nothing was actually being hidden inside closedOK
			// here, unlike the sibling inbound oracle where the same folding
			// masked a connection that lost 12,735 frames. Asserting it keeps
			// the distinction from decaying back.
			if n := clientRST.Load(); n != 0 {
				t.Errorf("%d conn(s) were RESET rather than closed cleanly: Linux emits RST when a "+
					"socket is closed with unread data still queued, which is a connection torn "+
					"down mid-stream, not a clean close (celeris#530)", n)
			}
			if n := otherWriteErr.Load(); n != 0 {
				t.Errorf("%d handler(s) saw a non-ECANCELED write error: the engine failed a "+
					"WriteMessage on a connection it should have kept alive", n)
			}
			if n := closeTimeout.Load(); n != 0 {
				// Deliberately does NOT name a cause. This counter only says
				// the server never closed its side within the client's
				// window; it cannot distinguish a conn whose recv stayed
				// paused (celeris#482) from one whose send accounting
				// desynchronised so the dirty-list flush skipped it forever
				// (celeris#519), and asserting the former sent three separate
				// investigations down the wrong path -- in the #519 failures
				// nothing was paused at all. The CLOSEPROBE lines above carry
				// the discriminator (celeris#607): a connection still
				// receiving bytes when the window expired was waiting on a
				// busy server, not a stuck one.
				t.Errorf("%d conn(s) never completed the Close handshake within the client's read window "+
					"after sending Close: "+
					"the server never closed its side. Cause is NOT implied by this counter -- read the "+
					"CLOSEPROBE record for each address above, and dump the engine's per-conn state "+
					"(recvPaused/recvArmed/sending/dirty/closing) to tell a "+
					"stuck pause (celeris#482) from stranded send accounting (celeris#519)",
					n)
			}
		})
	}
}

// dumpCloseProbe prints the per-connection close record for every connection
// that did not close cleanly and promptly, plus the ADDRMAP line that joins a
// record to the engine's own raddr= logging. Cold: it runs once, after the
// subtest's traffic is over, and prints nothing when every connection was
// clean.
func dumpCloseProbe(t *testing.T, kind string, cp []clientProbe, hp []handlerProbe, hc []*Conn, slow time.Duration) {
	t.Helper()
	ids := make([]int, 0, 8)
	for i := range cp {
		p := &cp[i]
		interesting := p.outcome != "closedOK" || p.endAt > slow
		if interesting {
			ids = append(ids, i)
		}
	}
	if len(ids) == 0 {
		return
	}
	sort.Slice(ids, func(a, b int) bool { return cp[ids[a]].endAt > cp[ids[b]].endAt })
	if len(ids) > 24 {
		ids = ids[:24]
	}
	for _, i := range ids {
		p := &cp[i]
		t.Logf("%s ADDRMAP conn %d laddr=%s", kind, i, p.laddr)
		firstByte, lastByte := "never", "never"
		if p.lastByteAt >= 0 {
			firstByte = p.firstByteAt.Round(time.Millisecond).String()
			lastByte = p.lastByteAt.Round(time.Millisecond).String()
		}
		t.Logf("%s CLOSEPROBE conn %d laddr=%s outcome=%s closeSentAt=%v wait=%v "+
			"postBytes=%d postReads=%d firstByteAfterClose=%s lastByteAfterClose=%s serverCloseFrameSeen=%v tail=%x finalErr=%q "+
			"preDrainBytes=%d floodEnd=%v preDrainEnd=%v writeErr=%q",
			kind, i, p.laddr, p.outcome,
			p.closeSentAt.Round(time.Millisecond), p.endAt.Round(time.Millisecond),
			p.postBytes, p.postReads, firstByte, lastByte, p.sawServerClose(), p.tail[:p.tailLen], p.finalErr,
			p.preDrainBytes, p.floodEnd.Round(time.Millisecond), p.preDrainEnd.Round(time.Millisecond), p.writeErr)
		if p.sock.ok {
			t.Logf("%s SOCKPROBE conn %d laddr=%s client=%s tx=%d rx=%d | server=%s tx=%d rx=%d "+
				"(server rx>0 with an idle handler = the engine is not reading a socket that has data)",
				kind, i, p.laddr, p.sock.clientState, p.sock.clientTx, p.sock.clientRx,
				p.sock.serverState, p.sock.serverTx, p.sock.serverRx)
		}
		if p.rdr.ok {
			t.Logf("%s LIVEREADER conn %d laddr=%s depth=%d/%d high=%d low=%d spill=%d paused=%v "+
				"readerClosed=%v spilled=%d dropped=%d wsClosed=%v wsCloseSent=%v (sampled AT the failure, not after)",
				kind, i, p.laddr, p.rdr.depth, p.rdr.capacity, p.rdr.high, p.rdr.low, p.rdr.spill,
				p.rdr.paused, p.rdr.closed, p.rdr.spilled, p.rdr.dropped, p.rdr.wsClosed, p.rdr.wsCloseSent)
		}
		h := &hp[i]
		if !h.set {
			t.Logf("%s HANDLERPROBE conn %d laddr=%s STILL RUNNING at dump time", kind, i, p.laddr)
		} else {
			t.Logf("%s HANDLERPROBE conn %d laddr=%s firstRead=%v lastRead=%v reads=%d echoBytes=%d "+
				"sawClose=%v readErrAt=%v readErr=%q closeEchoErr=%q exitAt=%v",
				kind, i, p.laddr, h.firstReadAt.Round(time.Millisecond), h.lastReadAt.Round(time.Millisecond),
				h.reads, h.echoBytes, h.sawClose, h.readErrAt.Round(time.Millisecond), h.readErr, h.closeEcho,
				h.exitAt.Round(time.Millisecond))
		}
		if i < len(hc) {
			if s := snapshotReader(hc[i]); s.ok {
				t.Logf("%s READERPROBE conn %d laddr=%s depth=%d/%d high=%d low=%d spill=%d paused=%v "+
					"readerClosed=%v spilled=%d dropped=%d wsClosed=%v wsCloseSent=%v",
					kind, i, p.laddr, s.depth, s.capacity, s.high, s.low, s.spill, s.paused,
					s.closed, s.spilled, s.dropped, s.wsClosed, s.wsCloseSent)
			}
		}
	}
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func wsHandshake(c net.Conn, hostPort string) error {
	return wsHandshakePath(c, hostPort, "/ws")
}

// wsHandshakeID upgrades on /ws?id=N. The id is the only join key available
// to an engine-path handler: the connection is detached from the HTTP layer
// and Conn.RemoteAddr() returns nil there, so nothing else identifies which
// client a handler is serving (celeris#607).
func wsHandshakeID(c net.Conn, hostPort string, id int) error {
	return wsHandshakePath(c, hostPort, "/ws?id="+strconv.Itoa(id))
}

func wsHandshakePath(c net.Conn, hostPort, path string) error {
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	req := "GET " + path + " HTTP/1.1\r\nHost: " + hostPort + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
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

// maskedTextFrames pre-encodes n client->server text frames of plen bytes.
func maskedTextFrames(n, plen int) []byte {
	out := make([]byte, 0, n*(6+plen))
	m := [4]byte{0x11, 0x22, 0x33, 0x44}
	for i := 0; i < n; i++ {
		out = append(out, 0x81, 0x80|byte(plen), m[0], m[1], m[2], m[3])
		for j := 0; j < plen; j++ {
			out = append(out, 'x'^m[j%4])
		}
	}
	return out
}

// writeAll writes every byte of buf, retrying partial writes until done or the
// overall deadline. A frame written this way is never truncated, so the stream
// stays well-formed regardless of backpressure timing. Returns false on error
// or deadline (e.g. the server killed the conn -- the celeris#482 symptom).
func writeAll(c net.Conn, buf []byte, within time.Duration) bool {
	return writeAllErr(c, buf, within) == nil
}

// writeAllErr is writeAll keeping the error. A client that cannot finish
// writing never reaches the close handshake at all, so which error stopped
// it is the difference between "the server tore this conn down" and "the
// server is merely too slow to drain it" (celeris#607).
func writeAllErr(c net.Conn, buf []byte, within time.Duration) error {
	end := time.Now().Add(within)
	for len(buf) > 0 {
		_ = c.SetWriteDeadline(end)
		n, err := c.Write(buf)
		buf = buf[n:]
		if err != nil {
			if len(buf) == 0 {
				return nil
			}
			return err
		}
	}
	return nil
}

// maskedCloseFrame is a client->server Close (opcode 8) with status 1000.
func maskedCloseFrame() []byte {
	m := [4]byte{0x11, 0x22, 0x33, 0x44}
	return []byte{0x88, 0x82, m[0], m[1], m[2], m[3], 0x03 ^ m[0], 0xE8 ^ m[1]}
}
