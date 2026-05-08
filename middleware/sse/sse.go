package sse

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris"
)

// Handler is the function signature for SSE endpoint handlers.
// The Client is valid for the duration of the call. When Handler returns,
// the SSE stream is closed automatically.
type Handler func(client *Client)

// Client provides the API for sending SSE events to a connected client.
// It wraps a [celeris.StreamWriter] and manages event formatting, heartbeat,
// and disconnect detection.
type Client struct {
	sw          *celeris.StreamWriter
	ctx         context.Context
	cancel      context.CancelFunc
	lastEventID string
	buf         []byte
	mu          sync.Mutex
	closed      bool

	// Queued-send mode (Config.MaxQueueDepth > 0). queue is nil in legacy
	// blocking mode; a non-nil queue selects the asynchronous drain path.
	// queueClosed flips to true the instant the handler defer is about to
	// close c.queue, so a Send racing the defer observes "closed" and
	// returns ErrClientClosed instead of panicking on send-to-closed-chan.
	queue         chan Event
	queueClosed   atomic.Bool
	drainDone     chan struct{}
	onSlowClient  func(*Client, Event) ClientPolicy
	droppedEvents atomic.Uint64

	// replayStore, when non-nil, captures every Event Send writes so the
	// next reconnect can resume via Last-Event-ID. Set per-handler from
	// [Config.ReplayStore] in [New].
	replayStore ReplayStore
}

// Send sends a complete SSE event. Thread-safe (serialized with heartbeat writes).
//
// Return contract:
//   - non-nil error: the event was NOT delivered (closed, context cancelled,
//     or write failed); the caller may retry on a new connection.
//   - nil error in blocking mode (MaxQueueDepth = 0): the event was written
//     to the underlying stream and flushed.
//   - nil error in queued mode (MaxQueueDepth > 0): the event was enqueued
//     OR dropped silently per the [Config.OnSlowClient] policy; check
//     [Client.DroppedEvents] to detect drops.
//
// When [Config.MaxQueueDepth] is zero (default), Send writes directly to the
// wire — historical blocking semantics. When MaxQueueDepth is positive Send
// enqueues onto a bounded channel that a per-client goroutine drains; if the
// queue is full Send dispatches the configured [Config.OnSlowClient] policy.
//
// Send must NOT be called after the user's [Handler] has returned. The
// handler defer closes the per-client queue; a Send racing that close
// observes the queueClosed flag and returns [ErrClientClosed] instead of
// panicking, but the contract still belongs to the caller — spawned
// goroutines that publish into a Client must be joined before the handler
// returns.
func (c *Client) Send(e Event) error {
	if c.queue != nil {
		return c.sendQueued(e)
	}
	return c.sendBlocking(e)
}

func (c *Client) sendBlocking(e Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return ErrClientClosed
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}

	// Append under the same lock that serialises the wire write so the
	// store's monotonic ID order matches the on-wire order even with
	// concurrent Send callers. Failed writes cancel the connection;
	// the appended event is still in the store and will replay on the
	// next reconnect — strictly correct (every appended event is
	// either delivered now OR delivered on reconnect).
	if c.replayStore != nil {
		id, err := c.replayStore.Append(c.ctx, e)
		if err != nil {
			return err
		}
		e.ID = id
	}

	c.buf = formatEvent(c.buf, &e)
	if _, err := c.sw.Write(c.buf); err != nil {
		c.cancel()
		return err
	}
	if err := c.sw.Flush(); err != nil {
		c.cancel()
		return err
	}
	return nil
}

