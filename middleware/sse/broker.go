package sse

import "sync"

// BrokerPolicy controls what happens to a slow subscriber whose
// per-subscriber queue is full at [Broker.Publish] time.
type BrokerPolicy uint8

const (
	// BrokerPolicyDrop silently discards the PreparedEvent for the slow
	// subscriber. Other subscribers are unaffected.
	BrokerPolicyDrop BrokerPolicy = iota

	// BrokerPolicyDisconnect removes the subscriber from the Broker and
	// closes its underlying connection. Use when prolonged backpressure
	// indicates a stuck client that should be evicted.
	BrokerPolicyDisconnect
)

// BrokerConfig tunes a [Broker]. Both fields are optional; zero values
// produce reasonable defaults.
type BrokerConfig struct {
	// SubscriberBuffer bounds each subscriber's outbound queue inside the
	// broker. Fast subscribers never see backpressure; slow ones get the
	// OnSlowSubscriber policy applied. Default 64.
	SubscriberBuffer int

	// OnSlowSubscriber is consulted when a subscriber's queue is full at
	// Publish time. When nil, [BrokerPolicyDrop] is used.
	OnSlowSubscriber func(c *Client, pe *PreparedEvent) BrokerPolicy
}

// DefaultBrokerSubscriberBuffer is the queue capacity used when
// [BrokerConfig.SubscriberBuffer] is zero.
const DefaultBrokerSubscriberBuffer = 64

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
func (b *Broker) Publish(e Event) *PreparedEvent {
	pe := NewPreparedEvent(e)
	b.PublishPrepared(pe)
	return pe
}

// PublishPrepared dispatches an already-prepared event to every current
// subscriber. Slow subscribers — those whose queue is full at the
// non-blocking send attempt — have the configured [BrokerPolicy] applied.
// Fan-out is concurrent in effect (each subscriber has its own goroutine
// draining its queue) and Publish itself does NOT block on wire I/O,
// the policy callback, or per-subscriber Close — those run in
// dedicated goroutines so the publisher returns as soon as the
// non-blocking send loop completes.
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
	// Run the policy callbacks (and any Disconnect cleanup) in a
	// detached goroutine so the publisher does not gate on a slow
	// user callback or a remote socket close.
	policy := b.cfg.OnSlowSubscriber
	go func(clients []*Client, states []*brokerSubscriber) {
		for i, c := range clients {
			action := BrokerPolicyDrop
			if policy != nil {
				action = policy(c, pe)
			}
			if action == BrokerPolicyDisconnect {
				state := states[i]
				b.removeSubscriber(c, state)
				_ = c.Close()
				<-state.done
			}
		}
	}(slowClients, slowStates)
}

func (b *Broker) removeSubscriber(c *Client, expected *brokerSubscriber) {
	b.mu.Lock()
	cur, ok := b.subscribers[c]
	if !ok || (expected != nil && cur != expected) {
		b.mu.Unlock()
		return
	}
	delete(b.subscribers, c)
	close(cur.queue)
	b.mu.Unlock()
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
		close(s.queue)
	}
	for _, s := range states {
		<-s.done
	}
}
