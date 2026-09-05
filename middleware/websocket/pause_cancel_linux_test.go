//go:build linux

package websocket

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
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

	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			var ecanceled, otherWriteErr, protoErr atomic.Int64
			addr, shutdownEngine := startNativeServer(t, kind, Config{
				CheckOrigin:           func(*celeris.Context) bool { return true },
				ReadLimit:             256 * 1024,
				MaxBackpressureBuffer: bpBuf, // realistic buffer; headroom (cap-highWater) must exceed async pause-apply latency, else Append drops a chunk (ErrReadLimit) -- a config artifact, not engine reordering
				Handler: func(c *Conn) {
					for {
						mt, msg, err := c.ReadMessage()
						if err != nil {
							if !isCloseErr(err) {
								protoErr.Add(1)
							}
							return
						}
						if err := c.WriteMessage(mt, msg); err != nil {
							if errors.Is(err, syscall.ECANCELED) {
								ecanceled.Add(1)
							} else if !errors.Is(err, ErrWriteClosed) {
								otherWriteErr.Add(1)
							}
							return
						}
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
					defer func() { _ = c.Close() }()
					if err := wsHandshake(c, hostPort); err != nil {
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
					// Complete the current frame so the wire is a whole number of frames. writeAll
					// retries to completion: once the flood stops the server drains and the send
					// buffer empties. A conn the server wrongly killed surfaces as an error here.
					if rem := wrote % 126; rem != 0 {
						need := 126 - rem
						start := wrote % len(batch)
						if !writeAll(c, batch[start:start+need], 15*time.Second) {
							clientCloseFail.Add(1)
							return
						}
						wrote += need
					}
					// Drain the echo backlog (a real WS client reads); relieves backpressure.
					buf := make([]byte, 64<<10)
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
					if !writeAll(c, maskedCloseFrame(), 10*time.Second) {
						clientCloseFail.Add(1)
						return
					}
					_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
					tClose := time.Now()
					for {
						_, err := c.Read(buf)
						if err == nil {
							continue
						}
						if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
							closedOK.Add(1)
						} else {
							closeTimeout.Add(1)
							t.Logf("close-timeout %s: %v after Close sent", c.LocalAddr(), time.Since(tClose).Round(time.Millisecond))
						}
						return
					}
				}()
			}
			wg.Wait()

			t.Logf("%s: clientMisaligned=%d framesSent=%d (if misaligned>0 the client truncated; if 0 while protocol errors>0 the engine mis-delivered)",
				kind, clientMisaligned.Load(), framesSent.Load())
			if clientMisaligned.Load() > 0 {
				t.Errorf("%d client conn(s) ended mid-frame -- test client bug, not a server verdict", clientMisaligned.Load())
			}

			t.Logf("%s: conns=%d protoErr=%d clientCloseFail=%d ecanceled=%d otherWriteErr=%d closedOK=%d closeTimeout=%d dialFail=%d hsFail=%d",
				kind, conns, protoErr.Load(), clientCloseFail.Load(), ecanceled.Load(), otherWriteErr.Load(), closedOK.Load(), closeTimeout.Load(), dialFail.Load(), hsFail.Load())
			if dialFail.Load()+hsFail.Load() > 0 {
				t.Fatalf("%d conns failed to dial/handshake -- environment problem, not a verdict", dialFail.Load()+hsFail.Load())
			}
			if n := ecanceled.Load(); n != 0 {
				t.Errorf("%d WebSocket handler(s) observed ECANCELED from WriteMessage: the recv-pause "+
					"cancel killed an in-flight SEND (celeris#482)", n)
			}
			if n := closeTimeout.Load(); n != 0 {
				t.Errorf("%d conn(s) never completed the Close handshake: recv paused and never re-armed "+
					"(the leaked-ESTAB symptom of celeris#482)", n)
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
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	req := "GET /ws HTTP/1.1\r\nHost: " + hostPort + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
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
	end := time.Now().Add(within)
	for len(buf) > 0 {
		_ = c.SetWriteDeadline(end)
		n, err := c.Write(buf)
		buf = buf[n:]
		if err != nil {
			return len(buf) == 0
		}
	}
	return true
}

// maskedCloseFrame is a client->server Close (opcode 8) with status 1000.
func maskedCloseFrame() []byte {
	m := [4]byte{0x11, 0x22, 0x33, 0x44}
	return []byte{0x88, 0x82, m[0], m[1], m[2], m[3], 0x03 ^ m[0], 0xE8 ^ m[1]}
}
