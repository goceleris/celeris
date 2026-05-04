package sse

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

// gatedStreamer is a mockStreamer variant whose Write+Flush can be paused
// and released by the test, deterministically controlling drain progress.
type gatedStreamer struct {
	mu         sync.Mutex
	chunks     [][]byte
	flushed    int
	closed     bool
	writeReady chan struct{} // closed once: blocks Write until released
	released   atomic.Bool
}

func newGatedStreamer() *gatedStreamer {
	return &gatedStreamer{writeReady: make(chan struct{})}
}

func (g *gatedStreamer) WriteResponse(_ *stream.Stream, _ int, _ [][2]string, _ []byte) error {
	return nil
}

func (g *gatedStreamer) WriteHeader(_ *stream.Stream, _ int, _ [][2]string) error {
	return nil
}

func (g *gatedStreamer) Write(_ *stream.Stream, data []byte) error {
	<-g.writeReady
	g.mu.Lock()
	g.chunks = append(g.chunks, append([]byte(nil), data...))
	g.mu.Unlock()
	return nil
}

// Flush is not gated. The SSE middleware flushes once at handler start
// to push response headers immediately (real-world behavior — without
// this an EventSource client never sees the response). Gating Flush
// would deadlock that initial flush; gating only Write keeps the test's
// drain-progress control while letting the headers through.
func (g *gatedStreamer) Flush(_ *stream.Stream) error {
	g.mu.Lock()
	g.flushed++
	g.mu.Unlock()
	return nil
}

func (g *gatedStreamer) Close(_ *stream.Stream) error {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.release()
	return nil
}

func (g *gatedStreamer) release() {
	if g.released.CompareAndSwap(false, true) {
		close(g.writeReady)
	}
}

func newGatedContext(t *testing.T) (*celeris.Context, *gatedStreamer) {
	t.Helper()
	ctx, _ := celeristest.NewContextT(t, "GET", "/events")
	g := newGatedStreamer()
	s := celeris.TestStream(ctx)
	s.ResponseWriter = g
	return ctx, g
}

// TestPooledClientStateReset — running two sequential handlers through
// the sync.Pool must not leak queued-mode state from the first run
// into the second. Specifically: queue, drainDone, queueClosed,
// onSlowClient, droppedEvents, replayStore must all be zeroed by
// releaseClient between handler invocations.
func TestPooledClientStateReset(t *testing.T) {
	captured := make(chan *Client, 2)

	// First handler: opt into the queued + replay paths so every new
	// state field is non-zero by the time releaseClient fires.
	cfg1 := Config{
		HeartbeatInterval: -1,
		MaxQueueDepth:     8,
		ReplayStore:       NewRingBuffer(4),
		OnSlowClient:      func(*Client, Event) ClientPolicy { return ClientPolicyDrop },
		Handler: func(c *Client) {
			_ = c.Send(Event{Data: "first"})
			captured <- c
		},
	}
	ctx1, _ := newSSEContext(t)
	if err := New(cfg1)(ctx1); err != nil {
		t.Fatal(err)
	}

	// Second handler: legacy blocking mode, no replay. The Client
	// pulled from the pool must look like a fresh acquisition — every
	// new state field zero.
	cfg2 := Config{
		HeartbeatInterval: -1,
		Handler: func(c *Client) {
			captured <- c
			if c.queue != nil {
				t.Errorf("pooled client carried queue from prior run: %v", c.queue)
			}
			if c.drainDone != nil {
				t.Errorf("pooled client carried drainDone from prior run")
			}
			if c.queueClosed.Load() {
				t.Errorf("pooled client carried queueClosed=true from prior run")
			}
			if c.onSlowClient != nil {
				t.Errorf("pooled client carried onSlowClient from prior run")
			}
			if got := c.DroppedEvents(); got != 0 {
				t.Errorf("pooled client carried DroppedEvents=%d from prior run", got)
			}
			if c.replayStore != nil {
				t.Errorf("pooled client carried replayStore from prior run")
			}
		},
	}
	ctx2, _ := newSSEContext(t)
	if err := New(cfg2)(ctx2); err != nil {
		t.Fatal(err)
	}

	first := <-captured
	second := <-captured
	// Sanity: under sync.Pool the two handlers are very likely to share
	// a Client instance (single-goroutine sequential run). If the
	// runtime decides not to recycle, the test still verifies the
	// fresh-state property — just less directly.
	if first == second {
		t.Logf("Client recycled across handlers — full reset path exercised")
	}
}

