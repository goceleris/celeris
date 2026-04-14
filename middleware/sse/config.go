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
