package redis

import (
	"context"
	"errors"
	"strings"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/redis/protocol"
)

// Client is the high-level handle users interact with. It owns a pool of
// connections and hands out per-command round trips.
type Client struct {
	cfg  Config
	pool *redisPool
}

// NewClient dials a pool at addr using the given options. Connections are
// established lazily on first command.
func NewClient(addr string, opts ...Option) (*Client, error) {
	if strings.HasPrefix(addr, "rediss://") {
		return nil, errors.New("celeris-redis: TLS (rediss://) is not supported in v1.4.0; use redis:// for VPC/loopback deployments or wait for v1.4.x TLS support")
	}
	// Strip redis:// prefix if present.
	addr = strings.TrimPrefix(addr, "redis://")
	cfg := Config{Addr: addr}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.Addr == "" {
		return nil, errors.New("celeris-redis: empty address")
	}
	// Direct-cmd-path ONLY when the engine dispatches handlers async
	// (caller runs on an unlocked G → net.Conn.Read uses Go's netpoll
	// to park the G without blocking an M).
	//
	// For standalone (no engine), the mini-loop's sync spin is measurably
	// faster than direct net.Conn.Read for redis's tiny GET responses (-2%
	// flipped to +2%). For WithEngine without async, direct on a locked
	// worker M would futex-storm.
	//
	// Pub/sub always uses the mini-loop because unsolicited server-push
	// frames need event-driven delivery.
	if eventloop.IsAsyncServer(cfg.Engine) {
		provider, err := eventloop.Resolve(nil)
		if err != nil {
			return nil, err
		}
		pool := newDirectCmdPool(cfg, provider)
		return &Client{cfg: cfg, pool: pool}, nil
	}
	provider, err := eventloop.Resolve(cfg.Engine)
	if err != nil {
		return nil, err
	}
	// WithEngine on an engine whose WorkerLoop doesn't implement
	// syncRoundTripper (io_uring, epoll today) would deadlock the HELLO
	// handshake: the handler goroutine is LockOSThread'd to the engine
	// worker, so wait() parks the M and the CQE carrying the HELLO reply
	// never drains. Fall back to direct mode (net.Conn via Go netpoll),
	// which parks locked-M Gs cleanly.
	if cfg.Engine != nil {
		if wl := provider.WorkerLoop(0); wl != nil {
			if _, ok := wl.(syncRoundTripper); !ok {
				pool := newDirectCmdPool(cfg, provider)
				return &Client{cfg: cfg, pool: pool}, nil
			}
		}
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

// Proto returns the negotiated RESP protocol version on the last dialed
// connection. Returns 0 if no conn has been dialed yet (call Ping first).
func (c *Client) Proto() int {
	// Best-effort: inspect the cmd pool for one open conn. Without a stats
	// hook returning per-conn proto, return the configured target — close
	// enough for introspection.
	if c.cfg.ForceRESP2 {
		return 2
	}
	if c.cfg.Proto == 2 {
		return 2
	}
	return 3
}

// Ping issues PING on a pooled conn.
func (c *Client) Ping(ctx context.Context) error {
	conn, err := c.pool.acquireCmd(ctx, workerFromCtx(ctx))
	if err != nil {
		return err
	}
	if perr := conn.Ping(ctx); perr != nil {
		if errors.Is(perr, context.Canceled) || errors.Is(perr, context.DeadlineExceeded) {
			c.pool.discardCmd(conn)
			return perr
		}
		c.pool.releaseCmd(conn)
		return perr
	}
	c.pool.releaseCmd(conn)
	return nil
}

// Stats returns cmd-pool occupancy.
func (c *Client) Stats() async.PoolStats {
	return c.pool.Stats()
}

// IdleConnWorkers returns the Worker() IDs of every currently-idle command
// connection. Intended for tests and introspection asserting that per-CPU
// affinity is actually honored by the dial path.
func (c *Client) IdleConnWorkers() []int {
	return c.pool.cmd.IdleConnWorkers()
}

// Do is an escape hatch for commands the typed API does not cover
// (OBJECT ENCODING, CLUSTER INFO, SCRIPT LOAD, XADD, FUNCTION, ...). Args
// are converted to strings via [argify]; the returned [protocol.Value] is
// detached from the Reader buffer and safe to retain. Server error replies
// surface as [*RedisError] via the returned error.
func (c *Client) Do(ctx context.Context, args ...any) (*protocol.Value, error) {
	if len(args) == 0 {
		return nil, errors.New("celeris-redis: Do requires at least one argument")
	}
	strs := make([]string, len(args))
	for i, a := range args {
		strs[i] = argify(a)
	}
	conn, err := c.pool.acquireCmd(ctx, workerFromCtx(ctx))
	if err != nil {
		return nil, err
	}
	// Track MULTI/EXEC/DISCARD so pool release can DISCARD-if-needed.
	switch strings.ToUpper(strs[0]) {
	case "MULTI":
		conn.dirty.Store(true)
	case "EXEC", "DISCARD":
		conn.dirty.Store(false)
	}
	req, err := conn.exec(ctx, strs...)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			c.pool.discardCmd(conn)
		} else {
			c.pool.releaseCmd(conn)
		}
		return nil, err
	}
	if req.resultErr != nil {
		c.pool.releaseCmd(conn)
		return nil, req.resultErr
	}
	// req.result is already detached inside dispatch. Release the request's
	// (now-cleared) pooled backing if any and hand a copy to the caller.
	out := req.result
	conn.releaseResult(req)
	c.pool.releaseCmd(conn)
	return &out, nil
}

// DoString is a convenience wrapper around [Client.Do] that decodes the reply
// as a string.
func (c *Client) DoString(ctx context.Context, args ...any) (string, error) {
	v, err := c.Do(ctx, args...)
	if err != nil {
		return "", err
	}
	return asString(*v)
}

// DoInt is a convenience wrapper around [Client.Do] that decodes the reply
// as an int64.
func (c *Client) DoInt(ctx context.Context, args ...any) (int64, error) {
	v, err := c.Do(ctx, args...)
	if err != nil {
		return 0, err
	}
	return asInt(*v)
}

// DoBool is a convenience wrapper around [Client.Do] that decodes the reply
// as a bool.
func (c *Client) DoBool(ctx context.Context, args ...any) (bool, error) {
	v, err := c.Do(ctx, args...)
	if err != nil {
		return false, err
	}
	return asBool(*v)
}

// DoSlice is a convenience wrapper around [Client.Do] that decodes the reply
// as a string slice.
func (c *Client) DoSlice(ctx context.Context, args ...any) ([]string, error) {
	v, err := c.Do(ctx, args...)
	if err != nil {
		return nil, err
	}
	return asStringSlice(*v)
}

// OnPush registers (or replaces) the push callback for RESP3 push frames
// arriving on command connections. It is safe to call concurrently with
// in-flight commands; delivery is best-effort. Pass nil to clear.
func (c *Client) OnPush(fn func(channel string, data []protocol.Value)) {
	c.cfg.OnPush = fn
	c.pool.setOnPush(fn)
}

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
