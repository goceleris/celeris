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

// TestPreparedEventFormatOnce pins the contract that NewPreparedEvent
// formats wire bytes a single time — the byte slice must round-trip
// through repeated Bytes() calls without re-encoding.
func TestPreparedEventFormatOnce(t *testing.T) {
	pe := NewPreparedEvent(Event{ID: "1", Event: "msg", Data: "hello"})
	want := "id: 1\nevent: msg\ndata: hello\n\n"
	if got := string(pe.Bytes()); got != want {
		t.Errorf("Bytes() = %q, want %q", got, want)
	}
	first := &pe.Bytes()[0]
	second := &pe.Bytes()[0]
	if first != second {
		t.Errorf("Bytes() returns a different backing array on repeated calls")
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
// Returns the *Client, a release callback, and a cleanup that drives the
// handler to completion. Avoids sharing stop chans across roles.
func startSlowClient(t *testing.T) (slow *Client, release func(), cleanup func()) {
	t.Helper()
	ctx, gate := newGatedContext(t)
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
		gate.release()
	}
	cleanup = func() {
		release()
		close(stop)
		<-done
	}
	return slow, release, cleanup
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

	slow, releaseSlow, slowCleanup := startSlowClient(t)
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

// TestBrokerSlowSubscriberDisconnectPolicy verifies the disconnect path
// removes the slow client from the broker AND closes its connection.
// Uses a delaying (not gated) streamer so c.Close in the policy path
// can acquire c.mu without waiting on a manually-released gate.
func TestBrokerSlowSubscriberDisconnectPolicy(t *testing.T) {
	b := NewBroker(BrokerConfig{
		SubscriberBuffer: 1,
		OnSlowSubscriber: func(_ *Client, _ *PreparedEvent) BrokerPolicy {
			return BrokerPolicyDisconnect
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
