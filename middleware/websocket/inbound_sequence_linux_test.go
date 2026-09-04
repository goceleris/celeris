//go:build linux

package websocket

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
)

// TestBackpressureInboundSequenceIntegrity (celeris#484 oracle): every inbound
// frame carries a strictly increasing 64-bit sequence number and connection index,
// so a lost, corrupted, or reordered frame is detected immediately. The client
// floods without reading (forcing repeated pause/resume) and the server handler
// validates sequence continuity per connection, payload content integrity, and
// verifies the tail sequence number transmitted in the Close frame.
func TestBackpressureInboundSequenceIntegrity(t *testing.T) {
	conns := envInt("WS484_CONNS", 96)
	// bpBuf is the chanReader backpressure buffer capacity (default 256, matching the
	// original #484 reproduction fixture; verified at both 256 and 512).
	bpBuf := envInt("WS484_BP", 256)
	bursts := envInt("WS484_BURSTS", 4)
	perBurst := envInt("WS484_BURST_FRAMES", 16000) // frames per burst per conn
	if testing.Short() {
		conns = envInt("WS484_CONNS", 16)
		bursts = envInt("WS484_BURSTS", 2)
		perBurst = envInt("WS484_BURST_FRAMES", 1000)
	}
	const plen = 120

	expectedPad := bytes.Repeat([]byte{'x'}, plen-16)

	for _, kind := range engineKinds(t) {
		kind := kind
		variants := []struct {
			name     string
			mshotEnv string
		}{
			{"defaults", ""},
		}
		if kind == celeris.IOUring {
			variants = append(variants, struct {
				name     string
				mshotEnv string
			}{"multishot_recv", "1"})
		}
		for _, v := range variants {
			v := v
			testName := kind.String()
			if v.name != "defaults" {
				testName += "/" + v.name
			}
			t.Run(testName, func(t *testing.T) {
				if v.mshotEnv != "" {
					t.Setenv("CELERIS_IOURING_MULTISHOT_RECV", v.mshotEnv)
					p := probe.Probe()
					t.Logf("%s: multishot_recv sub-run: kernel=%s tier=%s providedBuffers=%t multishotRecv=%t",
						testName, p.KernelVersion, p.IOUringTier.String(), p.ProvidedBuffers, p.MultishotRecv)
					if !p.ProvidedBuffers || p.IOUringTier < celerisengine.High {
						t.Skip("skipping multishot_recv sub-run: kernel does not support provided buffers (need High tier + ProvidedBuffers)")
					}
				}

				var gaps, parseErr, overflowErr, protoErr, framesIn, framesSent atomic.Int64
				var closedOK, closeTimeout, dialFail, hsFail, clientCloseFail atomic.Int64

				connLastSeq := make([]int64, conns)
				for i := range conns {
					connLastSeq[i] = -1
				}
				connFramesIn := make([]atomic.Int64, conns)
				connProtoErr := make([]atomic.Int64, conns)
				connSent := make([]atomic.Int64, conns)
				clientFailed := make([]atomic.Bool, conns)

				addr, shutdown := startNativeServer(t, kind, Config{
					CheckOrigin:           func(*celeris.Context) bool { return true },
					ReadLimit:             256 * 1024,
					MaxBackpressureBuffer: bpBuf,
					Handler: func(c *Conn) {
						var myConnIdx = -1
						for {
							mt, msg, err := c.ReadMessage()
							if err != nil {
								if ce, ok := err.(*CloseError); ok {
									if len(ce.Text) == 8 && myConnIdx >= 0 {
										finalCount := int64(binary.BigEndian.Uint64([]byte(ce.Text)))
										expectedCount := connLastSeq[myConnIdx] + 1
										if expectedCount != finalCount {
											t.Errorf("conn %d: tail loss: last seq=%d, client sent=%d",
												myConnIdx, connLastSeq[myConnIdx], finalCount)
											gaps.Add(1)
										}
									}
								} else if errors.Is(err, ErrReadLimit) {
									// Channel capacity overflow at chanReader.Append when async pause latency
									// outpaces headroom. Tracked separately from wire frame corruption.
									overflowErr.Add(1)
								} else if !isCloseErr(err) {
									protoErr.Add(1)
									if myConnIdx >= 0 {
										connProtoErr[myConnIdx].Add(1)
									}
									if isParseErr(err) {
										parseErr.Add(1)
										t.Errorf("parse error on conn %d: %v", myConnIdx, err)
									}
								}
								return
							}

							if len(msg) != plen {
								parseErr.Add(1)
								t.Errorf("invalid frame length: got %d, want %d", len(msg), plen)
								return
							}

							cIdx := int(binary.BigEndian.Uint64(msg[8:16]))
							if cIdx < 0 || cIdx >= conns {
								parseErr.Add(1)
								t.Errorf("invalid conn index in payload: %d", cIdx)
								return
							}
							if myConnIdx < 0 {
								myConnIdx = cIdx
							} else if myConnIdx != cIdx {
								parseErr.Add(1)
								t.Errorf("conn index mismatch: frame claimed %d, connection was %d", cIdx, myConnIdx)
								return
							}

							if !bytes.Equal(msg[16:], expectedPad) {
								parseErr.Add(1)
								t.Errorf("conn %d: payload padding corrupted", cIdx)
								return
							}

							seq := int64(binary.BigEndian.Uint64(msg[:8]))
							expectedSeq := connLastSeq[cIdx] + 1
							if seq != expectedSeq {
								gaps.Add(1)
								t.Errorf("conn %d: seq gap: got %d, want %d", cIdx, seq, expectedSeq)
							}
							connLastSeq[cIdx] = seq
							connFramesIn[cIdx].Add(1)
							framesIn.Add(1)

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
				for i := range conns {
					connID := i
					wg.Add(1)
					go func() {
						defer wg.Done()
						c, err := dialer.Dial("tcp", hostPort)
						if err != nil {
							dialFail.Add(1)
							clientFailed[connID].Store(true)
							return
						}
						defer func() { _ = c.Close() }()
						if err := wsHandshake(c, hostPort); err != nil {
							hsFail.Add(1)
							clientFailed[connID].Store(true)
							return
						}

						m := [4]byte{0x11, 0x22, 0x33, 0x44}
						encode := func(seq uint64) []byte {
							f := make([]byte, 6+plen)
							f[0], f[1] = 0x82, 0x80|byte(plen) // BINARY: payload carries 64-bit seq + connID
							copy(f[2:6], m[:])
							var payload [plen]byte
							binary.BigEndian.PutUint64(payload[:8], seq)
							binary.BigEndian.PutUint64(payload[8:16], uint64(connID))
							for j := 16; j < plen; j++ {
								payload[j] = 'x'
							}
							for j := 0; j < plen; j++ {
								f[6+j] = payload[j] ^ m[j%4]
							}
							return f
						}

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
									clientFailed[connID].Store(true)
									clientCloseFail.Add(1)
									break
								}
							}
							time.Sleep(200 * time.Millisecond)
						}
						if off > 0 {
							if !writeAll(c, cur[off:], 15*time.Second) {
								clientFailed[connID].Store(true)
								clientCloseFail.Add(1)
								return
							}
							seq++
						}
						connSent[connID].Store(int64(seq))
						framesSent.Add(int64(seq))

						buf := make([]byte, 64<<10)
						for {
							_ = c.SetReadDeadline(time.Now().Add(1 * time.Second))
							if _, err := c.Read(buf); err != nil {
								break
							}
						}

						if !writeAll(c, maskedCloseFrameWithCount(seq), 10*time.Second) {
							clientFailed[connID].Store(true)
							clientCloseFail.Add(1)
							return
						}

						_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
						for {
							if _, err := c.Read(buf); err != nil {
								if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
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

				t.Logf("%s: conns=%d framesSent=%d framesIn=%d seqGaps=%d parseErr=%d overflowErr=%d protocolErrors=%d clientCloseFail=%d closedOK=%d closeTimeout=%d dialFail=%d hsFail=%d",
					testName, conns, framesSent.Load(), framesIn.Load(), gaps.Load(), parseErr.Load(), overflowErr.Load(), protoErr.Load(), clientCloseFail.Load(), closedOK.Load(), closeTimeout.Load(), dialFail.Load(), hsFail.Load())

				if dialFail.Load()+hsFail.Load() > 0 {
					t.Fatalf("environment: %d dial/handshake failures", dialFail.Load()+hsFail.Load())
				}

				if parseErr.Load() != 0 {
					t.Errorf("%s: %d frame parse error(s) observed — frames were corrupted", testName, parseErr.Load())
				}
				if gaps.Load() != 0 {
					t.Errorf("%s: %d sequence gap(s) observed — frames were dropped or reordered", testName, gaps.Load())
				}
				if closeTimeout.Load() != 0 {
					t.Errorf("%s: %d connection(s) timed out waiting for Close handshake", testName, closeTimeout.Load())
				}
				if bpBuf >= 256 && overflowErr.Load() != 0 {
					t.Errorf("%s: %d channel overflow error(s) observed at buffer capacity %d", testName, overflowErr.Load(), bpBuf)
				}

				for i := range conns {
					if !clientFailed[i].Load() {
						in := connFramesIn[i].Load()
						sent := connSent[i].Load()
						if in != sent {
							t.Errorf("%s conn %d: frame count mismatch: in=%d, sent=%d", testName, i, in, sent)
						}
						if pe := connProtoErr[i].Load(); pe != 0 {
							t.Errorf("%s conn %d: protocol error on unfailed client: %d", testName, i, pe)
						}
					}
				}
			})
		}
	}
}

// maskedCloseFrameWithCount builds a client->server Close frame (opcode 8, masked)
// carrying a 10-byte payload: 2 bytes status 1000 + 8 bytes uint64 sequence count.
func maskedCloseFrameWithCount(finalCount uint64) []byte {
	m := [4]byte{0x11, 0x22, 0x33, 0x44}
	var payload [10]byte
	binary.BigEndian.PutUint16(payload[:2], 1000)
	binary.BigEndian.PutUint64(payload[2:], finalCount)

	f := make([]byte, 6+10)
	f[0] = 0x88      // FIN + opcode 8 (Close)
	f[1] = 0x80 | 10 // Masked + payload len 10
	copy(f[2:6], m[:])
	for j := 0; j < 10; j++ {
		f[6+j] = payload[j] ^ m[j%4]
	}
	return f
}

func isCloseErr(err error) bool {
	if err == nil {
		return true
	}
	if _, ok := err.(*CloseError); ok {
		return true
	}
	return errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, ErrClosed) ||
		errors.Is(err, ErrWriteClosed)
}

func isParseErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrProtocol) ||
		errors.Is(err, ErrReservedBits) ||
		errors.Is(err, ErrInvalidUTF8) ||
		errors.Is(err, ErrFrameTooLarge) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}
