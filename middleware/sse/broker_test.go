package sse

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

// runWithClient spins up the SSE handler with a no-op stream backing and
// hands the *Client to fn. It blocks until fn returns. Reused by every
// broker test so the handler-lifecycle plumbing stays in one place.
func runWithClient(t *testing.T, cfg Config, fn func(*Client)) {
	t.Helper()
	ctx, _ := newSSEContext(t)
	cfg.HeartbeatInterval = -1
	cfg.Handler = func(c *Client) { fn(c) }
	if err := New(cfg)(ctx); err != nil {
		t.Fatal(err)
	}
}

// runWithClients runs n parallel handlers and invokes ready with each
// client once it has been activated. The returned teardown closes stop
// and joins every handler — tests must call it before returning so the
// per-context t.Cleanup doesn't race the still-running handlers.
func runWithClients(t *testing.T, n int, ready func(*Client)) (clientStop chan struct{}, teardown func()) {
	t.Helper()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			runWithClient(t, Config{}, func(c *Client) {
				ready(c)
				<-stop
			})
		}()
	}
	var once sync.Once
	return stop, func() {
		once.Do(func() {
			close(stop)
			wg.Wait()
		})
	}
}

// TestPreparedEventLen pins the public surface: Len reports the
// formatted wire-byte count without exposing the underlying buffer.
// The wire round-trip is covered by TestWritePreparedEvent.
func TestPreparedEventLen(t *testing.T) {
	pe := NewPreparedEvent(Event{ID: "1", Event: "msg", Data: "hello"})
	want := len("id: 1\nevent: msg\ndata: hello\n\n")
	if pe.Len() != want {
		t.Errorf("Len() = %d, want %d", pe.Len(), want)
	}
}

// TestWritePreparedEvent verifies the prepared write reaches the wire
// unchanged from the cached bytes.
func TestWritePreparedEvent(t *testing.T) {
	ctx, ms := newSSEContext(t)
	pe := NewPreparedEvent(Event{Data: "ping"})
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler: func(c *Client) {
			if err := c.WritePreparedEvent(pe); err != nil {
				t.Errorf("WritePreparedEvent: %v", err)
			}
		},
	})
	if err := handler(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ms.allData(), "data: ping\n\n") {
		t.Errorf("wire missing prepared payload: %q", ms.allData())
	}
}

// TestBrokerFanOutFormatsOnce — the headline strict-alloc gate from
// issue #249. The per-subscriber loop must not allocate, so total allocs
// stay constant as the subscriber count grows. We verify this by
// measuring allocs at two subscriber counts and asserting scaling stays
// within a small tolerance.
func TestBrokerFanOutFormatsOnce(t *testing.T) {
	if raceEnabled || testing.CoverMode() != "" || testing.Short() {
		t.Skip("alloc counts unstable under -race / coverage / -short")
	}
	allocsLow := measureBrokerPublishAllocs(t, 50)
	allocsHigh := measureBrokerPublishAllocs(t, 500)
	// Scaling tolerance — allocs must not grow with N. 1 alloc of slack
	// covers map-iterator runtime variance.
	if allocsHigh > allocsLow+1 {
		t.Fatalf("Publish allocs scale with N: 50→%.1f, 500→%.1f", allocsLow, allocsHigh)
	}
	atomic.StoreUint64(&dummyKeepLive, uint64(allocsLow))
}

var dummyKeepLive uint64

func measureBrokerPublishAllocs(t *testing.T, subscribers int) float64 {
	t.Helper()

	b := NewBroker(BrokerConfig{SubscriberBuffer: 1024})

	gotClients := make(chan *Client, subscribers)
	_, teardown := runWithClients(t, subscribers, func(c *Client) {
		b.Subscribe(c)
		gotClients <- c
	})
	defer teardown()
	defer b.Close()

	for range subscribers {
		<-gotClients
	}
	for b.SubscriberCount() != subscribers {
		time.Sleep(time.Millisecond)
	}
	return testing.AllocsPerRun(20, func() {
		_ = b.Publish(Event{Data: "x"})
	})
}

