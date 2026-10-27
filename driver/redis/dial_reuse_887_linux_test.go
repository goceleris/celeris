//go:build linux

package redis

// celeris#887 item 28 (the checklist's twenty-eighth box): a driver dial fails
// with "celeris/iouring: fd already registered" when the new socket gets the
// number of a conn that was just unregistered on the same worker and whose
// onClose has not run. engine.WorkerLoop.UnregisterConn documents the refusal;
// dialRedisConn handed it to the caller, so Pool.Acquire could fail under
// connection churn on the io_uring engine.

import (
	"bufio"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDialAfterTheCloseOfAConnOnTheSameNumberSucceeds887 closes a conn and
// dials the next one at once, on each engine of the rig. The next socket takes
// the number of the conn just closed, and the io_uring engine keeps that
// number registered on its worker until the old conn is finalized. A dial that
// loses the race must wait for it (bounded) instead of returning the refusal.
func TestDialAfterTheCloseOfAConnOnTheSameNumberSucceeds887(t *testing.T) {
	eachRigEngine(t, func(t *testing.T, eng rigEngine) {
		prov := rigProvider(t, eng)
		fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {})
		// ForceRESP2 with no password and no DB: the handshake sends nothing,
		// so the fake needs no replies and the test measures the dial alone.
		cfg := Config{Addr: fake.Addr(), ForceRESP2: true}

		// Four goroutines close and dial without pause: the churn makes the
		// next socket take a number that was just released, on a worker that
		// has not finalized the conn that held it.
		const goroutines, perG = 4, 500
		var refused, other atomic.Int32
		var mu sync.Mutex
		var first error
		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perG; i++ {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					c, err := dialRedisConn(ctx, prov, cfg, -1)
					cancel()
					if err != nil {
						if strings.Contains(err.Error(), "already registered") {
							refused.Add(1)
						} else {
							other.Add(1)
						}
						mu.Lock()
						if first == nil {
							first = err
						}
						mu.Unlock()
						continue
					}
					_ = c.Close()
				}
			}()
		}
		wg.Wait()
		rounds := goroutines * perG
		t.Logf("engine %s: %d dials, %d refused with 'already registered', %d failed otherwise", eng, rounds, refused.Load(), other.Load())
		if first != nil {
			t.Errorf("a dial right after a close failed (%d of %d refused, %d other); first error: %v", refused.Load(), rounds, other.Load(), first)
		}
	})
}
