package redis

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
)

// Message is one pub/sub notification. Pattern is set only for PSUBSCRIBE
// deliveries ("pmessage" push type). Shard is true for shard channel
// deliveries ("smessage" push type, Redis 7+).
type Message struct {
	Channel string
	Pattern string
	Payload string
	Shard   bool
}

// PubSub is a dedicated pub/sub connection. Callers read from [PubSub.Channel]
// and use [PubSub.Subscribe] / [PubSub.Unsubscribe] / [PubSub.PSubscribe] /
// [PubSub.PUnsubscribe] to alter the subscription set.
//
// The connection is re-established automatically on transport errors: a
// hook registered with the underlying redisConn fires when the FD closes and
// a background goroutine dials a replacement, re-sends SUBSCRIBE/PSUBSCRIBE
// for every tracked channel/pattern, and resumes message delivery. Messages
// arriving while the conn is down are lost (at-most-once delivery).
//
// Callers should treat subscribe/unsubscribe as "at most once" — the server
// ack is not tracked synchronously. The channel returned by [PubSub.Channel]
// is closed once [PubSub.Close] is called or the reconnect loop gives up.
type PubSub struct {
	client *Client

	mu        sync.Mutex
	conn      *redisConn
	subs      map[string]struct{}
	psubs     map[string]struct{}
	shardSubs map[string]struct{}
	msgCh   chan *Message
	closeCh chan struct{}
	closed  atomic.Bool

	// reconnecting is set while a reconnect goroutine is active. Guards
	// against double-fires from both onRecv-error and onClose in the same
	// conn teardown.
	reconnectingMu sync.Mutex
	reconnecting   bool

	closeMsgOnce sync.Once

	backoff *async.Backoff
	// drops counts Messages dropped because msgCh was full.
	drops atomic.Uint64
}

// Subscribe opens a pub/sub conn and subscribes to channels.
func (c *Client) Subscribe(ctx context.Context, channels ...string) (*PubSub, error) {
	ps, err := c.newPubSub(ctx)
	if err != nil {
		return nil, err
	}
	if len(channels) > 0 {
		if err := ps.Subscribe(ctx, channels...); err != nil {
			_ = ps.Close()
			return nil, err
		}
	}
	return ps, nil
}

// PSubscribe opens a pub/sub conn and subscribes to patterns.
func (c *Client) PSubscribe(ctx context.Context, patterns ...string) (*PubSub, error) {
	ps, err := c.newPubSub(ctx)
	if err != nil {
		return nil, err
	}
	if len(patterns) > 0 {
		if err := ps.PSubscribe(ctx, patterns...); err != nil {
			_ = ps.Close()
			return nil, err
		}
	}
	return ps, nil
}

func (c *Client) newPubSub(ctx context.Context) (*PubSub, error) {
	conn, err := c.pool.acquirePubSub(ctx)
	if err != nil {
		return nil, err
	}
	ps := &PubSub{
		client:    c,
		conn:      conn,
		subs:      map[string]struct{}{},
		psubs:     map[string]struct{}{},
		shardSubs: map[string]struct{}{},
		msgCh:     make(chan *Message, defaultPubSubChanBuf),
		closeCh:   make(chan struct{}),
		backoff:   async.NewBackoff(50*time.Millisecond, 5*time.Second),
	}
	ps.bindConn(conn)
	return ps, nil
}

// bindConn wires ps into conn: router + close hook.
func (ps *PubSub) bindConn(conn *redisConn) {
	conn.state.router.set(ps)
	conn.setPubSubCloseHook(ps.onConnDrop)
}

// Channel returns the read-only message stream. It remains open until Close
// is called or the reconnect loop exhausts its retries.
func (ps *PubSub) Channel() <-chan *Message {
	return ps.msgCh
}

// Drops returns the number of messages dropped because the channel buffer was
// full.
func (ps *PubSub) Drops() uint64 { return ps.drops.Load() }

