package sse

import (
	"runtime"
	"sync"
)

// BrokerPolicy controls what happens to a slow subscriber whose
// per-subscriber queue is full at [Broker.Publish] time. Values mirror
// websocket.HubPolicy and [ClientPolicy] — same verbs, same semantics.
type BrokerPolicy uint8

const (
	// BrokerPolicyDrop silently discards the PreparedEvent for the slow
	// subscriber. Other subscribers are unaffected.
	BrokerPolicyDrop BrokerPolicy = iota

	// BrokerPolicyRemove unregisters the subscriber from the Broker
	// without closing the underlying [Client]. Use when the caller owns
	// the connection lifecycle and only wants the broker to stop
	// fanning events to that subscriber.
	BrokerPolicyRemove

	// BrokerPolicyClose unregisters the subscriber AND closes the
	// underlying [Client]. Use when prolonged backpressure indicates
	// a stuck client that should be evicted entirely.
	BrokerPolicyClose
)

// BrokerConfig tunes a [Broker]. All fields are optional; zero values
// produce reasonable defaults.
type BrokerConfig struct {
	// SubscriberBuffer bounds each subscriber's outbound queue inside the
	// broker. Fast subscribers never see backpressure; slow ones get the
	// OnSlowSubscriber policy applied. Default 64.
	SubscriberBuffer int

	// OnSlowSubscriber is consulted when a subscriber's queue is full at
	// Publish time. When nil, [BrokerPolicyDrop] is used.
	OnSlowSubscriber func(c *Client, pe *PreparedEvent) BrokerPolicy

	// SlowSubscriberConcurrency caps the number of in-flight per-
	// subscriber slow-path goroutines spawned by Publish/PublishPrepared
	// when one or more subscribers' queues are full. Zero means
	// [DefaultBrokerSlowConcurrency] — runtime.GOMAXPROCS(0)*4. Negative
	// opts out (true unbounded) — useful for benchmarks; production
	// callers should keep the cap on so a misbehaving OnSlowSubscriber
	// callback cannot fan out into thousands of goroutines.
	SlowSubscriberConcurrency int
}

// DefaultBrokerSubscriberBuffer is the queue capacity used when
// [BrokerConfig.SubscriberBuffer] is zero.
const DefaultBrokerSubscriberBuffer = 64

// DefaultBrokerSlowConcurrency is used when
// [BrokerConfig.SlowSubscriberConcurrency] is zero. Sized at
// GOMAXPROCS*4 — same heuristic as
// [middleware/websocket.DefaultHubConcurrency] — so a many-slow-
// subscriber publish parallelises up to a small multiple of CPU cores
// without spawning unbounded goroutines.
func DefaultBrokerSlowConcurrency() int { return runtime.GOMAXPROCS(0) * 4 }

// Broker fans out a single SSE event source to N subscribers without
// re-formatting. Each subscriber gets its own bounded outbound queue plus
// a dedicated drain goroutine; [Broker.Publish] does a single
// [FormatEvent] call and then non-blocking sends to every queue.
//
// Safe for concurrent use from any number of publishers and from any
// number of Subscribe / unsubscribe call sites.
type Broker struct {
	cfg BrokerConfig

	mu          sync.RWMutex
	subscribers map[*Client]*brokerSubscriber
	closed      bool
}

type brokerSubscriber struct {
	queue chan *PreparedEvent
	done  chan struct{}
	// closeOnce guards close(queue). Multiple paths may try to close
	// it: removeSubscriber called from a slow-disconnect goroutine,
	// the unsubscribe closure returned by Subscribe, and Broker.Close
	// during shutdown. sync.Once collapses them into a single safe
	// close — close-of-closed-channel is a runtime panic.
	closeOnce sync.Once
}

// closeQueue closes s.queue exactly once, regardless of how many
// callers reach this method.
func (s *brokerSubscriber) closeQueue() {
	s.closeOnce.Do(func() { close(s.queue) })
}

// NewBroker constructs a Broker with the provided config.
func NewBroker(cfg BrokerConfig) *Broker {
	if cfg.SubscriberBuffer <= 0 {
		cfg.SubscriberBuffer = DefaultBrokerSubscriberBuffer
	}
	return &Broker{
		cfg:         cfg,
		subscribers: make(map[*Client]*brokerSubscriber),
	}
}

// Subscribe registers c to receive every subsequent Publish. Spawns a
// drain goroutine that calls [Client.WritePreparedEvent] for each event.
// Returns an unsubscribe function the handler MUST defer; calling it
// twice is safe.
//
// Subscribing a Client to a Broker that has already been Close()'d is a
// no-op — the returned unsubscribe is also a no-op.
func (b *Broker) Subscribe(c *Client) (unsubscribe func()) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return func() {}
	}
	if _, exists := b.subscribers[c]; exists {
		b.mu.Unlock()
		return func() {}
	}
	s := &brokerSubscriber{
		queue: make(chan *PreparedEvent, b.cfg.SubscriberBuffer),
		done:  make(chan struct{}),
	}
	b.subscribers[c] = s
	b.mu.Unlock()

	go b.drain(c, s)

	var once sync.Once
	return func() {
		once.Do(func() {
			b.removeSubscriber(c, s)
			<-s.done
		})
	}
}

// drain consumes the per-subscriber queue and writes each PreparedEvent
// to the Client. Exits when the queue is closed (graceful) or a wire
// write fails (unhealthy — client is gone).
func (b *Broker) drain(c *Client, s *brokerSubscriber) {
	defer close(s.done)
	for pe := range s.queue {
		if err := c.WritePreparedEvent(pe); err != nil {
			return
		}
	}
}