func (c *Client) sendQueued(e Event) error {
	if c.queueClosed.Load() {
		return ErrClientClosed
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}
	select {
	case c.queue <- e:
		return nil
	default:
	}

	action := ClientPolicyDrop
	if c.onSlowClient != nil {
		action = c.onSlowClient(c, e)
	}
	switch action {
	case ClientPolicyClose:
		c.droppedEvents.Add(1)
		c.cancel()
		return ErrClientClosed
	case ClientPolicyBlock:
		// Re-check queueClosed inside the select so a defer that fires
		// between the top-of-function check and here doesn't park the
		// caller on a closed-chan send forever.
		if c.queueClosed.Load() {
			return ErrClientClosed
		}
		select {
		case c.queue <- e:
			return nil
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
	default: // ClientPolicyDrop and any unknown value
		c.droppedEvents.Add(1)
		return nil
	}
}

// drain pulls events off the per-client queue and writes them to the wire.
// Started by [New] when Config.MaxQueueDepth > 0. The range exits when the
// queue is closed by the handler defer (graceful close, remaining events
// flushed) or when the closed flag flips after [Client.Close] (unhealthy
// close, remaining events dropped — the connection is already gone).
func (c *Client) drain() {
	defer close(c.drainDone)
	for e := range c.queue {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		// Append under the wire-write lock so store order matches the
		// on-wire order even when Send + drain race. A failing Append
		// proceeds with the wire-write using the user-supplied e.ID —
		// a transient store hiccup must not kill an otherwise healthy
		// connection. The next successful Send recovers the order.
		if c.replayStore != nil {
			if id, err := c.replayStore.Append(c.ctx, e); err == nil {
				e.ID = id
			}
		}
		c.buf = formatEvent(c.buf, &e)
		_, err := c.sw.Write(c.buf)
		if err == nil {
			err = c.sw.Flush()
		}
		c.mu.Unlock()
		if err != nil {
			c.cancel()
			return
		}
	}
}

// DroppedEvents returns the cumulative count of events dropped under
// [ClientPolicyDrop] (or [ClientPolicyClose]) since this client connected.
// Always zero when [Config.MaxQueueDepth] is zero.
func (c *Client) DroppedEvents() uint64 {
	return c.droppedEvents.Load()
}

// QueueDepth returns the current outbound-queue length, or zero when the
// client is in legacy blocking mode.
func (c *Client) QueueDepth() int {
	if c.queue == nil {
		return 0
	}
	return len(c.queue)
}

// SendData is a convenience for sending a data-only event.
// Equivalent to Send(Event{Data: data}).
func (c *Client) SendData(data string) error {
	return c.Send(Event{Data: data})
}

// SendComment sends a comment line. Useful for custom keep-alives.
func (c *Client) SendComment(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return ErrClientClosed
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}

	c.buf = formatComment(c.buf, text)
	if _, err := c.sw.Write(c.buf); err != nil {
		c.cancel()
		return err
	}
	if err := c.sw.Flush(); err != nil {
		c.cancel()
		return err
	}
	return nil
}

// Close closes the SSE stream. Safe to call multiple times.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.cancel()
	return c.sw.Close()
}

// Context returns a context.Context that is cancelled when the client
// disconnects or the stream is closed.
func (c *Client) Context() context.Context {
	return c.ctx
}

// LastEventID returns the value of the Last-Event-ID header sent by the
// client on reconnection. Returns empty string for initial connections.
func (c *Client) LastEventID() string {
	return c.lastEventID
}

const maxPooledBufSize = 4096

var clientPool = sync.Pool{New: func() any {
	return &Client{buf: make([]byte, 0, 256)}
}}

func acquireClient(ctx context.Context, sw *celeris.StreamWriter, cancel context.CancelFunc, lastEventID string) *Client {
	c := clientPool.Get().(*Client)
	c.sw = sw
	c.ctx = ctx
	c.cancel = cancel
	c.lastEventID = lastEventID
	c.buf = c.buf[:0]
	c.closed = false
	return c
}

func releaseClient(c *Client) {
	c.sw = nil
	c.ctx = nil
	c.cancel = nil
	c.lastEventID = ""
	c.queue = nil
	c.queueClosed.Store(false)
	c.drainDone = nil
	c.onSlowClient = nil
	c.droppedEvents.Store(0)
	c.replayStore = nil
	if cap(c.buf) <= maxPooledBufSize {
		clientPool.Put(c)
	}
}