// Subscribe adds channels to the subscription set.
func (ps *PubSub) Subscribe(ctx context.Context, channels ...string) error {
	if ps.closed.Load() {
		return ErrClosed
	}
	ps.mu.Lock()
	for _, ch := range channels {
		ps.subs[ch] = struct{}{}
	}
	conn := ps.conn
	ps.mu.Unlock()
	args := append([]string{"SUBSCRIBE"}, channels...)
	return sendPubSubControl(conn, args)
}

// Unsubscribe removes channels. Empty list unsubscribes all.
func (ps *PubSub) Unsubscribe(ctx context.Context, channels ...string) error {
	if ps.closed.Load() {
		return ErrClosed
	}
	ps.mu.Lock()
	if len(channels) == 0 {
		ps.subs = map[string]struct{}{}
	} else {
		for _, ch := range channels {
			delete(ps.subs, ch)
		}
	}
	conn := ps.conn
	ps.mu.Unlock()
	args := append([]string{"UNSUBSCRIBE"}, channels...)
	return sendPubSubControl(conn, args)
}

// PSubscribe adds patterns to the subscription set.
func (ps *PubSub) PSubscribe(ctx context.Context, patterns ...string) error {
	if ps.closed.Load() {
		return ErrClosed
	}
	ps.mu.Lock()
	for _, p := range patterns {
		ps.psubs[p] = struct{}{}
	}
	conn := ps.conn
	ps.mu.Unlock()
	args := append([]string{"PSUBSCRIBE"}, patterns...)
	return sendPubSubControl(conn, args)
}

// PUnsubscribe removes patterns. Empty list unsubscribes all patterns.
func (ps *PubSub) PUnsubscribe(ctx context.Context, patterns ...string) error {
	if ps.closed.Load() {
		return ErrClosed
	}
	ps.mu.Lock()
	if len(patterns) == 0 {
		ps.psubs = map[string]struct{}{}
	} else {
		for _, p := range patterns {
			delete(ps.psubs, p)
		}
	}
	conn := ps.conn
	ps.mu.Unlock()
	args := append([]string{"PUNSUBSCRIBE"}, patterns...)
	return sendPubSubControl(conn, args)
}

// SSubscribe adds shard channels (Redis 7+ SSUBSCRIBE) to the subscription
// set. Shard channels are scoped to the cluster slot of the channel name.
func (ps *PubSub) SSubscribe(ctx context.Context, channels ...string) error {
	if ps.closed.Load() {
		return ErrClosed
	}
	ps.mu.Lock()
	for _, ch := range channels {
		ps.shardSubs[ch] = struct{}{}
	}
	conn := ps.conn
	ps.mu.Unlock()
	args := append([]string{"SSUBSCRIBE"}, channels...)
	return sendPubSubControl(conn, args)
}

// SUnsubscribe removes shard channels. Empty list unsubscribes all shard
// channels.
func (ps *PubSub) SUnsubscribe(ctx context.Context, channels ...string) error {
	if ps.closed.Load() {
		return ErrClosed
	}
	ps.mu.Lock()
	if len(channels) == 0 {
		ps.shardSubs = map[string]struct{}{}
	} else {
		for _, ch := range channels {
			delete(ps.shardSubs, ch)
		}
	}
	conn := ps.conn
	ps.mu.Unlock()
	args := append([]string{"SUNSUBSCRIBE"}, channels...)
	return sendPubSubControl(conn, args)
}

// sendPubSubControl writes a control command without tracking a reply (pubsub
// control frames are delivered as push).
func sendPubSubControl(conn *redisConn, args []string) error {
	if conn == nil {
		return ErrClosed
	}
	_, err := conn.writeCommand(args...)
	return err
}

// Close tears down the pubsub conn and closes msgCh.
func (ps *PubSub) Close() error {
	if !ps.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(ps.closeCh)
	ps.mu.Lock()
	conn := ps.conn
	ps.conn = nil
	ps.mu.Unlock()
	if conn != nil {
		conn.state.router.clear()
		conn.setPubSubCloseHook(nil)
		ps.client.pool.releasePubSub(conn)
	}
	ps.closeMsgCh()
	return nil
}

