//go:build linux

package websocket

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

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
// Oracles, all must hold on every engine:
//  1. the handler never observes ECANCELED from WriteMessage;
//  2. every conn finishes its last frame and its Close frame, and then
//     completes the Close handshake (a paused conn whose recv was never
//     re-armed cannot read them, and the client gives up instead);
//  3. no handler sees a write or read error before its own client closed;
//  4. engine shutdown completes within a bound (paused zombies block it).
//
// The client waits on progress, not on a clock, and reads while it waits:
// see backpressure_oracle_linux_test.go for why (celeris#633). A connection
// that never finishes its writes used to be counted and not judged
// (clientCloseFail, celeris#623); it now fails the test, and every give-up
// prints both ends' timeline (WSO-GIVEUP).
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

	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			rig := newWSORig()
			var ecanceled, otherWriteErr, protoErr atomic.Int64
			// protoErr counts every read error that is not a close, so on its
			// own it cannot distinguish a frame the engine mis-delivered from
			// a chanReader that hit ErrReadLimit because the asynchronous
			// pause overshot its buffer. Those want opposite fixes. The rig
			// records each error with its time and its connection, and they
			// are printed with the verdict; handler goroutines outlive the
			// subtest, so they must never touch t directly.
			addr, shutdownEngine, srv := startNativeServerWithHandle(t, kind, Config{
				CheckOrigin:           func(*celeris.Context) bool { return true },
				ReadLimit:             256 * 1024,
				MaxBackpressureBuffer: bpBuf, // realistic buffer; headroom (cap-highWater) must exceed async pause-apply latency, else Append drops a chunk (ErrReadLimit) -- a config artifact, not engine reordering
				Handler: func(c *Conn) {
					s := rig.attach(c)
					defer s.exited("returned", nil)
					for {
						s.phase.Store(1)
						mt, msg, err := c.ReadMessage()
						if err != nil {
							if !isCloseErr(err) {
								protoErr.Add(1)
								s.noteErr("read", err)
							}
							s.exited("read", err)
							return
						}
						s.phase.Store(2)
						if err := c.WriteMessage(mt, msg); err != nil {
							if errors.Is(err, syscall.ECANCELED) {
								ecanceled.Add(1)
							} else if !errors.Is(err, ErrWriteClosed) {
								otherWriteErr.Add(1)
								s.noteErr("write", err)
							}
							s.exited("write", err)
							return
						}
						s.echoed(len(msg))
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
				// Recv-arming witnesses, read after shutdown so every worker
				// has left its loop and no stall episode is still open. They
				// are direct atomics, so nothing is stranded in a
				// per-iteration batch. RECVSTALL is the celeris#607 witness:
				// a connection owed a recv arm that the dirty-list retry
				// passed over because a SEND was outstanding. Logged, not
				// asserted -- an episode is normal SQ-ring pressure; it is
				// the DURATION, joined against closeTimeout above, that
				// carries the verdict.
				if kind != celeris.IOUring {
					return
				}
				info := srv.EngineInfo()
				if info == nil {
					t.Errorf("EngineInfo() is nil after shutdown; cannot read the recv-arming witnesses")
					return
				}
				m := info.Metrics
				t.Logf("%s: RECVSTALL sqFull=%d episodes=%d totalMs=%d maxMs=%d armDeclined=%d resumeWhileCancelPending=%d doubleArmed=%d cqeUnaccounted=%d",
					kind, m.RecvSQFull, m.RecvStallEpisodes,
					m.RecvStallNanos/1e6, m.RecvStallMaxNanos/1e6,
					m.RecvArmDeclined, m.RecvResumeWhileCancelPending,
					m.RecvDoubleArmed, m.RecvCQEUnaccounted)
				t.Logf("%s: LINKBLOCK arms=%d blockedTotalMs=%d blockedMaxMs=%d",
					kind, m.RecvLinkedArms, m.RecvLinkedBlockedNanos/1e6,
					m.RecvLinkedBlockedMaxNanos/1e6)
			}()

			hostPort := strings.TrimPrefix(addr, "ws://")
			hostPort = strings.TrimSuffix(hostPort, "/ws")
			rig.setServer(hostPort)

			var closedOK, closeTimeout, dialFail, hsFail, clientCloseFail, clientMisaligned, framesSent atomic.Int64
			// An RST is NOT a clean close (celeris#530). Linux emits one when
			// a socket is closed with unread data still in its receive queue,
			// which is the signature of a connection torn down mid-stream —
			// one of the failure modes this oracle exists to catch. Folding
			// it into closedOK scored a hard teardown as success.
			var clientRST atomic.Int64
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
				wg.Add(1)
				go func() {
					defer wg.Done()
					c, err := dialer.Dial("tcp", hostPort)
					if err != nil {
						dialFail.Add(1)
						return
					}
					cl := rig.client(c)
					defer rig.closeClient(cl, c)
					if err := wsHandshakePath(c, hostPort, cl.path()); err != nil {
						hsFail.Add(1)
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
					cl.mark("flood end wrote=%d", wrote)
					cl.sample(c, "flood end")
					buf := make([]byte, 64<<10)
					// Complete the current frame so the wire is a whole number of frames,
					// waiting on progress and reading while it waits. A conn the server
					// wrongly killed or wedged surfaces here, with its timeline.
					if rem := wrote % 126; rem != 0 {
						need := 126 - rem
						start := wrote % len(batch)
						if w := cl.write(c, "fc", batch[start:start+need], buf); !w.ok {
							clientCloseFail.Add(1)
							cl.giveUp(t, c, kind.String(), "gave up finishing its last frame: "+w.String())
							return
						}
						wrote += need
					}
					// Drain the echo backlog (a real WS client reads); relieves backpressure.
					for {
						_ = c.SetReadDeadline(time.Now().Add(1 * time.Second))
						if _, err := c.Read(buf); err != nil {
							break
						}
					}
					if wrote%126 != 0 {
						clientMisaligned.Add(1)
					}
					framesSent.Add(int64(wrote / 126))
					cl.mark("drained; writing Close")
					if w := cl.write(c, "cw", maskedCloseFrame(), buf); !w.ok {
						clientCloseFail.Add(1)
						cl.giveUp(t, c, kind.String(), "gave up writing its Close frame: "+w.String())
						return
					}
					cl.mark("Close sent")
					switch r := cl.closeWait(c, buf); r.outcome {
					case "eof":
						closedOK.Add(1)
						if r.maxGap > wsoCloseSlow {
							t.Logf("%s: close after a silent stretch longer than %v, conn %s: %s", kind, wsoCloseSlow, c.LocalAddr(), r)
						}
					case "rst":
						clientRST.Add(1)
						cl.giveUp(t, c, kind.String(), "was RESET instead of closed: "+r.String())
					default:
						closeTimeout.Add(1)
						cl.giveUp(t, c, kind.String(), "gave up waiting for the server's close: "+r.String())
					}
				}()
			}
			wg.Wait()

			t.Logf("%s: clientMisaligned=%d framesSent=%d (if misaligned>0 the client truncated; if 0 while protocol errors>0 the engine mis-delivered)",
				kind, clientMisaligned.Load(), framesSent.Load())
			if clientMisaligned.Load() > 0 {
				t.Errorf("%d client conn(s) ended mid-frame -- test client bug, not a server verdict", clientMisaligned.Load())
			}

			// A server error that came after its own client closed is that
			// client's teardown echoing back, not the engine's doing
			// (celeris#633: 1,165 of 1,165 such errors came after). Only the
			// ones before are judged; the rest are printed.
			errsBefore, errsAfter := rig.serverErrs()
			var writeBefore, readBefore int64
			for _, e := range errsBefore {
				if e.kind == "write" {
					writeBefore++
				} else {
					readBefore++
				}
			}
			t.Logf("%s: conns=%d protoErr=%d clientCloseFail=%d ecanceled=%d otherWriteErr=%d closedOK=%d clientRST=%d closeTimeout=%d dialFail=%d hsFail=%d serverErrsAfterClientClose=%d protoErrAll=%d otherWriteErrAll=%d",
				kind, conns, readBefore, clientCloseFail.Load(), ecanceled.Load(), writeBefore, closedOK.Load(), clientRST.Load(), closeTimeout.Load(), dialFail.Load(), hsFail.Load(),
				len(errsAfter), protoErr.Load(), otherWriteErr.Load())
			t.Logf("%s: %s", kind, rig.waitStats())
			for _, e := range errsAfter {
				t.Logf("%s: not judged, after its client's close: %s", kind, e.line)
			}
			if dialFail.Load()+hsFail.Load() > 0 {
				t.Fatalf("%d conns failed to dial/handshake -- environment problem, not a verdict", dialFail.Load()+hsFail.Load())
			}
			if n := ecanceled.Load(); n != 0 {
				t.Errorf("%d WebSocket handler(s) observed ECANCELED from WriteMessage: the recv-pause "+
					"cancel killed an in-flight SEND (celeris#482)", n)
			}
			// protoErr and otherWriteErr were logged but never asserted, so a
			// server that killed a healthy connection mid-write passed on
			// these counters alone (celeris#530). Judged only when the error
			// came before its own client closed (see above).
			if len(errsBefore) != 0 {
				for _, e := range errsBefore {
					t.Logf("%s: %s", kind, e.line)
				}
				t.Errorf("%d handler error(s) (%d read, %d write) before their own client closed: the engine "+
					"mis-delivered, or tore down or failed a connection it should have kept alive",
					len(errsBefore), readBefore, writeBefore)
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
			// celeris#623: counted and printed for months and never judged,
			// while it was the larger of the two stall populations. The
			// client now waits on progress and reads while it waits, so a
			// give-up here is a connection on which nothing moved for
			// wsoWriteIdle (or that ran past wsoWaitCap), not a slow server.
			if n := clientCloseFail.Load(); n != 0 {
				t.Errorf("%d conn(s) never finished writing their last frame or their Close frame: nothing "+
					"moved for %v (or the wait ran past %v). Each WSO-GIVEUP record above has the "+
					"connection's timeline from both ends and the shape it stalled in (celeris#623)",
					n, wsoWriteIdle, wsoWaitCap)
			}
			if n := closeTimeout.Load(); n != 0 {
				// Deliberately does NOT name a cause in the message: a
				// connection whose recv stayed paused (celeris#482) and one
				// whose send accounting desynchronised so the dirty-list flush
				// skipped it forever (celeris#519) look the same here, and
				// asserting the former sent three separate investigations down
				// the wrong path. The WSO-GIVEUP record carries the state.
				t.Errorf("%d conn(s) never completed the Close handshake: no byte and no close from the "+
					"server for %v after the client's Close (or the wait ran past %v). Each WSO-GIVEUP "+
					"record above has the connection's timeline and the state of both ends",
					n, wsoCloseIdle, wsoWaitCap)
			}
		})
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

// wsHandshakePath is wsHandshake for a request target other than "/ws" (the
// backpressure oracles put their join key in the query).
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

// maskedCloseFrame is a client->server Close (opcode 8) with status 1000.
func maskedCloseFrame() []byte {
	m := [4]byte{0x11, 0x22, 0x33, 0x44}
	return []byte{0x88, 0x82, m[0], m[1], m[2], m[3], 0x03 ^ m[0], 0xE8 ^ m[1]}
}
