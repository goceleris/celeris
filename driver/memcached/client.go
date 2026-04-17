package memcached

import (
	"context"
	"errors"
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
	provider, err := eventloop.Resolve(cfg.Engine)
	if err != nil {
		return nil, err
	}
	ownsLoop := cfg.Engine == nil
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
