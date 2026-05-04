package sse

import (
	"time"

	"github.com/goceleris/celeris"
)

const (
	// DefaultHeartbeatInterval is the default interval between heartbeat
	// comments sent to detect client disconnects.
	DefaultHeartbeatInterval = 15 * time.Second
)

// ClientPolicy selects what the middleware does when a per-client
// outbound queue is full at Send time. Used together with
// [Config.MaxQueueDepth] and [Config.OnSlowClient]. Naming mirrors
// [BrokerPolicy] and websocket.HubPolicy for ecosystem consistency.
type ClientPolicy uint8

const (
	// ClientPolicyDrop silently discards the Event and increments
	// [Client.DroppedEvents]. Send returns nil.
	ClientPolicyDrop ClientPolicy = iota

	// ClientPolicyDisconnect cancels the client's context, causing the
	// handler goroutine to exit. Send returns [ErrClientClosed].
	ClientPolicyDisconnect

	// ClientPolicyBlock falls back to the legacy blocking semantics:
	// Send waits for queue space until the context is cancelled.
	// Provided so opt-in users who want backpressure on a single
	// subscriber without disabling the queue infrastructure for the
	// rest can do so.
	ClientPolicyBlock
)

// Config defines the SSE middleware configuration.
type Config struct {
	// Handler is the SSE handler function called for each connected client.
	// Required; panics at init if nil.
	Handler Handler

	// HeartbeatInterval is the interval between heartbeat comments sent to
	// detect client disconnects. Set to a negative value to disable.
	// Default: 15s.
	HeartbeatInterval time.Duration

	// RetryInterval is the reconnection time (in milliseconds) sent to the
	// client in the initial "retry:" field. Zero means no retry field is sent
	// (client uses its default, typically ~3s).
	RetryInterval int

	// MaxQueueDepth bounds the per-client outbound queue. Zero means
	// unbounded — the legacy blocking Send semantics are preserved exactly.
	// When set, Send enqueues and returns immediately; a per-client drain
	// goroutine writes to the wire. If the queue is full at Send time,
	// OnSlowClient is invoked; if OnSlowClient is nil, [ClientPolicyDrop]
	// is the default.
	MaxQueueDepth int

	// OnSlowClient decides what to do when [MaxQueueDepth] is exceeded.
	// Only consulted when MaxQueueDepth > 0. The hook may inspect c (via
	// DroppedEvents/QueueDepth) and the dropped Event to drive
	// observability or escalating policies. Default: [ClientPolicyDrop].
	OnSlowClient func(c *Client, e Event) ClientPolicy

	// ReplayStore persists events for Last-Event-ID resume. When nil
	// (default), Client.LastEventID() returns the header but replay is
	// the user's problem — matches today's behavior. When set, the
	// middleware:
	//   - on connect with a Last-Event-ID, reads Since(lastID) and
	//     writes the missed events to the wire BEFORE invoking Handler;
	//   - wraps Send so each call also Appends to the store, rewriting
	//     the wire id: field with the canonical store-assigned ID;
	//   - if Since returns ErrLastIDUnknown, logs at debug and still
	//     invokes Handler (the user's resumption logic decides what to
	//     do with a fresh start).
	ReplayStore ReplayStore

	// OnConnect is called when a new SSE client connects, before Handler.
	// The celeris.Context is available for extracting request metadata.
	// Return a non-nil error to reject the connection.
	OnConnect func(c *celeris.Context, client *Client) error

	// OnDisconnect is called after the SSE stream closes.
	OnDisconnect func(c *celeris.Context, client *Client)

	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip from SSE handling (exact match).
	SkipPaths []string
}

var defaultConfig = Config{
	HeartbeatInterval: DefaultHeartbeatInterval,
}

func applyDefaults(cfg Config) Config {
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	return cfg
}

func (cfg Config) validate() {
	if cfg.Handler == nil {
		panic("sse: Handler must not be nil")
	}
}