// startSlowClient spins up an SSE handler whose underlying streamer is
// gated, so writes block until the returned release() function fires.
// Returns the *Client, a release callback, a cleanup that drives the
// handler to completion, and the gated streamer (so the caller can
// wait on `<-gate.writeReachedOnce` to synchronize on "drain has pulled
// an event and is now blocked at the gate"). Avoids sharing stop chans
// across roles.
func startSlowClient(t *testing.T) (slow *Client, release func(), cleanup func(), gate *gatedStreamer) {
	t.Helper()
	ctx, gs := newGatedContext(t)
	gate = gs
	ready := make(chan *Client, 1)
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		_ = New(Config{
			HeartbeatInterval: -1,
			Handler: func(c *Client) {
				ready <- c
				<-stop
			},
		})(ctx)
	}()
	slow = <-ready
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		gs.release()
	}
	cleanup = func() {
		release()
		close(stop)
		<-done
	}
	return slow, release, cleanup, gate
}

// TestBrokerSlowSubscriberDropPolicy: 1 slow client + many fast — the
// slow one's drops do not stall the fast ones.
func TestBrokerSlowSubscriberDropPolicy(t *testing.T) {
	const fastN = 10

	b := NewBroker(BrokerConfig{
		SubscriberBuffer: 2,
		OnSlowSubscriber: func(_ *Client, _ *PreparedEvent) BrokerPolicy {
			return BrokerPolicyDrop
		},
	})
	defer b.Close()

	slow, releaseSlow, slowCleanup, _ := startSlowClient(t)
	slowUnsub := b.Subscribe(slow)

	fastClients := make(chan *Client, fastN)
	_, fastTeardown := runWithClients(t, fastN, func(c *Client) {
		b.Subscribe(c)
		fastClients <- c
	})
	defer fastTeardown()
	for range fastN {
		<-fastClients
	}
	for b.SubscriberCount() != fastN+1 {
		time.Sleep(time.Millisecond)
	}

	const events = 50
	for range events {
		b.Publish(Event{Data: "x"})
	}
	time.Sleep(50 * time.Millisecond)

	// Order matters: release the gate first so the broker's drain
	// goroutine can complete its in-flight Write, then unsubscribe (which
	// closes its queue and joins the drain), then drive the handler home.
	releaseSlow()
	slowUnsub()
	slowCleanup()

	if b.SubscriberCount() != fastN {
		t.Errorf("SubscriberCount after slow unsub = %d, want %d", b.SubscriberCount(), fastN)
	}
}

// startDelayedClient spins up an SSE handler whose underlying streamer
// inserts a small delay on every Write. This models a real-world slow
// consumer (latent network) without the c.mu deadlock that a hard-gated
// streamer would create when policy actions need to call c.Close.
func startDelayedClient(t *testing.T, delay time.Duration) (slow *Client, cleanup func()) {
	t.Helper()
	ctx, _ := celeristest.NewContextT(t, "GET", "/events")
	s := celeris.TestStream(ctx)
	s.ResponseWriter = &delayStreamer{delay: delay}

	ready := make(chan *Client, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = New(Config{
			HeartbeatInterval: -1,
			Handler: func(c *Client) {
				ready <- c
				<-stop
			},
		})(ctx)
	}()
	slow = <-ready
	cleanup = func() {
		close(stop)
		<-done
	}
	return slow, cleanup
}

type delayStreamer struct {
	mockStreamer
	delay time.Duration
}

func (d *delayStreamer) Write(s *stream.Stream, data []byte) error {
	time.Sleep(d.delay)
	return d.mockStreamer.Write(s, data)
}

