//go:build linux

package websocket

import (
	"bytes"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"testing"
)

// TestNativeEngineYieldIntegrity756 is the byte-integrity oracle for the loop's
// idle-wait yield (celeris#756): many connections echo large and small frames
// concurrently, with GC churn in the background to keep the scheduler busy,
// and every echoed payload is compared byte for byte (the load generator
// cannot see a corrupted echo, only an error).
func TestNativeEngineYieldIntegrity756(t *testing.T) {
	const (
		conns  = 16
		frames = 150
	)
	for _, kind := range engineKinds(t) {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			addr, shutdown := startNativeServer(t, kind, Config{
				Handler: func(c *Conn) {
					for {
						mt, msg, err := c.ReadMessage()
						if err != nil {
							return
						}
						if err := c.WriteMessage(mt, msg); err != nil {
							return
						}
					}
				},
			})
			defer shutdown()

			stop := make(chan struct{})
			var gcwg sync.WaitGroup
			gcwg.Add(1)
			go func() { // allocation churn: keeps GC cycles running
				defer gcwg.Done()
				var sink [][]byte
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					sink = append(sink, make([]byte, 32<<10))
					if len(sink) > 256 {
						sink = sink[:0]
						runtime.Gosched()
					}
				}
			}()
			defer func() { close(stop); gcwg.Wait() }()

			var wg sync.WaitGroup
			errs := make(chan error, conns)
			for ci := 0; ci < conns; ci++ {
				wg.Add(1)
				go func(ci int) {
					defer wg.Done()
					client := dialRaw(t, addr)
					defer client.close()
					client.upgrade(t, "/ws")
					rng := rand.New(rand.NewSource(int64(ci) + 1))
					for i := 0; i < frames; i++ {
						size := 64 << 10 // the ws-large-echo shape: 64 KiB frames
						if i%5 == 4 {
							size = 1 + rng.Intn(2048) // small frames mixed in
						}
						payload := make([]byte, size)
						rng.Read(payload)
						if err := client.writeClientFrame(true, OpBinary, payload); err != nil {
							errs <- fmt.Errorf("conn %d frame %d write: %w", ci, i, err)
							return
						}
						_, op, resp := client.readServerFrame(t)
						if op != OpBinary || !bytes.Equal(resp, payload) {
							errs <- fmt.Errorf("conn %d frame %d: op=%d len=%d want len=%d (bytes equal=%v)",
								ci, i, op, len(resp), len(payload), bytes.Equal(resp, payload))
							return
						}
					}
				}(ci)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
		})
	}
}
