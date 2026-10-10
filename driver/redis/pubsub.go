package redis

import (
	"context"
	"errors"
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

	// ctlMu keeps a control write and the release of its conn apart
	// (celeris#929). A conn is written by its file-descriptor number, and the
	// number goes back to the kernel the moment the conn is closed, so a write
	// that reaches loop.Write after that lands on whichever socket took the
	// number. Every control write (Subscribe, Unsubscribe, ...) and every
	// resubscribe of the reconnect loop holds ctlMu from before it reads
	// ps.conn until its write has returned. Whoever takes a conn out of
	// ps.conn (Close, the reconnect loop) releases it only after it has
	// passed through ctlMu with the conn already out, so no write that read
	// the conn is still in flight.
	//
	// Lock order: ctlMu, then mu. Never take ctlMu from deliver, onRecv,
	// onClose or the close hook (the worker goroutine runs those, and no
	// loop's Write waits for a worker, so a worker never waits on a writer
	// that holds ctlMu). Never hold mu across a conn release: UnregisterConn
	// can wait for an onRecv that is blocked in deliver on mu.
	ctlMu     sync.Mutex
	mu        sync.Mutex
	conn      *redisConn
	subs      map[string]struct{}
	psubs     map[string]struct{}
	shardSubs map[string]struct{}
	msgCh     chan *Message
	closeCh   chan struct{}
	closed    atomic.Bool

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

// Subscribe adds channels to the subscription set. The ctx argument is kept
// for API symmetry with [Client.Subscribe]; cancellation is not honored.
func (ps *PubSub) Subscribe(_ context.Context, channels ...string) error {
	return ps.control("SUBSCRIBE", channels, func() {
		for _, ch := range channels {
			ps.subs[ch] = struct{}{}
		}
	})
}

// Unsubscribe removes channels. Empty list unsubscribes all. The ctx argument
// is kept for API symmetry; cancellation is not honored.
func (ps *PubSub) Unsubscribe(_ context.Context, channels ...string) error {
	return ps.control("UNSUBSCRIBE", channels, func() {
		if len(channels) == 0 {
			ps.subs = map[string]struct{}{}
			return
		}
		for _, ch := range channels {
			delete(ps.subs, ch)
		}
	})
}

// PSubscribe adds patterns to the subscription set. The ctx argument is kept
// for API symmetry; cancellation is not honored.
func (ps *PubSub) PSubscribe(_ context.Context, patterns ...string) error {
	return ps.control("PSUBSCRIBE", patterns, func() {
		for _, p := range patterns {
			ps.psubs[p] = struct{}{}
		}
	})
}

// PUnsubscribe removes patterns. Empty list unsubscribes all patterns. The
// ctx argument is kept for API symmetry; cancellation is not honored.
func (ps *PubSub) PUnsubscribe(_ context.Context, patterns ...string) error {
	return ps.control("PUNSUBSCRIBE", patterns, func() {
		if len(patterns) == 0 {
			ps.psubs = map[string]struct{}{}
			return
		}
		for _, p := range patterns {
			delete(ps.psubs, p)
		}
	})
}

// SSubscribe adds shard channels (Redis 7+ SSUBSCRIBE) to the subscription
// set. Shard channels are scoped to the cluster slot of the channel name. The
// ctx argument is kept for API symmetry; cancellation is not honored.
func (ps *PubSub) SSubscribe(_ context.Context, channels ...string) error {
	return ps.control("SSUBSCRIBE", channels, func() {
		for _, ch := range channels {
			ps.shardSubs[ch] = struct{}{}
		}
	})
}

// SUnsubscribe removes shard channels. Empty list unsubscribes all shard
// channels. The ctx argument is kept for API symmetry; cancellation is not
// honored.
func (ps *PubSub) SUnsubscribe(_ context.Context, channels ...string) error {
	return ps.control("SUNSUBSCRIBE", channels, func() {
		if len(channels) == 0 {
			ps.shardSubs = map[string]struct{}{}
			return
		}
		for _, ch := range channels {
			delete(ps.shardSubs, ch)
		}
	})
}

// control applies update to the tracked sets under mu, then sends verb with
// names on the conn ps holds at that moment. The send runs under ctlMu, so
// the conn cannot be released while it is in flight: the write goes to the
// conn it was issued on or fails, and never reaches another socket that took
// the conn's number (celeris#929). When ps has no conn (it is closed, or the
// reconnect loop has not yet replaced a dropped one) the sets are still
// updated, so the reconnect replays them, and the call returns ErrClosed as
// it always did.
func (ps *PubSub) control(verb string, names []string, update func()) error {
	if ps.closed.Load() {
		return ErrClosed
	}
	ps.ctlMu.Lock()
	defer ps.ctlMu.Unlock()
	ps.mu.Lock()
	update()
	conn := ps.conn
	ps.mu.Unlock()
	err := sendPubSubControl(conn, append([]string{verb}, names...))
	if errors.Is(err, ErrClosed) && conn != nil && !ps.closed.Load() {
		// conn is down and ps is not: the reconnect loop replays the sets,
		// so tell the caller the write was not sent, but not that ps is
		// closed (ErrClosed is final, this is not).
		return errPubSubConnDown
	}
	return err
}

// errPubSubConnDown is returned by a control call that finds the pubsub conn
// dropped and the reconnect loop not yet done. The subscription set is
// updated and the reconnect replays it.
var errPubSubConnDown = errors.New("celeris-redis: pubsub connection is down; reconnecting, the subscription is replayed")

// sendPubSubControl writes a control command without tracking a reply (pubsub
// control frames are delivered as push). The caller holds ps.ctlMu and read
// conn from ps.conn under it.
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
		// Detach first: unregistering the conn fails a control write that
		// is still in flight on it (or unblocks one that waits on a full
		// send buffer), and the number is still the conn's own, so that
		// write cannot reach another socket. Then wait for every writer
		// that read conn before it was taken out (ctlMu), and release the
		// number under it, so none can start after the release either.
		ps.detach(conn)
		ps.ctlMu.Lock()
		ps.client.pool.releasePubSub(conn)
		ps.ctlMu.Unlock()
	}
	ps.closeMsgCh()
	return nil
}