// TestSlowClientLegacyBlocking pins the no-opt-in path: with MaxQueueDepth=0
// the queue/drain machinery is dormant and Send writes synchronously.
func TestSlowClientLegacyBlocking(t *testing.T) {
	ctx, _ := newSSEContext(t)
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler: func(client *Client) {
			if client.queue != nil {
				t.Errorf("queue must be nil when MaxQueueDepth=0; got %v", client.queue)
			}
			if client.DroppedEvents() != 0 {
				t.Errorf("DroppedEvents = %d, want 0", client.DroppedEvents())
			}
			if client.QueueDepth() != 0 {
				t.Errorf("QueueDepth = %d, want 0", client.QueueDepth())
			}
			_ = client.Send(Event{Data: "x"})
		},
	})
	if err := handler(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestSlowClientDropEvent fills the queue while the streamer is gated, then
// asserts the configured drop policy increments DroppedEvents and leaves
// QueueDepth saturated at the cap. The drain goroutine may have one event
// in-flight (between channel-receive and gated Write), so the drop count is
// either total-queueSize or total-queueSize-1 depending on scheduling.
func TestSlowClientDropEvent(t *testing.T) {
	ctx, gs := newGatedContext(t)
	const queueSize = 4
	const total = 100

	resultDropped := make(chan uint64, 1)
	resultDepth := make(chan int, 1)

	handler := New(Config{
		HeartbeatInterval: -1,
		MaxQueueDepth:     queueSize,
		Handler: func(client *Client) {
			for range total {
				_ = client.Send(Event{Data: "x"})
			}
			resultDepth <- client.QueueDepth()
			resultDropped <- client.DroppedEvents()
			gs.release()
		},
	})
	if err := handler(ctx); err != nil {
		t.Fatal(err)
	}
	dropped := <-resultDropped
	depth := <-resultDepth

	if depth != queueSize {
		t.Errorf("QueueDepth = %d, want %d (queue saturated)", depth, queueSize)
	}
	minDrop, maxDrop := uint64(total-queueSize-1), uint64(total-queueSize)
	if dropped < minDrop || dropped > maxDrop {
		t.Errorf("DroppedEvents = %d, want %d..%d", dropped, minDrop, maxDrop)
	}
}

// TestSlowClientDisconnect verifies the disconnect policy cancels the context
// and surfaces an error once the queue overflows. The handler must release
// the streamer gate before returning so the in-flight drain Write can
// complete; otherwise the defer's drain join would deadlock. Send is looped
// because drain scheduling determines whether the queue fills on Send #2
// (drain hasn't pulled yet) or Send #3 (drain already holds one event).
func TestSlowClientDisconnect(t *testing.T) {
	ctx, gs := newGatedContext(t)

	var (
		policyErr error
		ctxErr    error
	)
	handler := New(Config{
		HeartbeatInterval: -1,
		MaxQueueDepth:     1,
		OnSlowClient: func(_ *Client, _ Event) ClientPolicy {
			return ClientPolicyDisconnect
		},
		Handler: func(client *Client) {
			for range 8 {
				if err := client.Send(Event{Data: "x"}); err != nil {
					policyErr = err
					break
				}
			}
			ctxErr = client.Context().Err()
			gs.release()
		},
	})
	if err := handler(ctx); err != nil {
		t.Fatal(err)
	}
	if policyErr == nil {
		t.Errorf("disconnect policy did not surface an error after queue overflow")
	}
	if ctxErr == nil {
		t.Errorf("Context not cancelled after disconnect policy fired")
	}
}

// TestSlowClientBlockPolicy verifies ClientPolicyBlock falls through to a blocking
// queue send: total Send time stretches to at least the gate-release delay
// because at least one Send must wait on drain to free a slot.
func TestSlowClientBlockPolicy(t *testing.T) {
	ctx, gs := newGatedContext(t)
	const queueSize = 2
	const total = 8
	const gateDelay = 30 * time.Millisecond

	go func() {
		time.Sleep(gateDelay)
		gs.release()
	}()

	var blockedFor time.Duration
	handler := New(Config{
		HeartbeatInterval: -1,
		MaxQueueDepth:     queueSize,
		OnSlowClient: func(_ *Client, _ Event) ClientPolicy {
			return ClientPolicyBlock
		},
		Handler: func(client *Client) {
			start := time.Now()
			for range total {
				if err := client.Send(Event{Data: "x"}); err != nil {
					t.Errorf("Send under ClientPolicyBlock returned %v", err)
				}
			}
			blockedFor = time.Since(start)
		},
	})
	if err := handler(ctx); err != nil {
		t.Fatal(err)
	}
	floor := gateDelay - 5*time.Millisecond
	if blockedFor < floor {
		t.Errorf("ClientPolicyBlock total Send time %v < %v — block path did not engage", blockedFor, floor)
	}
}

// TestSlowClientRace exercises 100 concurrent publishers writing to a single
// client to flush out queue/drain races. Run with -race for full coverage.
func TestSlowClientRace(t *testing.T) {
	ctx, _ := newSSEContext(t)
	const publishers = 100
	const perPublisher = 100
	var (
		sent atomic.Uint64
		errs atomic.Uint64
	)
	handler := New(Config{
		HeartbeatInterval: -1,
		MaxQueueDepth:     32,
		Handler: func(client *Client) {
			var wg sync.WaitGroup
			wg.Add(publishers)
			for range publishers {
				go func() {
					defer wg.Done()
					for range perPublisher {
						if err := client.Send(Event{Data: "x"}); err != nil {
							errs.Add(1)
							return
						}
						sent.Add(1)
					}
				}()
			}
			wg.Wait()
		},
	})
	if err := handler(ctx); err != nil {
		t.Fatal(err)
	}
	if errs.Load() > 0 {
		t.Errorf("got %d errors during concurrent Send", errs.Load())
	}
	if sent.Load() == 0 {
		t.Errorf("no Send succeeded — race or deadlock")
	}
}

// TestSlowClientSendNonFullQueueAllocs is a strict-alloc gate on the
// happy-path enqueue. The queued Send must avoid heap growth in steady state
// — a regression here would mean per-event garbage on every fan-out.
func TestSlowClientSendNonFullQueueAllocs(t *testing.T) {
	if raceEnabled || testing.CoverMode() != "" || testing.Short() {
		t.Skip("alloc counts unstable under -race / coverage / -short")
	}
	ctx, gs := newGatedContext(t)

	ready := make(chan *Client, 1)
	released := make(chan struct{})
	handlerDone := make(chan struct{})
	handler := New(Config{
		HeartbeatInterval: -1,
		MaxQueueDepth:     1024,
		Handler: func(client *Client) {
			ready <- client
			<-released
		},
	})
	go func() {
		defer close(handlerDone)
		_ = handler(ctx)
	}()
	client := <-ready

	allocs := testing.AllocsPerRun(200, func() {
		// Drain one slot first so the next Send is non-blocking.
		select {
		case <-client.queue:
		default:
		}
		_ = client.Send(Event{Data: "x"})
	})
	close(released)
	gs.release() // unblock the drain goroutine so handler defer can complete
	<-handlerDone

	const budget = 1.0
	if allocs > budget {
		t.Fatalf("Send queued path: %.1f allocs/op, budget %.1f", allocs, budget)
	}
}
