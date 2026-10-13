package sse

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/testhooks"
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

	// writeReachedOnce is closed the first time Write is entered (before
	// it blocks on writeReady). Tests that need to synchronize on
	// "drain goroutine has pulled an event and is now blocked at the
	// gate" wait on this channel — fixes a flake in
	// TestBrokerSlowSubscriberConcurrencyCap where pub2 was enqueueing
	// before the drain goroutine had pulled pub1, so the queue-full
	// slow path fired on the wrong publish.
	writeReachedOnce chan struct{}
	writeReachedFlag atomic.Bool
}

func newGatedStreamer() *gatedStreamer {
	return &gatedStreamer{
		writeReady:       make(chan struct{}),
		writeReachedOnce: make(chan struct{}),
	}
}

func (g *gatedStreamer) WriteResponse(_ *stream.Stream, _ int, _ [][2]string, _ []byte) error {
	return nil
}

func (g *gatedStreamer) WriteHeader(_ *stream.Stream, _ int, _ [][2]string) error {
	return nil
}

func (g *gatedStreamer) Write(_ *stream.Stream, data []byte) error {
	if g.writeReachedFlag.CompareAndSwap(false, true) {
		close(g.writeReachedOnce)
	}
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
	s := testhooks.Stream(ctx)
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
// QueueDepth saturated at the cap. The first event is sent alone and the test
// waits until the drain goroutine has popped it and is parked at the gated
// Write; only then is the queue filled. Without that barrier the drain may
// pop an event at any point during or after the send loop, so the depth read
// could observe queueSize-1 (the popped event is no longer queued). With the
// barrier the drain holds exactly one event and the queue is exactly full.
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
			_ = client.Send(Event{Data: "x"})
			select {
			case <-gs.writeReachedOnce:
			case <-time.After(3 * time.Second):
				t.Errorf("drain goroutine did not reach the gated Write within 3s")
			}
			for range total - 1 {
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

// TestSlowClientClose verifies the close policy cancels the context
// and surfaces an error once the queue overflows. The handler must release
// the streamer gate before returning so the in-flight drain Write can
// complete; otherwise the defer's drain join would deadlock. Send is looped
// because drain scheduling determines whether the queue fills on Send #2
// (drain hasn't pulled yet) or Send #3 (drain already holds one event).
func TestSlowClientClose(t *testing.T) {
	ctx, gs := newGatedContext(t)

	var (
		policyErr error
		ctxErr    error
	)
	handler := New(Config{
		HeartbeatInterval: -1,
		MaxQueueDepth:     1,
		OnSlowClient: func(_ *Client, _ Event) ClientPolicy {
			return ClientPolicyClose
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
		t.Errorf("Context not cancelled after close policy fired")
	}
}

// TestSlowClientBlockPolicy verifies ClientPolicyBlock falls through to a
// blocking queue send, and drops nothing. The gate opens gateDelay after the
// first OnSlowClient call, which runs only when a Send finds the queue full.
// Until the gate opens the drain writes nothing, so it and the queue take at
// most 1+queueSize events, and the handler sends more than that: the queue
// does fill (OnSlowClient runs), and the last Send cannot return before the
// gate opens, so the Sends take at least gateDelay from that first call
// (celeris#888 item 14: a clock the handler started later than the gate's
// timer needed slack and still failed in CI, and a clock started before the
// handler could pass without a Send ever blocking).
func TestSlowClientBlockPolicy(t *testing.T) {
	ctx, gs := newGatedContext(t)
	const queueSize = 2
	const total = 8 // more than the drain's one event plus queueSize
	const gateDelay = 30 * time.Millisecond

	var once sync.Once
	var gateStart, finished time.Time
	var dropped uint64
	handler := New(Config{
		HeartbeatInterval: -1,
		MaxQueueDepth:     queueSize,
		OnSlowClient: func(_ *Client, _ Event) ClientPolicy {
			once.Do(func() {
				gateStart = time.Now()
				go func() {
					time.Sleep(gateDelay)
					gs.release()
				}()
			})
			return ClientPolicyBlock
		},
		Handler: func(client *Client) {
			defer gs.release() // if OnSlowClient never ran, the handler's defer would join a parked drain
			for range total {
				if err := client.Send(Event{Data: "x"}); err != nil {
					t.Errorf("Send under ClientPolicyBlock returned %v", err)
				}
			}
			finished = time.Now()
			dropped = client.DroppedEvents()
		},
	})
	if err := handler(ctx); err != nil {
		t.Fatal(err)
	}
	if gateStart.IsZero() {
		t.Fatalf("the queue never filled, so OnSlowClient never ran")
	}
	if dropped != 0 {
		t.Errorf("ClientPolicyBlock dropped %d events, want 0", dropped)
	}
	if d := finished.Sub(gateStart); d < gateDelay {
		t.Errorf("ClientPolicyBlock: the Sends finished %v after the first full-queue Send, before the %v gate opened — block path did not engage", d, gateDelay)
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
// — a regression here would mean per-event garbage on every fan-out. The one
// permitted allocation is the copy of the event's strings, which the queue
// keeps after Send returns (they can be another request's views,
// celeris#732); an event with no strings allocates nothing.
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

// dropFullQueue fills a queued client's queue while its drain is parked at
// the gated Write, so the next Send finds the queue full, and runs f on the
// handler's goroutine. It releases the gate before the handler returns, as
// the handler's defer joins the drain.
func dropFullQueue(t testing.TB, ctx *celeris.Context, gs *gatedStreamer, cfg Config, f func(client *Client)) {
	t.Helper()
	cfg.HeartbeatInterval = -1
	cfg.Handler = func(client *Client) {
		defer gs.release()
		if err := client.Send(Event{Data: "first"}); err != nil {
			t.Errorf("first Send: %v", err)
			return
		}
		select {
		case <-gs.writeReachedOnce:
		case <-time.After(3 * time.Second):
			t.Errorf("drain goroutine did not reach the gated Write within 3s")
			return
		}
		for range cap(client.queue) {
			if err := client.Send(Event{Data: "fill"}); err != nil {
				t.Errorf("filling Send: %v", err)
				return
			}
		}
		if got := client.QueueDepth(); got != cap(client.queue) {
			t.Errorf("QueueDepth = %d, want %d (queue full)", got, cap(client.queue))
			return
		}
		f(client)
	}
	if err := New(cfg)(ctx); err != nil {
		t.Fatal(err)
	}
}

// dropEvent888 is the event the drop-path tests send: three strings, so a
// copy of them would be an allocation (an event with no strings copies
// nothing).
var dropEvent888 = Event{ID: "42", Event: "update", Data: "payload-payload-payload"}

// TestSlowClientDropPathAllocs888 pins that a queued Send the default
// slow-client policy drops makes no allocation (celeris#888 item 21). The copy
// that keeps the event past Send (celeris#732) is for an event that is
// queued; with OnSlowClient nil a full queue drops the event, which nothing
// keeps, so the copy is skipped.
func TestSlowClientDropPathAllocs888(t *testing.T) {
	if raceEnabled || testing.CoverMode() != "" || testing.Short() {
		t.Skip("alloc counts unstable under -race / coverage / -short")
	}
	ctx, gs := newGatedContext(t)
	var allocs float64
	var dropped, calls uint64
	dropFullQueue(t, ctx, gs, Config{MaxQueueDepth: 4}, func(client *Client) {
		before := client.DroppedEvents()
		allocs = testing.AllocsPerRun(200, func() {
			calls++
			if err := client.Send(dropEvent888); err != nil {
				t.Errorf("Send to a full queue under the default policy: %v", err)
			}
		})
		dropped = client.DroppedEvents() - before
	})
	if allocs != 0 {
		t.Errorf("a dropped Send made %.1f allocations, want 0", allocs)
	}
	if calls == 0 || dropped != calls {
		t.Errorf("%d Sends to a full queue counted %d drops, want all of them", calls, dropped)
	}
}

// TestSlowClientFullQueueErrorsBeatDrop888 pins the order of checks in
// sendQueued: a Send to a closed or cancelled client returns its error even
// when the queue is full, as the Send contract says, and does not count a drop.
// The early drop of celeris#888 item 21 comes after both checks.
func TestSlowClientFullQueueErrorsBeatDrop888(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(client *Client)
		want error
	}{
		{"cancelled", func(client *Client) { client.cancel() }, context.Canceled},
		{"closed", func(client *Client) { client.queueClosed.Store(true) }, ErrClientClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, gs := newGatedContext(t)
			var err error
			var dropped uint64
			dropFullQueue(t, ctx, gs, Config{MaxQueueDepth: 2}, func(client *Client) {
				before := client.DroppedEvents()
				tc.make(client)
				err = client.Send(dropEvent888)
				dropped = client.DroppedEvents() - before
			})
			if !errors.Is(err, tc.want) {
				t.Errorf("Send to a %s client with a full queue returned %v, want %v", tc.name, err, tc.want)
			}
			if dropped != 0 {
				t.Errorf("Send to a %s client with a full queue counted %d drops, want 0", tc.name, dropped)
			}
		})
	}
}

// TestSlowClientCallbackKeepsItsEvent888 pins the other side of the drop-path
// shortcut: with OnSlowClient set the callback is handed a copy of the event,
// not the caller's strings, because the callback can keep it (celeris#888 item
// 21). The caller's strings are views of a buffer that changes after Send
// returns, as a request's do on epoll and io_uring (celeris#732).
func TestSlowClientCallbackKeepsItsEvent888(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy ClientPolicy
	}{
		{"drop", ClientPolicyDrop},
		{"close", ClientPolicyClose},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, gs := newGatedContext(t)
			var kept Event
			var calls int
			buf := []byte("42update-kindpayload-bytes")
			cfg := Config{
				MaxQueueDepth: 2,
				OnSlowClient: func(_ *Client, e Event) ClientPolicy {
					calls++
					kept = e
					return tc.policy
				},
			}
			dropFullQueue(t, ctx, gs, cfg, func(client *Client) {
				_ = client.Send(Event{ID: view(buf[:2]), Event: view(buf[2:13]), Data: view(buf[13:]), Retry: 9})
				for k := range buf {
					buf[k] = 'z'
				}
			})
			if calls != 1 {
				t.Fatalf("OnSlowClient ran %d times, want 1", calls)
			}
			if kept.ID != "42" || kept.Event != "update-kind" || kept.Data != "payload-bytes" || kept.Retry != 9 {
				t.Errorf("the event the callback kept reads ID %q Event %q Data %q Retry %d after the caller's bytes changed, want 42, update-kind, payload-bytes, 9",
					kept.ID, kept.Event, kept.Data, kept.Retry)
			}
		})
	}
}

// TestQueuedSendToBareRingKeepsACopy888 sends in queued mode to a bare
// NewRingBuffer store, so appendQueued hands the event to ringStore.appendOwned,
// which keeps it without a copy of its own (celeris#888 item 20). The drain is
// held on client.mu while the caller's bytes change, so the ring appends and the
// wire formats the event only after they have: both must still hold what Send
// was given. TestSentEventSurvivesSendersNextRequest covers the same on the
// native engines, but wraps the ring, which is not a *ringStore.
func TestQueuedSendToBareRingKeepsACopy888(t *testing.T) {
	ctx, ms := newSSEContext(t)
	ring := NewRingBuffer(4)
	buf := []byte("caller-idkind-amsg-a")
	var replay []Event
	var replayErr error
	handler := New(Config{
		HeartbeatInterval: -1,
		MaxQueueDepth:     4,
		ReplayStore:       ring,
		Handler: func(client *Client) {
			client.mu.Lock() // the drain parks on it, holding or about to hold the event
			err := client.Send(Event{ID: view(buf[:9]), Event: view(buf[9:15]), Data: view(buf[15:]), Retry: 5})
			for k := range buf {
				buf[k] = 'z'
			}
			client.mu.Unlock()
			if err != nil {
				t.Errorf("Send: %v", err)
				return
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if replay, replayErr = ring.Since(client.Context(), ""); len(replay) > 0 {
					return
				}
				time.Sleep(time.Millisecond)
			}
		},
	})
	if err := handler(ctx); err != nil {
		t.Fatal(err)
	}
	if replayErr != nil || len(replay) != 1 {
		t.Fatalf("the ring replays %+v, %v; want the one event", replay, replayErr)
	}
	if got := replay[0]; got.ID != "1" || got.Event != "kind-a" || got.Data != "msg-a" || got.Retry != 5 {
		t.Errorf("the ring kept ID %q Event %q Data %q Retry %d after the caller's bytes changed, want 1, kind-a, msg-a, 5",
			got.ID, got.Event, got.Data, got.Retry)
	}
	if wire := ms.allData(); !strings.Contains(wire, "event: kind-a\n") || !strings.Contains(wire, "data: msg-a\n") {
		t.Errorf("the wire carries %q after the caller's bytes changed, want event kind-a and data msg-a", wire)
	}
}