// detach unhooks conn from ps and removes it from its worker's interest set.
// It does not release the conn's number; releasePubSub does, once.
func (ps *PubSub) detach(conn *redisConn) {
	conn.state.router.clear()
	conn.setPubSubCloseHook(nil)
	conn.unregister()
}

// takeConn clears ps.conn if it is still conn and reports whether the caller
// now owns conn. Whoever takes a conn out of ps.conn is the one that releases
// it, so every conn the pool handed out is released exactly once.
func (ps *PubSub) takeConn(conn *redisConn) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.conn != conn {
		return false
	}
	ps.conn = nil
	return true
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
		ps.ctlMu.Lock()
		ps.mu.Lock()
		if ps.closed.Load() {
			ps.mu.Unlock()
			ps.ctlMu.Unlock()
			// Never installed: nothing else holds conn.
			ps.client.pool.releasePubSub(conn)
			return
		}
		// Install conn under ctlMu and mu together, and resubscribe on it
		// before ctlMu is let go: a caller's control write then goes to
		// conn, after the replay, and Close cannot release conn under the
		// replay's writes.
		old := ps.conn
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
		ok := true
		for _, r := range [...]struct {
			verb  string
			names []string
		}{{"SUBSCRIBE", subs}, {"PSUBSCRIBE", psubs}, {"SSUBSCRIBE", ssubs}} {
			if len(r.names) == 0 {
				continue
			}
			if ps.closed.Load() {
				ok = false
				break
			}
			if err := sendPubSubControl(conn, append([]string{r.verb}, r.names...)); err != nil {
				ok = false
				break
			}
		}
		// conn may have died after bindConn but before its close hook was
		// set (the hook fires once, when the conn closes): look.
		if ok && conn.closed.Load() {
			ok = false
		}
		var failed *redisConn
		if !ok && ps.takeConn(conn) {
			failed = conn // else Close took it and releases it
		}
		ps.ctlMu.Unlock()
		// The dropped conn is out of ps.conn, no writer can hold it (writers
		// hold ctlMu and read ps.conn under it): release it, once.
		if old != nil {
			ps.detach(old)
			ps.client.pool.releasePubSub(old)
		}
		if failed != nil {
			ps.detach(failed)
			ps.client.pool.releasePubSub(failed)
		}
		if ok {
			ps.backoff.Reset()
			return
		}
	}
}
