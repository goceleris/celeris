//go:build linux

package iouring

import (
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// The two closes of handleRecv's detection branch, and the provided buffer
// each one has to give back to the ring.
//
// With multishot recv the deciding recv of a conn arrives in a buffer of the
// worker's provided-buffer ring. handleRecv closes a conn whose header budget
// (detectDeadline, given at accept) is spent in two places: before
// initProtocol, when the deciding recv itself is past the budget (the check),
// and after it, when the budget ran out between the check and the arm of the
// header deadline, so that the arm closed the conn from inside initProtocol
// (the re-check). Each must return the buffer: one that is dropped is gone for
// good, a worker's ring holds as few as 1024 (bufRingCountMin), and a ring
// that has lost them all answers no recv.
//
// Both exits are decided by the clock at the instant they run, and a conn that
// reaches them with real lateness is closed by the idle sweep first unless
// its late bytes land inside the sweep's period (every 32 loop iterations: a
// few milliseconds under load, 800 ms on an idle worker). The end-to-end test
// of the root package (TestSpentBudgetReturnsItsRingBuffer) sees between 250
// and 1300 of its 3072 late conns reach an exit, depending on load, and on an
// unlucky run too few to empty a ring (reviewed on PR 1006). So this test
// puts the conns through the exit on demand: the engine's budget is a
// minute, nothing is late, and detectStageHook moves cs.detectDeadline into
// the past at the stage under test. Every conn then takes that exit, and the
// test counts, per worker, how many did: a worker that took fewer than its
// ring holds could not have shown a lost buffer, so the test then fails
// rather than pass without the exit having been exercised.

// exitCounts counts hook invocations per worker id.
type exitCounts struct {
	mu sync.Mutex
	n  map[int]int
}

func (c *exitCounts) add(id int) {
	c.mu.Lock()
	c.n[id]++
	c.mu.Unlock()
}

func (c *exitCounts) get(id int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[id]
}

func TestSpentDetectBudgetReturnsItsRingBuffer(t *testing.T) {
	const ringSize = 1024 // bufRingCountMin, pinned below through envPbufCount
	cases := []struct {
		name  string
		stage detectStage
		async bool
	}{
		{"check", detectBeforeBudgetCheck, false},
		{"recheck", detectAfterBudgetCheck, false},
		{"check-async", detectBeforeBudgetCheck, true},
		{"recheck-async", detectAfterBudgetCheck, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CELERIS_IOURING_MULTISHOT_RECV", "1")
			t.Setenv(envPbufCount, strconv.Itoa(ringSize))
			e, cancel, done := startRingRetried662(t, func() (*Engine, error) {
				return New(resource.Config{
					Addr:              "127.0.0.1:0",
					Protocol:          engine.Auto,
					Resources:         resource.Resources{Workers: 2}, // the engine refuses fewer
					AsyncHandlers:     tc.async,
					ReadHeaderTimeout: time.Minute,
					ReadTimeout:       time.Minute,
					IdleTimeout:       time.Minute,
					Logger:            slog.New(slog.DiscardHandler),
				}, idleRespHandler662{})
			})
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("engine did not stop within 10s")
				}
			})
			workers := workers662(e)
			if !workers[0].tier.SupportsMultishotRecv() {
				t.Skip("multishot recv with provided buffers is not available on this kernel")
			}
			addr := e.Addr().String()
			t.Logf("workers=%d ring=%d", len(workers), ringSize)

			var active atomic.Bool
			counts := &exitCounts{n: map[int]int{}}
			hook := func(w *Worker, cs *connState, st detectStage) {
				// Only a deciding recv that arrived in a provided buffer has one
				// to lose.
				if st == tc.stage && active.Load() && w.bufRing != nil {
					cs.detectDeadline = 1 // long past
					counts.add(w.id)
				}
			}
			detectStageHook.Store(&hook)
			t.Cleanup(func() { detectStageHook.Store(nil) })

			// Late conns, in batches, until every worker has taken more exits
			// than its ring holds (or the bound is hit and the test fails).
			active.Store(true)
			const batch, parallel, maxConns = 512, 128, 16 * ringSize
			var answered, closed atomic.Int64
			sent := 0
			enough := func() bool {
				for _, w := range workers {
					if counts.get(w.id) < ringSize+ringSize/8 {
						return false
					}
				}
				return true
			}
			total := func() (n int) {
				for _, w := range workers {
					n += counts.get(w.id)
				}
				return n
			}
			for sent < maxConns && !enough() {
				before := total()
				sem := make(chan struct{}, parallel)
				var wg sync.WaitGroup
				for i := 0; i < batch; i++ {
					sem <- struct{}{}
					wg.Add(1)
					go func() {
						defer func() { <-sem; wg.Done() }()
						c, err := net.DialTimeout("tcp", addr, 5*time.Second)
						if err != nil {
							return
						}
						defer func() { _ = c.Close() }()
						_, _ = io.WriteString(c, "GET /ping HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
						_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
						b, _ := io.ReadAll(c)
						if strings.HasPrefix(string(b), "HTTP/1.1 200") {
							answered.Add(1)
						} else {
							closed.Add(1)
						}
					}()
				}
				wg.Wait()
				sent += batch
				if total() == before {
					// No conn reached the exit: a worker whose ring is empty
					// receives nothing, and the probes below report it.
					break
				}
			}
			active.Store(false)
			for _, w := range workers {
				t.Logf("worker %d: %d conns through the %s exit", w.id, counts.get(w.id), tc.name)
			}
			t.Logf("%d late conns: %d answered, %d closed unanswered", sent, answered.Load(), closed.Load())
			if answered.Load() != 0 {
				t.Errorf("%d requests whose conn had a spent budget were answered", answered.Load())
			}
			for _, w := range workers {
				if got := counts.get(w.id); got < ringSize {
					t.Fatalf("worker %d took the exit %d times, fewer than its ring of %d buffers: "+
						"a lost buffer could not have shown, the test did not exercise the exit", w.id, got, ringSize)
				}
			}

			// A GET on a fresh conn per probe, none late: every worker must
			// still have buffers to receive it. 64 conns reach both workers
			// with all but 2^-63 probability.
			for i := 0; i < 64; i++ {
				c, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.WriteString(c, "GET /ping HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				b, _ := io.ReadAll(c)
				_ = c.Close()
				if !strings.HasPrefix(string(b), "HTTP/1.1 200") || !strings.HasSuffix(string(b), "ok") {
					t.Fatalf("probe %d, after %d conns closed on a spent budget at the %s exit, got %q, "+
						"want a 200 answer: a worker's ring ran out of buffers", i, sent, tc.name, b)
				}
			}
		})
	}
}
