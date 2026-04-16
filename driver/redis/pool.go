package redis

import (
	"context"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/redis/protocol"
	"github.com/goceleris/celeris/engine"
)

// redisPool bundles two async.Pools: one for command-mode connections and a
// dedicated one for pub/sub — the latter must never be reused for command
// replies because push frames interleave unpredictably.
type redisPool struct {
	cmd      *async.Pool[*redisConn]
	pubsub   *async.Pool[*redisConn]
	cfg      Config
	provider engine.EventLoopProvider
	ownsLoop bool
}

func newPool(cfg Config, provider engine.EventLoopProvider, ownsLoop bool) *redisPool {
	nw := provider.NumWorkers()
	if nw <= 0 {
		nw = 1
	}
	maxOpen := cfg.PoolSize
	if maxOpen == 0 {
		maxOpen = nw * 4
	}
	idle := cfg.MaxIdlePerWorker
	if idle == 0 {
		idle = defaultMaxIdlePerWorker
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
		hc = 30 * time.Second
	}
	// A negative value explicitly disables the health check.
	if hc < 0 {
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
	p := &redisPool{
		cfg:      cfg,
		provider: provider,
		ownsLoop: ownsLoop,
	}
	dialCmd := func(ctx context.Context, workerID int) (*redisConn, error) {
		c, err := dialRedisConn(ctx, provider, cfg, workerID)
		if err != nil {
			return nil, err
		}
		c.maxLifetime = life
		c.maxIdleTime = idleT
		c.state.onPush = cfg.OnPush
		return c, nil
	}
	p.cmd = async.NewPool[*redisConn](asyncCfg, dialCmd)
	// Pubsub pool has its own config — pub/sub conns are long-lived, often
	// one per subscriber.
	pubsubCfg := asyncCfg
	pubsubCfg.MaxOpen = 0 // unlimited (caller-controlled via Subscribe count)
	pubsubCfg.MaxIdlePerWorker = 0
	p.pubsub = async.NewPool[*redisConn](pubsubCfg, dialCmd)
	return p
}

// pinnedConnKey is the context key carrying a pre-acquired *redisConn that
// must be reused by acquireCmd (and skipped by releaseCmd). Used by the
// cluster ASK redirect to guarantee ASKING and the following command share
// the same TCP connection.
type pinnedConnKey struct{}

// acquireCmd returns a cmd-mode conn with the given worker hint (-1 for RR).
// If ctx carries a pinned conn (see pinnedConnKey), it is returned directly
// without touching the pool. Stale (closed) idle conns are discarded with
// bounded retries to avoid unbounded recursion when the entire idle pool is
// dead. The underlying pool blocks when MaxOpen is reached (wait-queue
// semantics), so ErrPoolExhausted is only returned when every retry finds a
// closed conn.
func (p *redisPool) acquireCmd(ctx context.Context, hint int) (*redisConn, error) {
	if c, ok := ctx.Value(pinnedConnKey{}).(*redisConn); ok && c != nil {
		c.pinned.Add(1)
		return c, nil
	}
	const maxRetries = 5
	for range maxRetries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c, err := p.cmd.Acquire(ctx, hint)
		if err != nil {
			return nil, err
		}
		if c.closed.Load() {
			p.cmd.Discard(c)
			continue
		}
		return c, nil
	}
	return nil, ErrPoolExhausted
}

// releaseCmd returns c to the cmd pool. Before returning the conn to the
// shared idle list, a best-effort DISCARD is sent so that a caller who
// issued MULTI without a matching EXEC/DISCARD cannot leave the conn in a
// dirty transactional state for the next acquirer. DISCARD is idempotent
// and replies with -ERR (ignored) outside of MULTI.
func (p *redisPool) releaseCmd(c *redisConn) {
	if c == nil {
		return
	}
	// If the conn was acquired via a pinned context, decrement the pin
	// counter and skip the real release — the original acquirer will do it.
	if c.pinned.Add(-1) >= 0 {
		return
	}
	// Counter went below zero: this is the real (non-pinned) release path.
	// Restore to zero so a future pin cycle starts clean.
	c.pinned.Store(0)
	if c.closed.Load() {
		p.cmd.Discard(c)
		return
	}
	c.resetSession()
	if c.closed.Load() {
		// resetSession may have triggered a close (e.g. write failure).
		p.cmd.Discard(c)
		return
	}
	c.touch()
	p.cmd.Release(c)
}

// discardCmd drops c from the cmd pool unconditionally.
func (p *redisPool) discardCmd(c *redisConn) {
	if c == nil {
		return
	}
	p.cmd.Discard(c)
}

// acquirePubSub dials a dedicated push-mode conn. It is never returned to the
// pool; callers close it via releasePubSub.
func (p *redisPool) acquirePubSub(ctx context.Context) (*redisConn, error) {
	c, err := p.pubsub.Acquire(ctx, -1)
	if err != nil {
		return nil, err
	}
	c.state.setMode(modePubSub)
	return c, nil
}

// releasePubSub closes the dedicated pubsub conn.
func (p *redisPool) releasePubSub(c *redisConn) {
	if c == nil {
		return
	}
	p.pubsub.Discard(c)
}

// Close closes both pools and releases the event loop if owned.
func (p *redisPool) Close() error {
	_ = p.cmd.Close()
	_ = p.pubsub.Close()
	if p.ownsLoop && p.provider != nil {
		eventloop.Release(p.provider)
		p.provider = nil
	}
	return nil
}

// setOnPush updates the push callback in the config so future dials pick it
// up. Existing connections retain whatever callback was set at dial time.
func (p *redisPool) setOnPush(fn func(string, []protocol.Value)) {
	p.cfg.OnPush = fn
}

// Stats returns command-pool statistics.
func (p *redisPool) Stats() async.PoolStats {
	return p.cmd.Stats()
}
