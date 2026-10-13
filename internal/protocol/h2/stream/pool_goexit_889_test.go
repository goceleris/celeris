package stream

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// celeris#889 (item 32): a handler on the worker pool that calls
// runtime.Goexit ends the pool goroutine that ran it, because
// h2WorkerPool.run has no defer: the worker does not re-credit idle and does
// not come back. The pool is process-wide (GOMAXPROCS*4 workers), so repeated
// Goexits shrank it for every Server in the process; once no worker was
// parked, every stream ran on a goroutine of its own. The pool keeps its size.

type goexitPoolHandler889 struct{ ran atomic.Int32 }

func (h *goexitPoolHandler889) HandleStream(context.Context, *Stream) error {
	h.ran.Add(1)
	runtime.Goexit()
	return nil
}
func (h *goexitPoolHandler889) RouteAsync(_, _ string) bool { return true }
func (h *goexitPoolHandler889) HasAsyncRoutes() bool        { return true }

func TestH2Pool_GoexitDoesNotShrinkThePool889(t *testing.T) {
	const size = 4
	const goexits = 3 * size // more than the pool has workers
	p := newH2WorkerPool(size)
	waitIdle := func(want int32) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); p.idle.Load() != want && time.Now().Before(deadline); {
			time.Sleep(2 * time.Millisecond)
		}
		if got := p.idle.Load(); got != want {
			t.Fatalf("idle=%d, want %d parked workers", got, want)
		}
	}
	waitIdle(size)

	h := &goexitPoolHandler889{}
	for i := range goexits {
		proc := NewProcessor(h, newTestFrameWriter(), newTestResponseWriter())
		s := proc.manager.CreateStream(uint32(2*i + 1))
		if s == nil {
			t.Fatalf("stream %d: CreateStream returned nil", i)
		}
		proc.poolRunning.Add(1)
		p.Submit(proc, s)
		for deadline := time.Now().Add(5 * time.Second); proc.PoolHandlersRunning() && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		if proc.PoolHandlersRunning() {
			t.Fatalf("Goexit %d: the handler did not finish", i)
		}
		// Every Goexit must leave the pool as it found it: size parked workers
		// (not size-i), or the next streams run on goroutines of their own.
		waitIdle(size)
	}
	if got := h.ran.Load(); got != goexits {
		t.Fatalf("%d handlers ran, want %d", got, goexits)
	}
}
