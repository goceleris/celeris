package stream

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingPoolHandler blocks every stream handler until release is closed and
// reports how many handlers actually started.
type blockingPoolHandler struct {
	started atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (h *blockingPoolHandler) HandleStream(_ context.Context, _ *Stream) error {
	h.started.Add(1)
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-h.release
	return nil
}

func (h *blockingPoolHandler) RouteAsync(_, _ string) bool { return true }
func (h *blockingPoolHandler) HasAsyncRoutes() bool        { return true }

// TestH2Pool_StreamingHandlersDoNotStarveLaterStreams pins celeris#520.
//
// An H2 stream handler is not guaranteed to return: an SSE stream, a long
// poll or any streaming handler holds its worker until the peer goes away.
// The pool used to queue onto a buffered channel whenever the channel had
// room, so once `size` such handlers were running every later stream sat in
// the buffer behind workers that would never come back -- and because the
// pool is process-global, that starved every H2 connection in the binary,
// not just the one that filled it.
//
// The pool is deliberately tiny here: the failure needs more concurrent
// streaming handlers than workers, which on a 4-vCPU CI runner (16 workers)
// is 17 SSE clients and on a 32-core host is 129. Sizing the pool down is
// what makes the bug reproducible on any machine.
func TestH2Pool_StreamingHandlersDoNotStarveLaterStreams(t *testing.T) {
	const size = 2
	const streams = size * 4

	p := newH2WorkerPool(size)
	h := &blockingPoolHandler{entered: make(chan struct{}, streams), release: make(chan struct{})}
	defer close(h.release)

	var wg sync.WaitGroup
	for i := 0; i < streams; i++ {
		proc := NewProcessor(h, newTestFrameWriter(), newTestResponseWriter())
		s := proc.manager.CreateStream(uint32(2*i + 1))
		if s == nil {
			t.Fatalf("stream %d: CreateStream returned nil", i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Submit(proc, s)
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for h.started.Load() < int32(streams) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := h.started.Load(); got != int32(streams) {
		t.Fatalf("%d/%d streaming handlers started on a %d-worker pool: the rest are queued behind "+
			"workers that never return, and every later H2 stream in the process is stuck with them "+
			"(celeris#520)", got, streams, size)
	}
}

// TestH2Pool_IdleCreditTracksParkedWorkers pins the invariant Submit relies
// on: idle == (workers parked in the receive) - (tasks queued in work). With
// no work submitted every worker is parked, so idle must equal the pool size;
// it must never go negative.
func TestH2Pool_IdleCreditTracksParkedWorkers(t *testing.T) {
	const size = 4
	p := newH2WorkerPool(size)

	deadline := time.Now().Add(2 * time.Second)
	for p.idle.Load() != size && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := p.idle.Load(); got != size {
		t.Fatalf("idle=%d with an empty queue, want %d (every worker parked)", got, size)
	}

	// Run a burst of handlers that DO return: the credit must come back.
	h := &blockingPoolHandler{entered: make(chan struct{}, 64), release: make(chan struct{})}
	close(h.release) // handlers return immediately
	for i := 0; i < 64; i++ {
		proc := NewProcessor(h, newTestFrameWriter(), newTestResponseWriter())
		s := proc.manager.CreateStream(uint32(2*i + 1))
		if s == nil {
			t.Fatalf("stream %d: CreateStream returned nil", i)
		}
		p.Submit(proc, s)
		if got := p.idle.Load(); got < 0 {
			t.Fatalf("idle went negative (%d) after %d submits: a task was queued with no parked worker", got, i+1)
		}
	}
	deadline = time.Now().Add(5 * time.Second)
	for h.started.Load() < 64 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := h.started.Load(); got != 64 {
		t.Fatalf("%d/64 returning handlers ran", got)
	}
	for p.idle.Load() != size && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := p.idle.Load(); got != size {
		t.Fatalf("idle=%d after the burst drained, want %d: credits leak", got, size)
	}
}