// TestBrokerSlowSubscriberClosePolicy verifies the close path
// removes the slow client from the broker AND closes its connection.
// Uses a delaying (not gated) streamer so c.Close in the policy path
// can acquire c.mu without waiting on a manually-released gate.
func TestBrokerSlowSubscriberClosePolicy(t *testing.T) {
	b := NewBroker(BrokerConfig{
		SubscriberBuffer: 1,
		OnSlowSubscriber: func(_ *Client, _ *PreparedEvent) BrokerPolicy {
			return BrokerPolicyClose
		},
	})
	defer b.Close()

	slow, slowCleanup := startDelayedClient(t, 25*time.Millisecond)
	defer slowCleanup()
	b.Subscribe(slow)

	// Publish faster than the 25ms drain delay so the SubscriberBuffer=1
	// queue fills and the policy fires.
	for range 10 {
		b.Publish(Event{Data: "x"})
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (b.SubscriberCount() != 0 || slow.Context().Err() == nil) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := b.SubscriberCount(); got != 0 {
		t.Errorf("SubscriberCount after disconnect = %d, want 0", got)
	}
	if slow.Context().Err() == nil {
		t.Errorf("slow client context not cancelled after disconnect policy")
	}
}

// TestBrokerSubscriberCountRace stresses Subscribe / unsubscribe / Close
// concurrently. Verified under -race for goroutine safety.
func TestBrokerSubscriberCountRace(t *testing.T) {
	const workers = 32
	const iterations = 50

	b := NewBroker(BrokerConfig{SubscriberBuffer: 4})
	defer b.Close()

	_, teardown := runWithClients(t, workers, func(c *Client) {
		for range iterations {
			unsub := b.Subscribe(c)
			b.Publish(Event{Data: "y"})
			unsub()
		}
	})
	defer teardown()

	// Brief publish pressure during the dance.
	for i := 0; i < 200; i++ {
		b.Publish(Event{Data: "z"})
	}
}

// TestBrokerCloseIdempotent + TestBrokerSubscribeAfterClose pin the
// terminal-state semantics that the spec spells out.
func TestBrokerCloseIdempotent(t *testing.T) {
	b := NewBroker(BrokerConfig{})
	b.Close()
	b.Close() // must not panic
	if b.SubscriberCount() != 0 {
		t.Errorf("SubscriberCount after Close = %d, want 0", b.SubscriberCount())
	}
}

func TestBrokerSubscribeAfterClose(t *testing.T) {
	b := NewBroker(BrokerConfig{})
	b.Close()
	runWithClient(t, Config{}, func(c *Client) {
		unsub := b.Subscribe(c)
		unsub() // no-op must not panic
		if b.SubscriberCount() != 0 {
			t.Errorf("Subscribe after Close registered the subscriber")
		}
	})
}

// TestBrokerUnsubscribeIdempotent — calling the returned unsubscribe
// closure twice must be a no-op, not double-decrement SubscriberCount
// or panic on close-of-closed-channel.
func TestBrokerUnsubscribeIdempotent(t *testing.T) {
	b := NewBroker(BrokerConfig{})
	defer b.Close()

	runWithClient(t, Config{}, func(c *Client) {
		unsub := b.Subscribe(c)
		if got := b.SubscriberCount(); got != 1 {
			t.Errorf("after Subscribe SubscriberCount = %d, want 1", got)
		}
		unsub()
		if got := b.SubscriberCount(); got != 0 {
			t.Errorf("after first unsub SubscriberCount = %d, want 0", got)
		}
		unsub() // must not panic
		if got := b.SubscriberCount(); got != 0 {
			t.Errorf("after second unsub SubscriberCount = %d, want 0", got)
		}
	})
}

// TestBrokerSubscribeRacingClose stresses the order between a fresh
// Subscribe and Close. Both paths take the broker mutex, so either
// Subscribe wins (state added, then Close removes it) or Close wins
// (Subscribe returns a no-op unsubscribe). Either is fine; what we
// guard against is panics or zombie drain goroutines after Close.
func TestBrokerSubscribeRacingClose(t *testing.T) {
	const workers = 32
	for range 50 {
		b := NewBroker(BrokerConfig{SubscriberBuffer: 4})

		_, teardown := runWithClients(t, workers, func(c *Client) {
			unsub := b.Subscribe(c)
			defer unsub()
		})
		// Race window: half the workers may still be inside Subscribe
		// when Close fires.
		time.Sleep(50 * time.Microsecond)
		b.Close()
		teardown()
	}
}

// BenchmarkSSEBrokerPublishTo100Subs — exit-criterion benchmark from
// issue #249. FormatEvent runs once per Publish regardless of N.
func BenchmarkSSEBrokerPublishTo100Subs(b *testing.B) {
	benchmarkBrokerFanOut(b, 100)
}

func BenchmarkSSEBrokerPublishTo1000Subs(b *testing.B) {
	benchmarkBrokerFanOut(b, 1000)
}

func benchmarkBrokerFanOut(b *testing.B, n int) {
	b.Helper()
	stop := make(chan struct{})
	defer close(stop)

	br := NewBroker(BrokerConfig{SubscriberBuffer: 1024})
	defer br.Close()

	for range n {
		ready := make(chan *Client, 1)
		go func() {
			ctx := newDiscardContext()
			_ = New(Config{
				HeartbeatInterval: -1,
				Handler: func(c *Client) {
					ready <- c
					<-stop
				},
			})(ctx)
		}()
		c := <-ready
		br.Subscribe(c)
	}
	for br.SubscriberCount() != n {
		time.Sleep(time.Millisecond)
	}

	ev := Event{Data: "x"}
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		br.Publish(ev)
	}
}

