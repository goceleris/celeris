package async

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ErrPoolClosed is returned from Acquire once the pool has been closed.
var ErrPoolClosed = errors.New("celeris/async: pool is closed")

// ErrPoolExhausted is returned when MaxOpen is reached and no slot becomes
// available before the context deadline.
//
// Deprecated: Since the introduction of the wait-queue, Acquire blocks until a
// slot is available or the context expires (returning ctx.Err()). This
// sentinel is retained for backward compatibility but is no longer returned by
// Pool.Acquire.
var ErrPoolExhausted = errors.New("celeris/async: pool exhausted")

// Conn is the minimal interface a pooled connection must satisfy.
//
// Implementations are expected to be goroutine-safe for Close; other methods
// are called serially by the pool while a single goroutine owns the
// connection.
type Conn interface {
	// Ping verifies the connection is usable. It is invoked by the
	// periodic health check and on acquire when a lifetime is configured.
	Ping(ctx context.Context) error
	// Close releases any resources. Called at most once per connection.
	Close() error
	// Worker returns the engine worker index this connection is pinned to.
	// Pool routes idle connections back to this worker's idle list.
	Worker() int
	// IsExpired reports whether this connection has exceeded MaxLifetime.
	IsExpired(now time.Time) bool
	// IsIdleTooLong reports whether this connection has been idle for
	// longer than MaxIdleTime.
	IsIdleTooLong(now time.Time) bool
}

// PoolConfig controls pool sizing and lifetime.
type PoolConfig struct {
	// MaxOpen is the total connection cap across all workers.
	// Zero means unlimited.
	MaxOpen int
	// MaxIdlePerWorker bounds the idle pool held on each worker. Connections
	// beyond this limit on Release are closed.
	MaxIdlePerWorker int
	// MaxLifetime is the max age of a connection before it is evicted on
	// release or health check. Zero disables lifetime expiry.
	MaxLifetime time.Duration
	// MaxIdleTime is the max time a connection can sit idle before
	// eviction. Zero disables idle expiry.
	MaxIdleTime time.Duration
	// HealthCheck is the interval at which the background checker runs.
	// Zero disables the checker.
	HealthCheck time.Duration
	// NumWorkers is the number of worker-affinity slots. Must be >= 1.
	NumWorkers int
}

// Pool is a generic worker-affinity pool. Drivers supply a dial function and
// a conn type implementing Conn.
//
// Each worker owns an idle list guarded by its own lock. When MaxOpen > 0 a
// buffered channel (sem) acts as a counting semaphore that limits concurrent
// in-use connections. Acquire blocks (respecting the context deadline) until a
// slot is available, matching database/sql.DB semantics.
type Pool[C Conn] struct {
	cfg  PoolConfig
	dial func(ctx context.Context, workerID int) (C, error)

	perWorker []*workerSlot[C]
	openCount int64  // atomic — total open conns across all workers (stats)
	rr        uint64 // atomic — round-robin cursor for Acquire hint<0

	// sem limits concurrent in-use connections when MaxOpen > 0.
	// A send acquires a slot; a receive releases it. nil when unlimited.
	sem chan struct{}

	closed atomic.Bool

	hcStop chan struct{}
	hcDone chan struct{}
}

type workerSlot[C Conn] struct {
	mu   sync.Mutex
	idle []C
}

// NewPool constructs a pool with the given configuration. It panics if
// NumWorkers is less than 1 or dial is nil.
func NewPool[C Conn](cfg PoolConfig, dial func(ctx context.Context, workerID int) (C, error)) *Pool[C] {
	if cfg.NumWorkers < 1 {
		panic("celeris/async: PoolConfig.NumWorkers must be >= 1")
	}
	if dial == nil {
		panic("celeris/async: dial function must not be nil")
	}
	p := &Pool[C]{
		cfg:       cfg,
		dial:      dial,
		perWorker: make([]*workerSlot[C], cfg.NumWorkers),
	}
	if cfg.MaxOpen > 0 {
		p.sem = make(chan struct{}, cfg.MaxOpen)
	}
	for i := range p.perWorker {
		p.perWorker[i] = &workerSlot[C]{}
	}
	if cfg.HealthCheck > 0 {
		p.hcStop = make(chan struct{})
		p.hcDone = make(chan struct{})
		go p.healthLoop()
	}
	return p
}

// Acquire returns a connection pinned to a worker. If workerHint is negative
// or >= NumWorkers, a round-robin worker is chosen.
//
// When MaxOpen > 0 and all slots are in use, Acquire blocks until a slot is
// released or the context expires (returning ctx.Err()). The semaphore tracks
// in-use connections; idle connections do not hold slots.
func (p *Pool[C]) Acquire(ctx context.Context, workerHint int) (C, error) {
	var zero C
	if p.closed.Load() {
		return zero, ErrPoolClosed
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}

	// Reserve a slot. Blocks when all MaxOpen slots are in use.
	if p.sem != nil {
		select {
		case p.sem <- struct{}{}:
		case <-ctx.Done():
			return zero, fmt.Errorf("celeris/pool: acquire blocked and timed out (MaxOpen=%d): %w", p.cfg.MaxOpen, ctx.Err())
		}
	}

	// From here on we own a sem slot — every error path must release it.
	c, err := p.acquireInner(ctx, workerHint)
	if err != nil {
		if p.sem != nil {
			<-p.sem
		}
		return zero, err
	}
	return c, nil
}