// closeMsgCh closes msgCh exactly once. Holds ps.mu to serialize with deliver.
func (ps *PubSub) closeMsgCh() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.closeMsgOnce.Do(func() { close(ps.msgCh) })
}

// deliver pushes a message to msgCh; drops if the buffer is full. Holds ps.mu
// to serialize with closeMsgCh, preventing sends on a closed channel.
func (ps *PubSub) deliver(m *Message) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.closed.Load() {
		return false
	}
	select {
	case ps.msgCh <- m:
		return true
	default:
		ps.drops.Add(1)
		// Drop oldest to make room.
		select {
		case <-ps.msgCh:
		default:
		}
		select {
		case ps.msgCh <- m:
			return true
		default:
			return false
		}
	}
}

// onConnDrop is invoked (on its own goroutine) by the underlying conn's close
// hook. It spawns the reconnect loop unless one is already running or the
// PubSub is closed.
func (ps *PubSub) onConnDrop(_ error) {
	if ps.closed.Load() {
		return
	}
	ps.reconnectingMu.Lock()
	if ps.reconnecting {
		ps.reconnectingMu.Unlock()
		return
	}
	ps.reconnecting = true
	ps.reconnectingMu.Unlock()
	go ps.reconnectLoop()
}

// reconnectLoop re-dials the pubsub conn and replays subscriptions with
// exponential backoff. It terminates when the pubsub is closed or the backoff
// reaches its cap and a configured retry budget is exhausted (here we keep
// retrying indefinitely, but every iteration honors closeCh).
func (ps *PubSub) reconnectLoop() {
	defer func() {
		ps.reconnectingMu.Lock()
		ps.reconnecting = false
		ps.reconnectingMu.Unlock()
	}()
	for attempt := 0; ; attempt++ {
		if ps.closed.Load() {
			return
		}
		delay := ps.backoff.Next(attempt)
		select {
		case <-time.After(delay):
		case <-ps.closeCh:
			return
		}
		if ps.closed.Load() {
			return
		}
		// Give the dial a bounded window so a hard-down server doesn't
		// block reconnect forever on a single attempt.
		dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := ps.client.pool.acquirePubSub(dialCtx)
		cancel()
		if err != nil {
			continue
		}
		ps.mu.Lock()
		if ps.closed.Load() {
			ps.mu.Unlock()
			_ = conn.Close()
			ps.client.pool.releasePubSub(conn)
			return
		}
		ps.conn = conn
		subs := make([]string, 0, len(ps.subs))
		for s := range ps.subs {
			subs = append(subs, s)
		}
		psubs := make([]string, 0, len(ps.psubs))
		for p := range ps.psubs {
			psubs = append(psubs, p)
		}
		ssubs := make([]string, 0, len(ps.shardSubs))
		for s := range ps.shardSubs {
			ssubs = append(ssubs, s)
		}
		ps.mu.Unlock()
		ps.bindConn(conn)
		if len(subs) > 0 {
			if err := sendPubSubControl(conn, append([]string{"SUBSCRIBE"}, subs...)); err != nil {
				ps.mu.Lock()
				ps.conn = nil
				ps.mu.Unlock()
				_ = conn.Close()
				continue
			}
		}
		if len(psubs) > 0 {
			if err := sendPubSubControl(conn, append([]string{"PSUBSCRIBE"}, psubs...)); err != nil {
				ps.mu.Lock()
				ps.conn = nil
				ps.mu.Unlock()
				_ = conn.Close()
				continue
			}
		}
		if len(ssubs) > 0 {
			if err := sendPubSubControl(conn, append([]string{"SSUBSCRIBE"}, ssubs...)); err != nil {
				ps.mu.Lock()
				ps.conn = nil
				ps.mu.Unlock()
				_ = conn.Close()
				continue
			}
		}
		ps.backoff.Reset()
		return
	}
}