// TestBrokerSlowSubscriberRemovePolicy — BrokerPolicyRemove unregisters
// the subscriber from the broker without closing the underlying
// Client. Pins the new policy added in v1.4.2 alignment.
func TestBrokerSlowSubscriberRemovePolicy(t *testing.T) {
	b := NewBroker(BrokerConfig{
		SubscriberBuffer: 1,
		OnSlowSubscriber: func(_ *Client, _ *PreparedEvent) BrokerPolicy {
			return BrokerPolicyRemove
		},
	})
	defer b.Close()

	slow, slowCleanup := startDelayedClient(t, 25*time.Millisecond)
	defer slowCleanup()
	b.Subscribe(slow)

	// Publish faster than the slow streamer drains so the queue fills
	// and BrokerPolicyRemove fires.
	for range 10 {
		b.Publish(Event{Data: "x"})
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && b.SubscriberCount() != 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := b.SubscriberCount(); got != 0 {
		t.Errorf("SubscriberCount after Remove = %d, want 0", got)
	}
	// BrokerPolicyRemove does NOT close the client — handler ctx
	// stays live, lifecycle owned by the caller.
	if slow.Context().Err() != nil {
		t.Errorf("slow client context cancelled by Remove policy; should only be cancelled by Close")
	}
}

// TestBrokerCloseRacingSlowSubscriberClose — the close-of-closed-channel
// regression we fixed in S1: Broker.Close iterates states and closes
// each queue while a detached PublishPrepared goroutine running
// removeSubscriber → closeQueue races on the same state. With the
// closeOnce guard both paths are safe; without it this panicked.
func TestBrokerCloseRacingSlowSubscriberClose(t *testing.T) {
	for range 50 {
		b := NewBroker(BrokerConfig{
			SubscriberBuffer: 1,
			OnSlowSubscriber: func(_ *Client, _ *PreparedEvent) BrokerPolicy {
				return BrokerPolicyClose
			},
		})

		slow, slowCleanup := startDelayedClient(t, 5*time.Millisecond)
		b.Subscribe(slow)

		// Burst publish to schedule the disconnect goroutine; almost
		// immediately call Close to race the goroutine's
		// removeSubscriber → closeQueue against Close's closeQueue.
		for range 5 {
			b.Publish(Event{Data: "x"})
		}
		b.Close()
		slowCleanup()
	}
}

// TestBrokerSlowSubscriberPolicyCallbackInvoked pins that
// OnSlowSubscriber actually fires when a subscriber's queue overflows,
// regardless of the policy returned. Callbacks run on per-subscriber
// goroutines but the publisher waits on every one before returning,
// so any Remove/Close cleanup completes before pool reuse can race
// acquireClient.
func TestBrokerSlowSubscriberPolicyCallbackInvoked(t *testing.T) {
	var calls atomic.Int32
	b := NewBroker(BrokerConfig{
		SubscriberBuffer: 1,
		OnSlowSubscriber: func(_ *Client, _ *PreparedEvent) BrokerPolicy {
			calls.Add(1)
			return BrokerPolicyDrop
		},
	})
	defer b.Close()

	slow, slowCleanup := startDelayedClient(t, 5*time.Millisecond)
	defer slowCleanup()
	b.Subscribe(slow)

	for range 20 {
		b.Publish(Event{Data: "x"})
	}
	if got := calls.Load(); got == 0 {
		t.Errorf("OnSlowSubscriber never invoked despite queue overflow")
	}
}

// subscribeGatedSlow wires N gated slow subscribers to b, then arranges
// for each subscriber's queue to be full so a subsequent Publish hits
// the slow path for every one of them. Returns a teardown that releases
// every gate and joins every handler.
//
// Synchronisation contract:
//
//  1. Publish "fill1" → enqueue into every subscriber's queue (size 1).
//  2. Wait for each subscriber's gatedStreamer.writeReachedOnce — proof
//     that the drain goroutine has pulled fill1 and is now blocked at
//     the gate, so the queue is empty and the drain won't pull again.
//  3. Publish "fill2" → enqueue (queue size 1/1, drain still blocked).
//  4. Any subsequent Publish finds every queue full → slow path.
//
// Without (2), the drain may not yet have started by the time fill2 is
// enqueued, so fill2 hits the slow path on the wrong publish and the
// trigger publish fast-paths — causing
// TestBrokerSlowSubscriberConcurrencyCap to see only one callback's
// worth of elapsed time. Observed on shared CI runners.
func subscribeGatedSlow(t *testing.T, b *Broker, n int) (teardown func()) {
	t.Helper()
	releases := make([]func(), 0, n)
	cleanups := make([]func(), 0, n)
	gates := make([]*gatedStreamer, 0, n)
	for range n {
		slow, release, cleanup, gate := startSlowClient(t)
		b.Subscribe(slow)
		releases = append(releases, release)
		cleanups = append(cleanups, cleanup)
		gates = append(gates, gate)
	}
	// Step 1: enqueue fill1 to every subscriber.
	b.Publish(Event{Data: "fill1"})
	// Step 2: wait for every drain goroutine to reach the gate.
	for i, g := range gates {
		select {
		case <-g.writeReachedOnce:
		case <-time.After(2 * time.Second):
			t.Fatalf("subscribeGatedSlow: subscriber %d drain did not reach gate within 2s", i)
		}
	}
	// Step 3: enqueue fill2 — now strictly behind the blocked drain.
	b.Publish(Event{Data: "fill2"})
	return func() {
		for _, r := range releases {
			r()
		}
		for _, c := range cleanups {
			c()
		}
	}
}

// TestBrokerSlowSubscriberCallbackParallel — N slow subscribers with a
// callback that sleeps D each must complete in roughly D, not N*D.
// This locks in the parallel slow-path semantics so a future revert to
// the serial loop is caught by CI.
func TestBrokerSlowSubscriberCallbackParallel(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive; skipped under -short")
	}
	const slowN = 16
	const callbackDelay = 50 * time.Millisecond

	b := NewBroker(BrokerConfig{
		SubscriberBuffer: 1,
		OnSlowSubscriber: func(_ *Client, _ *PreparedEvent) BrokerPolicy {
			time.Sleep(callbackDelay)
			return BrokerPolicyDrop
		},
	})
	defer b.Close()

	teardown := subscribeGatedSlow(t, b, slowN)
	defer teardown()

	start := time.Now()
	b.Publish(Event{Data: "trigger"})
	elapsed := time.Since(start)

	// Serial would be slowN * callbackDelay (~800ms). Parallel should be
	// roughly callbackDelay (~50ms) plus a small constant. Allow 4x
	// callbackDelay as the upper bound — generous enough to absorb CI
	// jitter, tight enough to fail if someone reverts to the serial loop.
	upper := 4 * callbackDelay
	if elapsed >= upper {
		t.Fatalf("Publish(slow=%d, cb=%v) took %v, want < %v — slow path serialised?",
			slowN, callbackDelay, elapsed, upper)
	}
}