// acquireInner tries idle lists then dials. Caller holds a sem slot.
func (p *Pool[C]) acquireInner(ctx context.Context, workerHint int) (C, error) {
	var zero C
	nw := len(p.perWorker)
	if workerHint < 0 || workerHint >= nw {
		workerHint = int(atomic.AddUint64(&p.rr, 1)-1) % nw
	}

	now := time.Now()

	// 1. Try hinted worker's idle list.
	if c, ok := p.popIdle(workerHint, now); ok {
		return c, nil
	}
	// 2. Scan other workers' idle lists.
	for i := 1; i < nw; i++ {
		w := (workerHint + i) % nw
		if c, ok := p.popIdle(w, now); ok {
			return c, nil
		}
	}
	// 3. Dial a new connection.
	atomic.AddInt64(&p.openCount, 1)
	c, err := p.dial(ctx, workerHint)
	if err != nil {
		atomic.AddInt64(&p.openCount, -1)
		return zero, err
	}
	return c, nil
}

// popIdle removes the most recently released conn on worker w, skipping any
// that are expired. Evicted conns are closed and openCount decremented.
func (p *Pool[C]) popIdle(w int, now time.Time) (C, bool) {
	var zero C
	slot := p.perWorker[w]
	slot.mu.Lock()
	for len(slot.idle) > 0 {
		n := len(slot.idle)
		c := slot.idle[n-1]
		slot.idle[n-1] = zero
		slot.idle = slot.idle[:n-1]
		expired := c.IsExpired(now) || c.IsIdleTooLong(now)
		if expired {
			slot.mu.Unlock()
			_ = c.Close()
			atomic.AddInt64(&p.openCount, -1)
			slot.mu.Lock()
			continue
		}
		slot.mu.Unlock()
		return c, true
	}
	slot.mu.Unlock()
	return zero, false
}

// Release returns c to its home worker's idle list and frees the semaphore
// slot so another Acquire can proceed. If the pool is closed, or the idle cap
// is exceeded, or the conn is expired, c is closed instead.
func (p *Pool[C]) Release(c C) {
	if p.closed.Load() {
		_ = c.Close()
		atomic.AddInt64(&p.openCount, -1)
		p.releaseSem()
		return
	}
	w := c.Worker()
	if w < 0 || w >= len(p.perWorker) {
		_ = c.Close()
		atomic.AddInt64(&p.openCount, -1)
		p.releaseSem()
		return
	}
	now := time.Now()
	if c.IsExpired(now) {
		_ = c.Close()
		atomic.AddInt64(&p.openCount, -1)
		p.releaseSem()
		return
	}
	slot := p.perWorker[w]
	slot.mu.Lock()
	if p.cfg.MaxIdlePerWorker > 0 && len(slot.idle) >= p.cfg.MaxIdlePerWorker {
		slot.mu.Unlock()
		_ = c.Close()
		atomic.AddInt64(&p.openCount, -1)
		p.releaseSem()
		return
	}
	slot.idle = append(slot.idle, c)
	slot.mu.Unlock()
	p.releaseSem()
}

// Discard permanently removes c from the pool: it is closed and openCount
// decremented. The semaphore slot is freed so another Acquire can proceed.
// Drivers call this on connection errors they cannot recover from.
func (p *Pool[C]) Discard(c C) {
	_ = c.Close()
	atomic.AddInt64(&p.openCount, -1)
	p.releaseSem()
}

// releaseSem frees one semaphore slot if the pool has a concurrency limit.
func (p *Pool[C]) releaseSem() {
	if p.sem != nil {
		<-p.sem
	}
}

// Close closes every idle connection, stops the health check, and prevents
// further Acquire calls. In-flight connections held by drivers will be
// closed when they are Release'd back to the pool.
func (p *Pool[C]) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	if p.hcStop != nil {
		close(p.hcStop)
		<-p.hcDone
	}
	var zero C
	for _, slot := range p.perWorker {
		slot.mu.Lock()
		for i, c := range slot.idle {
			_ = c.Close()
			atomic.AddInt64(&p.openCount, -1)
			slot.idle[i] = zero
		}
		slot.idle = nil
		slot.mu.Unlock()
	}
	return nil
}

// PoolStats reports current pool occupancy.
type PoolStats struct {
	// Open is the total number of connections (idle + in-use).
	Open int
	// Idle is the count across all worker idle lists.
	Idle int
	// InUse is Open - Idle.
	InUse int
	// PerWorker holds per-worker breakdowns; len == NumWorkers.
	PerWorker []PoolWorkerStats
}

// PoolWorkerStats reports per-worker occupancy.
type PoolWorkerStats struct {
	Idle int
}

// Stats returns a snapshot of current pool state.
func (p *Pool[C]) Stats() PoolStats {
	open := int(atomic.LoadInt64(&p.openCount))
	per := make([]PoolWorkerStats, len(p.perWorker))
	idle := 0
	for i, slot := range p.perWorker {
		slot.mu.Lock()
		per[i] = PoolWorkerStats{Idle: len(slot.idle)}
		idle += len(slot.idle)
		slot.mu.Unlock()
	}
	return PoolStats{
		Open:      open,
		Idle:      idle,
		InUse:     open - idle,
		PerWorker: per,
	}
}

// IdleConnWorkers returns the Worker() IDs of every currently-idle
// connection across all worker slots. The same worker ID may appear
// multiple times if the slot holds more than one idle conn. In-use
// conns do not appear — they are held by active callers and their
// Worker() is observable on the caller side. Useful for integration
// tests asserting that per-CPU affinity is actually being honored by
// the dial path.
func (p *Pool[C]) IdleConnWorkers() []int {
	var out []int
	for _, slot := range p.perWorker {
		slot.mu.Lock()
		for _, c := range slot.idle {
			out = append(out, c.Worker())
		}
		slot.mu.Unlock()
	}
	return out
}
