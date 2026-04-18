package memcached

import (
	"context"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/engine"
)

// memcachedPool wraps a single async.Pool of *memcachedConn. Memcached does
// not need a separate pub/sub pool (unlike Redis) — every command is
// request/response on a normal cmd conn.
type memcachedPool struct {
	pool     *async.Pool[*memcachedConn]
	cfg      Config
	provider engine.EventLoopProvider
	ownsLoop bool
}

func newPool(cfg Config, provider engine.EventLoopProvider, ownsLoop bool) *memcachedPool {
	nw := provider.NumWorkers()
	if nw <= 0 {
		nw = 1
	}
	maxOpen := cfg.MaxOpen
	if maxOpen == 0 {
		maxOpen = nw * 4
	}
	idle := cfg.MaxIdlePerWorker
	if idle == 0 {
		// Size the per-worker idle list so every released conn fits
		// without eviction. A tight default caused constant
		// reconnect churn under 256-way HTTP concurrency; keeping
		// the full working set idle avoids the dial on every
		// request cycle. Bounded below by defaultMaxIdlePerWorker.
		idle = (maxOpen + nw - 1) / nw
		if idle < defaultMaxIdlePerWorker {
			idle = defaultMaxIdlePerWorker
		}
	}
	life := cfg.MaxLifetime
	if life == 0 {
		life = defaultMaxLifetime
	}
	idleT := cfg.MaxIdleTime
	if idleT == 0 {
		idleT = defaultMaxIdleTime
	}
	hc := cfg.HealthCheckInterval
	if hc == 0 {
		hc = defaultHealthCheck
	}
	if hc < 0 {
		// A negative value explicitly disables the health check.
		hc = 0
	}
	asyncCfg := async.PoolConfig{
		MaxOpen:          maxOpen,
		MaxIdlePerWorker: idle,
		MaxLifetime:      life,
		MaxIdleTime:      idleT,
		HealthCheck:      hc,
		NumWorkers:       nw,
	}
	p := &memcachedPool{
		cfg:      cfg,
		provider: provider,
		ownsLoop: ownsLoop,
	}
	dial := func(ctx context.Context, workerID int) (*memcachedConn, error) {
		c, err := dialMemcachedConn(ctx, provider, cfg, workerID)
		if err != nil {
			return nil, err
		}
		c.maxLifetime = life
		c.maxIdleTime = idleT
		return c, nil
	}
	p.pool = async.NewPool[*memcachedConn](asyncCfg, dial)
	return p
}

// acquire returns a conn with the given worker hint (-1 for RR). Stale
// (closed) idle conns are discarded with bounded retries.
func (p *memcachedPool) acquire(ctx context.Context, hint int) (*memcachedConn, error) {
	const maxRetries = 5
	for range maxRetries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c, err := p.pool.Acquire(ctx, hint)
		if err != nil {
			return nil, err
		}
		if c.closed.Load() {
			p.pool.Discard(c)
			continue
		}
		return c, nil
	}
	return nil, ErrPoolExhausted
}

// release returns c to the pool's idle list.
func (p *memcachedPool) release(c *memcachedConn) {
	if c == nil {
		return
	}
	if c.closed.Load() {
		p.pool.Discard(c)
		return
	}
	c.touch()
	p.pool.Release(c)
}

// discard drops c from the pool unconditionally.
func (p *memcachedPool) discard(c *memcachedConn) {
	if c == nil {
		return
	}
	p.pool.Discard(c)
}

// Close closes the pool and releases the event loop if owned.
func (p *memcachedPool) Close() error {
	_ = p.pool.Close()
	if p.ownsLoop && p.provider != nil {
		eventloop.Release(p.provider)
		p.provider = nil
	}
	return nil
}

// Stats returns pool occupancy.
func (p *memcachedPool) Stats() async.PoolStats {
	return p.pool.Stats()
}