// TestBrokerSlowSubscriberCallbackPanicIsolated — a panic inside the
// user's OnSlowSubscriber callback must NOT crash the publisher or
// gate other slow subscribers' policies.
func TestBrokerSlowSubscriberCallbackPanicIsolated(t *testing.T) {
	const slowN = 8
	var panicked atomic.Int32
	var dropCalls atomic.Int32

	b := NewBroker(BrokerConfig{
		SubscriberBuffer: 1,
		OnSlowSubscriber: func(_ *Client, _ *PreparedEvent) BrokerPolicy {
			// Panic for the FIRST goroutine to enter the callback;
			// everyone else gets a normal Drop. CAS guarantees
			// exactly-one panic regardless of fan-out scheduling.
			if panicked.CompareAndSwap(0, 1) {
				panic("intentional test panic")
			}
			dropCalls.Add(1)
			return BrokerPolicyDrop
		},
	})
	defer b.Close()

	teardown := subscribeGatedSlow(t, b, slowN)
	defer teardown()

	// Trigger. The publisher must NOT propagate the panic; if it did,
	// the test process would crash and we'd never reach the assertions.
	b.Publish(Event{Data: "trigger"})

	if panicked.Load() != 1 {
		t.Fatalf("expected exactly one goroutine to panic, got panicked=%d", panicked.Load())
	}
	if got := dropCalls.Load(); got == 0 {
		t.Fatalf("non-panicking subscribers' callbacks never ran (panic gated others?)")
	}
}

