package memcached

import (
	"sync/atomic"
	"time"
)

// testHookDefaultOpTimeout, when set, replaces the default per-operation timeout
// (defaultTimeout, 3 s) of a client whose Config.Timeout is unset. A test sets it
// to avoid waiting 3 s, and resets it to nil when it is done.
var testHookDefaultOpTimeout atomic.Pointer[time.Duration]

// Protocol selects the memcached wire dialect spoken by a Client. Every
// memcached deployment supports text; binary was deprecated upstream but
// remains broadly available and is sometimes preferred for its CAS semantics
// and fixed-size packet framing. Text is the default.
type Protocol uint8

const (
	// ProtocolText is the line-oriented ASCII dialect (default).
	ProtocolText Protocol = iota
	// ProtocolBinary is the 24-byte-header binary dialect.
	ProtocolBinary
)

// String returns a short name for diagnostics.
func (p Protocol) String() string {
	switch p {
	case ProtocolText:
		return "text"
	case ProtocolBinary:
		return "binary"
	}
	return "unknown"
}

const (
	defaultDialTimeout      = 5 * time.Second
	defaultTimeout          = 3 * time.Second
	defaultMaxIdlePerWorker = 2
	defaultMaxLifetime      = 30 * time.Minute
	defaultMaxIdleTime      = 5 * time.Minute
	defaultHealthCheck      = 30 * time.Second
)

// Config controls a [Client]. Use [Option] functions to set fields; the zero
// value is not directly usable.
type Config struct {
	// Addr is the TCP endpoint in "host:port" form.
	Addr string
	// Protocol selects text vs binary at dial time. Default is text.
	Protocol Protocol
	// MaxOpen is the total connection cap across all workers. Zero means
	// NumWorkers * 4.
	MaxOpen int
	// MaxIdlePerWorker bounds each worker's idle list; default 2.
	MaxIdlePerWorker int
	// DialTimeout bounds the TCP dial and, separately, the binary-protocol
	// handshake (a VERSION round trip) of a new conn; default 5s. A server that
	// accepts the connection and does not answer the handshake within DialTimeout
	// fails the dial with an error that matches [context.DeadlineExceeded], also
	// when the caller's context has a longer deadline or none; a slow connect
	// followed by a silent server takes up to about twice DialTimeout. A shorter
	// deadline of the caller's context wins. The text protocol has no handshake.
	DialTimeout time.Duration
	// Timeout is the advisory per-op deadline used when the caller's context
	// carries no deadline. Default 3s.
	Timeout time.Duration
	// MaxLifetime is the max age of a pooled conn; default 30m.
	MaxLifetime time.Duration
	// MaxIdleTime is the max idle duration before eviction; default 5m.
	MaxIdleTime time.Duration
	// HealthCheckInterval is the background health-check sweep interval.
	// Default 30s; a negative value disables the sweep.
	HealthCheckInterval time.Duration
	// Engine hooks the driver into a running celeris.Server's event loop.
	// If nil, a standalone loop is resolved on [NewClient].
	Engine ServerProvider
}

// Option mutates a [Config] during [NewClient].
type Option func(*Config)

// WithProtocol selects the wire dialect. Default is text.
func WithProtocol(p Protocol) Option { return func(c *Config) { c.Protocol = p } }

// WithMaxOpen sets the total connection cap.
func WithMaxOpen(n int) Option { return func(c *Config) { c.MaxOpen = n } }

// WithMaxIdlePerWorker bounds each worker's idle list.
func WithMaxIdlePerWorker(n int) Option { return func(c *Config) { c.MaxIdlePerWorker = n } }

// WithDialTimeout sets [Config.DialTimeout]: the bound of the TCP dial and of the
// binary-protocol handshake.
func WithDialTimeout(d time.Duration) Option { return func(c *Config) { c.DialTimeout = d } }

// WithTimeout sets the default per-op deadline when the caller's ctx has none.
func WithTimeout(d time.Duration) Option { return func(c *Config) { c.Timeout = d } }

// WithMaxLifetime sets the pooled conn lifetime.
func WithMaxLifetime(d time.Duration) Option { return func(c *Config) { c.MaxLifetime = d } }

// WithMaxIdleTime sets the pooled conn idle eviction.
func WithMaxIdleTime(d time.Duration) Option { return func(c *Config) { c.MaxIdleTime = d } }

// WithHealthCheckInterval sets the background health-check sweep interval.
// Default 30s; pass a negative value to disable.
func WithHealthCheckInterval(d time.Duration) Option {
	return func(c *Config) { c.HealthCheckInterval = d }
}

// WithEngine hooks the driver into a celeris.Server's event loop.
func WithEngine(sp ServerProvider) Option {
	return func(c *Config) { c.Engine = sp }
}