// New creates an SSE handler with the given config.
//
// Usage:
//
//	server.GET("/events", sse.New(sse.Config{
//	    Handler: func(client *sse.Client) {
//	        for msg := range messages {
//	            if err := client.Send(sse.Event{Data: msg}); err != nil {
//	                return
//	            }
//	        }
//	    },
//	}))
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	handler := cfg.Handler
	heartbeatInterval := cfg.HeartbeatInterval
	onConnect := cfg.OnConnect
	onDisconnect := cfg.OnDisconnect

	// Pre-format retry line if configured.
	var retryLine []byte
	if cfg.RetryInterval > 0 {
		retryLine = []byte("retry: " + strconv.Itoa(cfg.RetryInterval) + "\n\n")
	}

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	// SSE response headers. Shared across handlers; safe because both
	// the H1 and H2 StreamWriter.WriteHeader implementations copy values
	// out of this slice synchronously and do not retain a reference.
	// See TestSSEStreamWriterDoesNotRetainHeaders in sse_test.go for the
	// pinned regression check.
	sseHeaders := [][2]string{
		{"content-type", "text/event-stream"},
		{"cache-control", "no-cache"},
		{"x-accel-buffering", "no"},
	}

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		// Extract Last-Event-ID before Detach materializes headers.
		lastEventID := c.Header("last-event-id")

		sw := c.StreamWriter()
		if sw == nil {
			return celeris.NewHTTPError(500, "SSE requires streaming support")
		}

		// Run OnConnect before writing headers so rejection can return
		// a proper HTTP error code to the client.
		ctx, cancel := context.WithCancel(c.Context())
		client := acquireClient(ctx, sw, cancel, lastEventID)

		if onConnect != nil {
			if err := onConnect(c, client); err != nil {
				cancel()
				releaseClient(client)
				return err
			}
		}

		done := c.Detach()

		if err := sw.WriteHeader(200, sseHeaders); err != nil {
			cancel()
			releaseClient(client)
			done()
			return err
		}

		// heartbeatDone is closed by the heartbeat goroutine on exit so the
		// defer can wait for it before returning the Client to the pool.
		// Without this, the goroutine could read client.closed after a
		// future test reused the pooled Client (data race).
		var heartbeatDone chan struct{}

		defer func() {
			// Tear-down ordering — load-bearing:
			//
			//   1. queueClosed.Store(true) — fast-path Send check.
			//   2. cancel() — wakes any Send parked in
			//      ClientPolicyBlock's select on c.ctx.Done(). MUST
			//      precede close(c.queue): a producer that observed
			//      queueClosed==false in the fast path can be in the
			//      select at the moment we close the queue, and would
			//      panic on send-to-closed-chan if ctx was not
			//      already done. With cancel ahead of close, the
			//      ctx.Done() arm of the select wins instead.
			//   3. close(c.queue) — drain goroutine ranges to
			//      completion, flushing in-queue events.
			//   4. <-c.drainDone — join the drain.
			//   5. client.Close() — idempotent; cancel already fired,
			//      so this is just the sw.Close + closed=true bookkeeping.
			if client.queue != nil {
				client.queueClosed.Store(true)
				cancel()
				close(client.queue)
			}
			if client.drainDone != nil {
				<-client.drainDone
			}
			_ = client.Close()
			if heartbeatDone != nil {
				<-heartbeatDone
			}
			if onDisconnect != nil {
				onDisconnect(c, client)
			}
			releaseClient(client)
			done()
		}()

		// Flush the SSE response headers immediately so EventSource
		// clients can begin reading. Without this, the std engine
		// buffers headers until the first body byte — fine for
		// handlers that Send right away, but a deadlock for broker
		// patterns that Subscribe and wait for the next Publish.
		// The retry line piggybacks on the same flush when set.
		client.mu.Lock()
		var err error
		if len(retryLine) > 0 {
			_, err = sw.Write(retryLine)
		}
		if err == nil {
			err = sw.Flush()
		}
		client.mu.Unlock()
		if err != nil {
			return nil
		}

		// Bind the replay store to the client BEFORE the drain goroutine
		// starts so an event enqueued the instant the handler runs hits
		// Append on its way to the wire.
		client.replayStore = cfg.ReplayStore

		// Replay missed events from the previous session, if any.
		// ErrLastIDUnknown is the documented "fresh start" signal:
		// silently fall through and let Handler decide; the user's
		// Handler can still consult client.LastEventID() to react to
		// the unknown cursor itself.
		//
		// Holding client.mu across the entire replay loop is
		// load-bearing for Last-Event-ID monotonicity: a concurrent
		// Send (e.g. via a Broker.Publish that fires before the
		// handler reaches its Subscribe) would otherwise interleave
		// a newer event mid-replay; if the client disconnects mid-
		// replay it would record the newer event's ID and skip the
		// still-pending replay tail on next reconnect.
		if cfg.ReplayStore != nil && lastEventID != "" {
			missed, err := cfg.ReplayStore.Since(ctx, lastEventID)
			if err != nil && !errors.Is(err, ErrLastIDUnknown) {
				return nil
			}
			client.mu.Lock()
			for i := range missed {
				client.buf = formatEvent(client.buf, &missed[i])
				_, werr := sw.Write(client.buf)
				if werr == nil {
					werr = sw.Flush()
				}
				if werr != nil {
					client.mu.Unlock()
					return nil
				}
			}
			client.mu.Unlock()
		}

		// Start per-client drain goroutine when queued-send mode is active.
		// Stays in lockstep with the handler lifecycle: started after the
		// retry line so the channel cannot leak events written before the
		// client even saw the response, joined in the defer above.
		if cfg.MaxQueueDepth > 0 {
			client.queue = make(chan Event, cfg.MaxQueueDepth)
			client.drainDone = make(chan struct{})
			client.onSlowClient = cfg.OnSlowClient
			go client.drain()
		}

		// Start heartbeat goroutine.
		if heartbeatInterval > 0 {
			heartbeatDone = make(chan struct{})
			go func() {
				defer close(heartbeatDone)
				ticker := time.NewTicker(heartbeatInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						client.mu.Lock()
						if client.closed {
							client.mu.Unlock()
							return
						}
						_, err := sw.Write(heartbeatBytes)
						if err == nil {
							err = sw.Flush()
						}
						client.mu.Unlock()
						if err != nil {
							cancel()
							return
						}
					}
				}
			}()
		}

		handler(client)
		return nil
	}
}