// TestBrokerSlowSubscriberConcurrencyCap — at SlowSubscriberConcurrency=1
// the slow path serialises across subscribers. Used to prove the cap is
// load-bearing for users who want to dampen burst goroutine pressure.
func TestBrokerSlowSubscriberConcurrencyCap(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive; skipped under -short")
	}
	const slowN = 4
	// 80 ms × 4 callbacks × cap-1 ≈ 320 ms serialised. Bigger callback
	// than would happen in practice, but the headroom is what makes
	// the floor robust against scheduler jitter on shared CI runners
	// (an earlier 30 ms × 4 → floor 72 ms run flaked at 61.5 ms when
	// the host was under load — there was no real cap regression,
	// the floor was just too close to the no-op elapsed time).
	const callbackDelay = 80 * time.Millisecond

	b := NewBroker(BrokerConfig{
		SubscriberBuffer:          1,
		SlowSubscriberConcurrency: 1, // force serialisation
		OnSlowSubscriber: func(_ *Client, _ *PreparedEvent) BrokerPolicy {
			time.Sleep(callbackDelay)
			return BrokerPolicyDrop
		},
	})
	defer b.Close()

	teardown := subscribeGatedSlow(t, b, slowN)
	defer teardown()

	start := time.Now()
	b.Publish(Event{Data: "trigger"})
	elapsed := time.Since(start)

	// Serialised: slowN * callbackDelay = 320 ms. With a 0.5 floor we
	// fail any cap regression that runs faster than 160 ms — well
	// above the ~few-ms parallel-fanout achievable without the cap,
	// so the test remains discriminating.
	floor := time.Duration(float64(slowN*callbackDelay) * 0.5)
	if elapsed < floor {
		t.Fatalf("Publish elapsed %v < floor %v — concurrency cap not enforced?",
			elapsed, floor)
	}
}

// (BrokerPolicyClose under fan-out is covered by the existing
// TestBrokerSlowSubscriberClosePolicy — adding a parallel variant
// adds no signal because the fan-out machinery itself is policy-
// agnostic and is already exercised by the Drop / Cap tests above.)

