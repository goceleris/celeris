package memcached

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/internal/eventloop"
)

// Client is the high-level handle users interact with. It owns a pool of
// connections and hands out per-command round trips.
type Client struct {
	cfg  Config
	pool *memcachedPool
}

// NewClient dials a pool at addr using the given options. Connections are
// established lazily on first command.
//
// When opened without WithEngine(srv), the client uses direct [net.TCPConn]
// I/O on the caller's goroutine — no event-loop indirection. This matches
// [github.com/bradfitz/gomemcache] in shape (Go's netpoll parks the
// goroutine on EPOLLIN transparently) and closes the foreign-HTTP gap
// vs gomc on the MSR1 bench from ~17% to within noise.
//
// When opened WithEngine, the mini-loop sync path (WriteAndPollBusy) is
// used so DB I/O colocates with the celeris HTTP engine's worker.
//
// addr may optionally include a "memcache://" or "memcached://" scheme
// prefix; the prefix is stripped and the remaining "host:port" is used
// verbatim. TLS is not supported in v1.4.0.
func NewClient(addr string, opts ...Option) (*Client, error) {
	addr = strings.TrimPrefix(addr, "memcached://")
	addr = strings.TrimPrefix(addr, "memcache://")
	cfg := Config{Addr: addr}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.Addr == "" {
		return nil, errors.New("celeris/memcached: empty address")
	}
	// Experimental: always direct when CELERIS_MC_FORCE_DIRECT=1. Used
	// to validate async-handler-dispatch + direct-conn combination on
	// the celeris HTTP engine path. When the handler runs on a spawned
	// (unlocked) goroutine, direct net.Conn.Read uses Go's netpoll
	// which parks the G efficiently without blocking the M.
	if cfg.Engine == nil || os.Getenv("CELERIS_MC_FORCE_DIRECT") == "1" {
		pool := newDirectPool(cfg)
		return &Client{cfg: cfg, pool: pool}, nil
	}
	provider, err := eventloop.Resolve(cfg.Engine)
	if err != nil {
		return nil, err
	}
	ownsLoop := false
	pool := newPool(cfg, provider, ownsLoop)
	return &Client{cfg: cfg, pool: pool}, nil
}

// Close tears down every pooled connection. Safe to call on a nil Client.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	return c.pool.Close()
}

// PoolStats returns driver pool occupancy. Named PoolStats rather than
// Stats so it doesn't collide with the server-side [Client.Stats] command
// that queries the memcached server's own statistics map.
func (c *Client) PoolStats() async.PoolStats {
	return c.pool.Stats()
}

// IdleConnWorkers returns the Worker() IDs of every currently-idle
// connection. Intended for tests and introspection asserting that per-CPU
// affinity is actually honored by the dial path.
func (c *Client) IdleConnWorkers() []int {
	return c.pool.pool.IdleConnWorkers()
}

// Protocol returns the wire dialect negotiated at dial time.
func (c *Client) Protocol() Protocol { return c.cfg.Protocol }

// ---------- worker hint in context ----------

type workerCtxKey struct{}

// WithWorker returns ctx with a preferred worker hint.
func WithWorker(ctx context.Context, workerID int) context.Context {
	return context.WithValue(ctx, workerCtxKey{}, workerID)
}

// workerFromCtx returns -1 if no hint is set.
func workerFromCtx(ctx context.Context) int {
	v := ctx.Value(workerCtxKey{})
	if v == nil {
		return -1
	}
	if n, ok := v.(int); ok {
		return n
	}
	return -1
}
