//go:build linux

package websocket

// Regression test for the inline-egress fast path (engine/{epoll,iouring}
// detached guarded writeFn) and its ring/worker fallback. Broadcasts large
// (>= sendZCMinBytes = 4096B, so io_uring uses SEND_ZC) frames to many detached
// conns from a publisher while the dispatch goroutines issue inline writes,
// and asserts every RECEIVED frame is byte-intact and per-conn in order — a
// wire interleave between a raw unix.Write and a ring SEND (or a reorder vs the
// loop-thread flush) would corrupt the payload or the sequence. Run under
// -race in CI this covers the SEND_ZC-completion-vs-inline-write interaction
// that the 64-byte ws-hub-broadcast benchmark never reaches (below the 4096B
// ZC threshold). Frames may be DROPPED under backpressure (HubPolicyDrop) —
// that is allowed; corruption and reorder are not.

import (
	"bufio"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// readServerFrameNonFatal reads one unmasked server frame without calling
// t.Fatal (safe to run in a background goroutine at teardown).
func readServerFrameNonFatal(br *bufio.Reader) (fin bool, op Opcode, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	op = Opcode(h[0] & 0x0F)
	length := int64(h[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(br, ext[:]); err != nil {
			return
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(br, ext[:]); err != nil {
			return
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	payload = make([]byte, length)
	_, err = io.ReadFull(br, payload)
	return
}

func TestNativeEngineHubBroadcastInlineEgress(t *testing.T) {
	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			const (
				nconns    = 32
				nframes   = 400
				frameSize = 8192 // >= sendZCMinBytes(4096): io_uring exercises SEND_ZC
			)

			hub := NewHub(HubConfig{
				OnSlowConn: func(*Conn, error) HubPolicy { return HubPolicyDrop },
			})
			cfg := Config{Handler: func(c *Conn) {
				unreg := hub.Register(c)
				defer unreg()
				for {
					if _, _, err := c.ReadMessage(); err != nil {
						return
					}
				}
			}}
			addr, stop := startNativeServer(t, kind, cfg)
			defer stop()

			var wg sync.WaitGroup
			var corrupt, reorder int64
			clients := make([]*testWSClient, nconns)
			for i := 0; i < nconns; i++ {
				c := dialRaw(t, addr)
				c.upgrade(t, "/ws")
				clients[i] = c
				wg.Add(1)
				go func(c *testWSClient) {
					defer wg.Done()
					var lastSeq int64 = -1
					for {
						_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
						fin, op, payload, err := readServerFrameNonFatal(c.br)
						if err != nil {
							return
						}
						// A well-formed broadcast frame: final binary frame of the
						// exact size, seq in the first 8 bytes, fill = byte(seq)+byte(j).
						if !fin || op != OpBinary || len(payload) != frameSize {
							atomic.AddInt64(&corrupt, 1)
							return
						}
						seq := int64(binary.BigEndian.Uint64(payload[:8]))
						for j := 8; j < len(payload); j++ {
							if payload[j] != byte(seq)+byte(j) {
								atomic.AddInt64(&corrupt, 1)
								return
							}
						}
						if seq <= lastSeq { // per-conn FIFO: seq must strictly increase
							atomic.AddInt64(&reorder, 1)
						}
						lastSeq = seq
					}
				}(c)
			}

			for hub.Len() < nconns {
				time.Sleep(5 * time.Millisecond)
			}

			for seq := 0; seq < nframes; seq++ {
				payload := make([]byte, frameSize)
				binary.BigEndian.PutUint64(payload[:8], uint64(seq))
				for j := 8; j < frameSize; j++ {
					payload[j] = byte(seq) + byte(j)
				}
				pm, err := NewPreparedMessage(OpBinary, payload)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := hub.BroadcastPrepared(pm); err != nil {
					t.Fatal(err)
				}
			}

			time.Sleep(300 * time.Millisecond) // let the tail drain
			for _, c := range clients {
				c.close()
			}
			wg.Wait()

			if n := atomic.LoadInt64(&corrupt); n != 0 {
				t.Errorf("%s: %d corrupted frames — inline egress interleaved with a ring/loop send", kind, n)
			}
			if n := atomic.LoadInt64(&reorder); n != 0 {
				t.Errorf("%s: %d out-of-order frames — per-conn FIFO violated", kind, n)
			}
		})
	}
}
