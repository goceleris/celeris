package redis

import (
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/redis/protocol"
)

const (
	defaultDialTimeout      = 5 * time.Second
	defaultReadTimeout      = 3 * time.Second
	defaultWriteTimeout     = 3 * time.Second
	defaultMaxIdlePerWorker = 2
	defaultMaxLifetime      = 30 * time.Minute
	defaultMaxIdleTime      = 5 * time.Minute
	defaultPubSubChanBuf    = 256
)

// Config controls a Client. Use [Option] functions to set fields; the zero
// value is not directly usable.
type Config struct {
	// Addr is the TCP endpoint in "host:port" form.
	Addr string
	// Password is the AUTH credential; empty for no auth.
	Password string
	// Username is the ACL user (Redis 6+); empty for legacy auth.
	Username string
	// DB is the numeric database index applied via SELECT post-HELLO.
	DB int
	// DialTimeout is the TCP dial timeout; default 5s.
	DialTimeout time.Duration
	// ReadTimeout is unused currently — responses are driven by the event
	// loop and cancellation is via the request context.
	ReadTimeout time.Duration
	// WriteTimeout is unused currently — writes are non-blocking via the
	// event loop's per-FD queue.
	WriteTimeout time.Duration
	// PoolSize is the total connection cap across all workers; defaults to
	// NumWorkers*4.
	PoolSize int
	// MaxIdlePerWorker bounds each worker's idle list; default 2.
	MaxIdlePerWorker int
	// MaxLifetime is the max age of a pooled conn; default 30m.
	MaxLifetime time.Duration
	// MaxIdleTime is the max idle duration before eviction; default 5m.
	MaxIdleTime time.Duration
	// HealthCheckInterval is the interval at which the pool's background
	// health checker pings idle connections. Default 30s; 0 disables.
	HealthCheckInterval time.Duration
	// Proto selects the negotiation target (2 or 3); default 3 with
	// RESP2 fallback.
	Proto int
	// ForceRESP2 disables the HELLO handshake and speaks RESP2 plus AUTH+
	// SELECT. Useful for ElastiCache compatibility.
	ForceRESP2 bool
	// Engine hooks the driver into a running celeris.Server's event loop.
	// If nil, a standalone loop is resolved on NewClient.
	Engine eventloop.ServerProvider
	// OnPush is invoked when a RESP3 push frame arrives on a command
	// connection. The channel is the first element of the push array
	// (e.g. "invalidate" for client-tracking invalidations) and data
	// carries the remaining elements. If nil, push frames on command
	// connections are silently dropped.
	OnPush func(channel string, data []protocol.Value)
}

// Option mutates a Config during NewClient.
type Option func(*Config)

// WithPassword sets the AUTH password.
func WithPassword(s string) Option { return func(c *Config) { c.Password = s } }

// WithUsername sets the AUTH username (Redis 6 ACL).
func WithUsername(s string) Option { return func(c *Config) { c.Username = s } }

// WithDB selects a database index.
func WithDB(db int) Option { return func(c *Config) { c.DB = db } }

// WithDialTimeout sets the TCP dial timeout.
func WithDialTimeout(d time.Duration) Option { return func(c *Config) { c.DialTimeout = d } }

// WithReadTimeout sets the read timeout (currently advisory).
func WithReadTimeout(d time.Duration) Option { return func(c *Config) { c.ReadTimeout = d } }

// WithWriteTimeout sets the write timeout (currently advisory).
func WithWriteTimeout(d time.Duration) Option { return func(c *Config) { c.WriteTimeout = d } }

// WithPoolSize sets the total connection cap.
func WithPoolSize(n int) Option { return func(c *Config) { c.PoolSize = n } }

// WithMaxIdlePerWorker bounds each worker's idle list.
func WithMaxIdlePerWorker(n int) Option { return func(c *Config) { c.MaxIdlePerWorker = n } }

// WithMaxLifetime sets the pooled conn lifetime.
func WithMaxLifetime(d time.Duration) Option { return func(c *Config) { c.MaxLifetime = d } }

// WithMaxIdleTime sets the pooled conn idle eviction.
func WithMaxIdleTime(d time.Duration) Option { return func(c *Config) { c.MaxIdleTime = d } }

// WithHealthCheckInterval sets the background health-check sweep interval.
// Default is 30s; pass 0 to disable.
func WithHealthCheckInterval(d time.Duration) Option {
	return func(c *Config) { c.HealthCheckInterval = d }
}

// WithEngine hooks the driver into a celeris.Server's event loop.
func WithEngine(sp eventloop.ServerProvider) Option {
	return func(c *Config) { c.Engine = sp }
}

// WithForceRESP2 forces the RESP2 dialect; skips HELLO.
func WithForceRESP2() Option { return func(c *Config) { c.ForceRESP2 = true } }

// WithProto sets the preferred protocol (2 or 3). Default is 3 with fallback.
func WithProto(p int) Option { return func(c *Config) { c.Proto = p } }

// WithOnPush registers a callback for RESP3 push frames that arrive on
// command connections (e.g. client-tracking invalidation messages). The
// channel is the push kind (first array element) and data carries the
// remaining elements. If unset, push frames on command connections are
// silently dropped.
func WithOnPush(fn func(channel string, data []protocol.Value)) Option {
	return func(c *Config) { c.OnPush = fn }
}