// TestBrokerSubscribeSameClientTwice — calling Subscribe twice with
// the same Client returns a no-op unsubscribe and leaves the
// SubscriberCount at 1. Without this guard, a defer-pair'd handler
// that re-subscribes would silently double-register the same client
// and double-deliver every Publish.
func TestBrokerSubscribeSameClientTwice(t *testing.T) {
	b := NewBroker(BrokerConfig{})
	defer b.Close()

	ctx, _ := newSSEContext(t)
	cfg := Config{HeartbeatInterval: -1, Handler: func(c *Client) {
		unsub1 := b.Subscribe(c)
		unsub2 := b.Subscribe(c) // double subscribe
		if got := b.SubscriberCount(); got != 1 {
			t.Errorf("after double-Subscribe SubscriberCount=%d, want 1", got)
		}
		// Both unsubs must be safe to call (twice is also safe per Subscribe doc).
		unsub2()
		unsub1()
		if got := b.SubscriberCount(); got != 0 {
			t.Errorf("after both unsubs SubscriberCount=%d, want 0", got)
		}
	}}
	if err := New(cfg)(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestBrokerCallbackPanicsCounter — every recovered panic in a slow-
// path policy callback bumps the CallbackPanics counter so an
// observability pipeline can alert on a misbehaving callback instead
// of silent panic-eating.
func TestBrokerCallbackPanicsCounter(t *testing.T) {
	const slowN = 4
	var observed atomic.Int32
	b := NewBroker(BrokerConfig{
		SubscriberBuffer: 1,
		OnSlowSubscriber: func(_ *Client, _ *PreparedEvent) BrokerPolicy {
			observed.Add(1)
			panic("intentional test panic")
		},
	})
	defer b.Close()

	teardown := subscribeGatedSlow(t, b, slowN)
	defer teardown()

	b.Publish(Event{Data: "trigger"})

	// Across fill+trigger every subscriber's queue saturates exactly
	// once → callback fires exactly once per subscriber, panics every
	// time. The race in fill timing means some panics happen during
	// setup and some on trigger, but the *total* must match `observed`.
	// Pin the load-bearing property: every recovered panic increments
	// the counter — counter == observed after the publisher's wg.Wait.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if int(observed.Load()) >= slowN && b.CallbackPanics() == uint64(observed.Load()) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := observed.Load(); got < int32(slowN) {
		t.Fatalf("observed=%d, want >= %d (slow path didn't fire on every subscriber)", got, slowN)
	}
	if got, want := b.CallbackPanics(), uint64(observed.Load()); got != want {
		t.Errorf("CallbackPanics=%d, want %d (recover must count every panic)", got, want)
	}
}

// TestBrokerConfigNegativeSubscriberBuffer — negative SubscriberBuffer
// is normalised to the default. Pin the silent-clamp behaviour so a
// regression that lets through a chan-of-cap=-1 (panic on make) is
// caught.
func TestBrokerConfigNegativeSubscriberBuffer(t *testing.T) {
	b := NewBroker(BrokerConfig{SubscriberBuffer: -1})
	defer b.Close()
	// If we got here without panic, the negative value was clamped.
	// Sanity: the broker should accept a Subscribe and Publish.
	ctx, _ := newSSEContext(t)
	done := make(chan struct{})
	handlerDone := make(chan struct{})
	cfg := Config{HeartbeatInterval: -1, Handler: func(c *Client) {
		unsub := b.Subscribe(c)
		defer unsub()
		<-done
	}}
	go func() {
		defer close(handlerDone)
		_ = New(cfg)(ctx)
	}()
	for b.SubscriberCount() != 1 {
		time.Sleep(time.Millisecond)
	}
	b.Publish(Event{Data: "x"})
	close(done)
	// Wait for the handler to fully unwind before letting t.Cleanup
	// release the *Context — otherwise the deferred Detach inside
	// New(cfg)(ctx) races the celeristest cleanup hook.
	<-handlerDone
}
