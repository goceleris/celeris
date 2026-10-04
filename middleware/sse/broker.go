package sse

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
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
	// Publish time. When nil, [BrokerPolicyDrop] is used. The Drop
	// default is deliberate for SSE: dropping one event from one slow
	// subscriber leaves the stream coherent (the next event with a
	// fresh id replaces it, and replay via Last-Event-ID can recover).
	// websocket.HubConfig.OnSlowConn defaults to Close instead — a
	// dropped WS frame would corrupt the message-boundary contract,
	// so the safer default is to evict the bad peer.
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

	// slowSema is the bounded semaphore for slow-path policy goroutines.
	// Cached on the Broker rather than allocated per Publish — under
	// sustained slow-subscriber load (queues briefly full during head-
	// of-line bursts) every Publish would otherwise allocate a fresh
	// chan + slice. nil means SlowSubscriberConcurrency was negative
	// (opt-out for benchmarks); a positive cap is materialised once at
	// construction time.
	slowSema chan struct{}

	// callbackPanics counts user OnSlowSubscriber callback panics
	// recovered by the slow-path goroutine. Surfaced via
	// [Broker.CallbackPanics] so operators can alert on a misbehaving
	// callback instead of having panics swallowed silently.
	callbackPanics atomic.Uint64
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
	maxConc := cfg.SlowSubscriberConcurrency
	if maxConc == 0 {
		maxConc = DefaultBrokerSlowConcurrency()
	}
	var sema chan struct{}
	if maxConc > 0 {
		sema = make(chan struct{}, maxConc)
	}
	return &Broker{
		cfg:         cfg,
		subscribers: make(map[*Client]*brokerSubscriber),
		slowSema:    sema,
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
	// The drain goroutine uses c until it exits; releaseClient waits for
	// it, so c is not pooled while the drain still holds it (celeris#926).
	c.brokerRefs.Add(1)
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
// to the Client. Exits when the queue is closed (graceful) or a wire write
// fails (unhealthy — client is gone).
//
// When the Client's context ends, the subscriber is unsubscribed, which
// closes the queue and so ends the drain: its writes would fail anyway,
// and the handler's teardown, which cancels that context, waits for the
// drain before the Client goes back to its pool (brokerRefs), also when a
// policy removed the subscriber and the handler does not unsubscribe
// (celeris#926). context.AfterFunc costs nothing per event; a select on
// the context in this loop measured +36% on the fan-out benchmark.
//
// However the drain exits (a failed write included), it unsubscribes before
// it is done (deferred calls run last to first): the AfterFunc may still be
// on its way, and a Client must not reach its pool, once brokerRefs drops,
// while it is still in the subscriber map.
func (b *Broker) drain(c *Client, s *brokerSubscriber) {
	defer c.brokerRefs.Done()
	defer close(s.done)
	defer b.removeSubscriber(c, s)
	stop := context.AfterFunc(c.Context(), func() { b.removeSubscriber(c, s) })
	defer stop()
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
// subscriber policy callback runs in parallel across goroutines bounded
// by [BrokerConfig.SlowSubscriberConcurrency] (default GOMAXPROCS*4).
// Total slow-path latency is therefore approximately max(callback)
// rather than the sum across subscribers. PublishPrepared waits for
// every callback and its policy's unregistration before returning, so
// when it returns a subscriber the policy removed or closed is no longer
// registered, and under [BrokerPolicyClose] its context is cancelled.
//
// It does not wait for the slow subscriber's client (celeris#926). That
// client is slow because its writes block: under [BrokerPolicyRemove]
// its drain goroutine finishes the writes already queued on its own,
// and under [BrokerPolicyClose] the [Client.Close] that needs the
// client's lock, held by the blocked write, runs in its own goroutine.
// A Client stays out of its sync.Pool until such a goroutine is done
// with it, so a fresh connection never gets a Client that one of them
// can still close.
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
	policy := b.cfg.OnSlowSubscriber
	var slowClients []*Client
	var slowStates []*brokerSubscriber

	b.mu.RLock()
	for c, s := range b.subscribers {
		select {
		case s.queue <- pe:
		default:
			if policy == nil {
				// Default is BrokerPolicyDrop — nothing to clean up: the
				// event was dropped by this non-blocking send.
				continue
			}
			// The slow path uses c after the lock is released. Count it
			// on c now, while c is subscribed: the handler's unsubscribe
			// takes b.mu, so it cannot have completed, and releaseClient
			// waits for the count before c goes back to its pool.
			c.brokerRefs.Add(1)
			slowClients = append(slowClients, c)
			slowStates = append(slowStates, s)
		}
	}
	b.mu.RUnlock()

	if len(slowClients) == 0 {
		return
	}

	// Concurrency cap is materialised once on the Broker (see NewBroker).
	// Re-aliased here so the loop body reads sema, not b.slowSema.
	sema := b.slowSema

	var wg sync.WaitGroup
	wg.Add(len(slowClients))
	for i, c := range slowClients {
		state := slowStates[i]
		if sema != nil {
			sema <- struct{}{}
		}
		go func(c *Client, state *brokerSubscriber) {
			closeClient := false
			defer func() {
				if closeClient {
					// Client.Close takes c.mu, which the stuck write
					// holds: close it in a goroutine of its own, which
					// neither the publisher nor a semaphore slot waits
					// for. c's context is already cancelled.
					go func() {
						defer c.brokerRefs.Done()
						_ = c.Close()
					}()
					return
				}
				c.brokerRefs.Done()
			}()
			defer wg.Done()
			if sema != nil {
				defer func() { <-sema }()
			}
			// Recover from panics in the user policy callback. Without
			// this, one bad callback brings down every other in-flight
			// slow-path goroutine and the publisher itself. The
			// subscriber stays registered on panic — we could not run
			// the policy to decide otherwise. callbackPanics is
			// incremented for observability via [Broker.CallbackPanics].
			defer func() {
				if r := recover(); r != nil {
					b.callbackPanics.Add(1)
				}
			}()
			switch policy(c, pe) {
			case BrokerPolicyDrop:
				// Keep the subscriber registered; this Publish dropped
				// its event but future ones may land.
			case BrokerPolicyRemove:
				// The drain writes out what is queued and exits; the
				// handler's unsubscribe joins it. Waiting for it here
				// would wait for the slow client (celeris#926).
				b.removeSubscriber(c, state)
			case BrokerPolicyClose:
				b.removeSubscriber(c, state)
				// Cancel now, without c.mu: the handler, Send and the
				// heartbeat see the close at once.
				c.cancel()
				closeClient = true
			}
		}(c, state)
	}
	// Wait for every policy callback and unregistration; none of them
	// waits for a slow client's write.
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

// CallbackPanics returns the cumulative count of user
// OnSlowSubscriber callback panics recovered by the broker's slow-
// path goroutines. A non-zero value points at a misbehaving callback
// — the broker keeps running (panic was caught), but the affected
// subscriber's policy decision was lost so it stays registered with
// whatever queue state it had. Surface this via your metrics pipeline
// to avoid silent callback breakage.
func (b *Broker) CallbackPanics() uint64 {
	return b.callbackPanics.Load()
}

// Close unsubscribes every current subscriber and blocks new Subscribe
// calls. Pending in-flight Publish calls complete in best-effort order.
// Idempotent.
//
// Close closes every subscriber's queue and returns: it does not wait for
// the drain goroutines, which write out what is already queued and exit on
// their own, so a subscriber whose client stopped reading does not hold
// Close (celeris#926). Each subscriber's handler joins its drain through
// its unsubscribe, and the Client stays out of its pool until the drain is
// done with it.
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
}
