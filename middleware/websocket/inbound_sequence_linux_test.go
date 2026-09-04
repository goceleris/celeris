//go:build linux

package websocket

import (
	"encoding/binary"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestBackpressureInboundSequenceIntegrity (celeris#484 oracle): every inbound
// frame carries a strictly increasing 64-bit sequence number, so a lost or
// reordered frame is detected as a gap even though every frame is the same
// size. The client floods without reading (forcing repeated pause/resume) and
// the server handler checks seq == last+1 per connection.
func TestBackpressureInboundSequenceIntegrity(t *testing.T) {
	conns := envInt("WS484_CONNS", 96)
	bpBuf := envInt("WS484_BP", 256)
	bursts := envInt("WS484_BURSTS", 4)
	perBurst := envInt("WS484_BURST_FRAMES", 16000) // frames per burst per conn
	const plen = 120
	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			var gaps, protoErr, framesIn, framesSent, closedOK, closeTimeout, dialFail, hsFail, clientCloseFail atomic.Int64
			addr, shutdown := startNativeServer(t, kind, Config{
				CheckOrigin: func(*celeris.Context) bool { return true }, ReadLimit: 256 * 1024, MaxBackpressureBuffer: bpBuf,
				Handler: func(c *Conn) {
					var last int64 = -1
					for {
						mt, msg, err := c.ReadMessage()
						if err != nil {
							if !isCloseErr(err) {
								protoErr.Add(1)
							}
							return
						}
						framesIn.Add(1)
						if len(msg) < 8 {
							protoErr.Add(1)
							return
						}
						seq := int64(binary.BigEndian.Uint64(msg[:8]))
						if last >= 0 && seq != last+1 {
							gaps.Add(1)
						}
						last = seq
						if err := c.WriteMessage(mt, msg); err != nil {
							return
						}
					}
				},
			})
			defer func() {
				done := make(chan struct{})
				go func() { shutdown(); close(done) }()
				select {
				case <-done:
				case <-time.After(20 * time.Second):
					t.Errorf("engine shutdown did not complete within 20s")
				}
			}()
			hostPort := strings.TrimSuffix(strings.TrimPrefix(addr, "ws://"), "/ws")
			dialer := net.Dialer{Timeout: 3 * time.Second, Control: func(_, _ string, rc syscall.RawConn) error {
				var serr error
				_ = rc.Control(func(fd uintptr) { serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 32<<10) })
				return serr
			}}
			var wg sync.WaitGroup
			for i := 0; i < conns; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					c, err := dialer.Dial("tcp", hostPort)
					if err != nil {
						if dialFail.Add(1) == 1 {
							t.Logf("first dial error: addr=%q hostPort=%q err=%v", addr, hostPort, err)
						}
						return
					}
					defer func() { _ = c.Close() }()
					if err := wsHandshake(c, hostPort); err != nil {
						hsFail.Add(1)
						return
					}
					m := [4]byte{0x11, 0x22, 0x33, 0x44}
					encode := func(seq uint64) []byte {
						f := make([]byte, 6+plen)
						f[0], f[1] = 0x82, 0x80|byte(plen) // BINARY: the 8-byte seq is not UTF-8
						copy(f[2:6], m[:])
						var payload [plen]byte
						binary.BigEndian.PutUint64(payload[:8], seq)
						for j := 8; j < plen; j++ {
							payload[j] = 'x'
						}
						for j := 0; j < plen; j++ {
							f[6+j] = payload[j] ^ m[j%4]
						}
						return f
					}
					// Byte-budgeted bursts with an exact per-frame offset: a write
					// deadline ends a burst mid-frame and the NEXT burst resumes the
					// same frame at the same byte, so the wire is never truncated by
					// the client. Only the final frame is completed with writeAll.
					var seq uint64
					cur, off := encode(0), 0
					for b := 0; b < bursts; b++ {
						budget := perBurst * (6 + plen)
						for budget > 0 {
							_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
							n, err := c.Write(cur[off:])
							off += n
							budget -= n
							if off == len(cur) {
								seq++
								cur, off = encode(seq), 0
							}
							if err != nil {
								break
							}
						}
						time.Sleep(200 * time.Millisecond)
					}
					if off > 0 {
						if !writeAll(c, cur[off:], 15*time.Second) {
							clientCloseFail.Add(1)
							return
						}
						seq++
					}
					framesSent.Add(int64(seq))
					buf := make([]byte, 64<<10)
					for {
						_ = c.SetReadDeadline(time.Now().Add(1 * time.Second))
						if _, err := c.Read(buf); err != nil {
							break
						}
					}
					if !writeAll(c, maskedCloseFrame(), 10*time.Second) {
						return
					}
					_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
					for {
						if _, err := c.Read(buf); err != nil {
							if err == syscall.ECONNRESET || err.Error() == "EOF" || isEOF(err) {
								closedOK.Add(1)
							} else {
								closeTimeout.Add(1)
							}
							return
						}
					}
				}()
			}
			wg.Wait()
			t.Logf("%s: conns=%d framesSent=%d framesIn=%d seqGaps=%d protocolErrors=%d clientCloseFail=%d closedOK=%d closeTimeout=%d dialFail=%d hsFail=%d",
				kind, conns, framesSent.Load(), framesIn.Load(), gaps.Load(), protoErr.Load(), clientCloseFail.Load(), closedOK.Load(), closeTimeout.Load(), dialFail.Load(), hsFail.Load())
			if dialFail.Load()+hsFail.Load() > 0 {
				t.Fatalf("environment: %d dial/handshake failures", dialFail.Load()+hsFail.Load())
			}
			// A conn whose CLIENT failed to complete its final frame may legitimately end
			// mid-frame; only errors beyond those are the engine's.
			if gaps.Load() != 0 || protoErr.Load() > clientCloseFail.Load() {
				t.Errorf("celeris#484: inbound stream not intact: seqGaps=%d protocolErrors=%d (clientCloseFail=%d)", gaps.Load(), protoErr.Load(), clientCloseFail.Load())
			}
		})
	}
}

func isCloseErr(err error) bool {
	_, ok := err.(*CloseError)
	return ok || err == ErrClosed || err == ErrWriteClosed
}

func isEOF(err error) bool { return err != nil && err.Error() == "EOF" }