// Publish formats e once into a [PreparedEvent], dispatches it to every
// current subscriber, and returns the PreparedEvent so the caller can
// reuse it (e.g. for replay-store appends).
//
// Ordering: per subscriber, events are delivered in publish order
// (the per-subscriber drain goroutine pulls from a FIFO channel).
// Across subscribers there is no global ordering guarantee — fan-out
// is concurrent and a fast subscriber may observe event N before a
// slow subscriber observes event N-1.
func (b *Broker) Publish(e Event) *PreparedEvent {
	pe := NewPreparedEvent(e)
	b.PublishPrepared(pe)
	return pe
}

// PublishPrepared dispatches an already-prepared event to every current
// subscriber. Each subscriber has its own bounded queue + drain
// goroutine, so wire I/O for a fast subscriber never gates a slow
// one. Slow subscribers — those whose queue is full at the non-
// blocking send attempt — have the configured [BrokerPolicy] applied.
//
// Slow-path concurrency: when N subscribers are slow, the per-
// subscriber policy callback + cleanup runs in parallel across
// goroutines bounded by [BrokerConfig.SlowSubscriberConcurrency]
// (default GOMAXPROCS*4). Total slow-path latency is therefore
// approximately max(callback) + cleanup rather than the sum across
// subscribers. PublishPrepared still WAITS for every spawned slow-
// path goroutine before returning — the join is required so a
// subsequent [Subscribe] cannot race a [Client.Close] from a prior
// policy firing (sync.Pool of *Client could otherwise hand a still-
// closing pointer to a fresh connection).
//
// Panic isolation: a panic inside a user OnSlowSubscriber callback
// is recovered inside the slow-path goroutine — other slow-path
// goroutines continue, and the publisher returns normally. The
// failing subscriber stays registered (the policy could not be
// honoured); the panic is otherwise swallowed because the publisher
// has no error channel to surface it on.
//
// Default OnSlowSubscriber == nil short-circuits without spawning
// any slow-path goroutines: events were already dropped at the
// non-blocking-send branch and no cleanup is required.
func (b *Broker) PublishPrepared(pe *PreparedEvent) {
	if pe == nil {
		return
	}
	var slowClients []*Client
	var slowStates []*brokerSubscriber

	b.mu.RLock()
	for c, s := range b.subscribers {
		select {
		case s.queue <- pe:
		default:
			slowClients = append(slowClients, c)
			slowStates = append(slowStates, s)
		}
	}
	b.mu.RUnlock()

	if len(slowClients) == 0 {
		return
	}
	policy := b.cfg.OnSlowSubscriber
	if policy == nil {
		// Default is BrokerPolicyDrop — nothing to clean up. The slow
		// events were already dropped at the non-blocking-send default
		// branch above; no further work.
		return
	}

	// Concurrency cap: 0 ⇒ DefaultBrokerSlowConcurrency (GOMAXPROCS*4);
	// negative ⇒ unbounded (one goroutine per slow subscriber). The
	// default keeps peak goroutine count bounded on a many-slow fan-
	// out without hurting the small-N case (the semaphore is unused
	// when len(slowClients) ≤ cap).
	var sema chan struct{}
	maxConc := b.cfg.SlowSubscriberConcurrency
	if maxConc == 0 {
		maxConc = DefaultBrokerSlowConcurrency()
	}
	if maxConc > 0 {
		sema = make(chan struct{}, maxConc)
	}

	var wg sync.WaitGroup
	wg.Add(len(slowClients))
	for i, c := range slowClients {
		state := slowStates[i]
		if sema != nil {
			sema <- struct{}{}
		}
		go func(c *Client, state *brokerSubscriber) {
			defer wg.Done()
			if sema != nil {
				defer func() { <-sema }()
			}
			// Recover from panics in the user policy callback. Without
			// this, one bad callback brings down every other in-flight
			// slow-path goroutine and the publisher itself. The
			// subscriber stays registered on panic — we could not run
			// the policy to decide otherwise.
			defer func() { _ = recover() }()
			switch policy(c, pe) {
			case BrokerPolicyDrop:
				// Keep the subscriber registered; this Publish dropped
				// its event but future ones may land.
			case BrokerPolicyRemove:
				b.removeSubscriber(c, state)
				<-state.done
			case BrokerPolicyClose:
				b.removeSubscriber(c, state)
				_ = c.Close()
				<-state.done
			}
		}(c, state)
	}
	// Wait gates publisher return on every slow-path goroutine. This
	// is the load-bearing guarantee: a *Client returned to sync.Pool
	// after the user's handler defer cannot be re-acquired by a fresh
	// connection while a goroutine here still holds it.
	wg.Wait()
}

func (b *Broker) removeSubscriber(c *Client, expected *brokerSubscriber) {
	b.mu.Lock()
	cur, ok := b.subscribers[c]
	if !ok || (expected != nil && cur != expected) {
		b.mu.Unlock()
		return
	}
	delete(b.subscribers, c)
	b.mu.Unlock()
	cur.closeQueue()
}

// SubscriberCount is a point-in-time gauge useful for observability.
// Reflects state at the moment of the call; concurrent Subscribe /
// unsubscribe calls may change the value before the caller observes it.
func (b *Broker) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// Close unsubscribes every current subscriber and blocks new Subscribe
// calls. Pending in-flight Publish calls complete in best-effort order.
// Idempotent.
func (b *Broker) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	states := make([]*brokerSubscriber, 0, len(b.subscribers))
	for c, s := range b.subscribers {
		delete(b.subscribers, c)
		states = append(states, s)
	}
	b.mu.Unlock()

	for _, s := range states {
		s.closeQueue()
	}
	for _, s := range states {
		<-s.done
	}
}
