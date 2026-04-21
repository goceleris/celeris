// Package sf provides a tiny generic singleflight-style coalescer used
// internally by middlewares that need to dedupe concurrent in-flight
// producers for the same key (middleware/cache — and, potentially,
// future adapters). The public middleware/singleflight package serves a
// different purpose (HTTP-response coalescing at the chain level) and
// owns its own implementation tuned for http/celeris types.
package sf

import (
	"sync"
	"sync/atomic"
)

// Call is a single in-flight call for a key. Followers block on wg; the
// leader populates Result/Err before calling wg.Done.
type Call[T any] struct {
	wg sync.WaitGroup
	// waiters counts concurrent followers that joined the leader's
	// in-flight state. Followers increment under the group mutex before
	// releasing; leader reads under the same mutex at delete time. A
	// zero count is the signal that the entry can be pool-recycled —
	// no follower will ever read Result/Err. Mirrors the design added
	// to middleware/singleflight in R73/R74.
	waiters atomic.Int32
	Result  T
	Err     error
}

// Group coalesces concurrent calls keyed by string. The leader runs the
// producer; followers block on the leader's result.
type Group[T any] struct {
	mu    sync.Mutex
	calls map[string]*Call[T]
	// pool recycles *Call[T] entries whose leader finished with no
	// followers observing the result. Entries seen by followers must
	// stay live until the last follower reads Result/Err, so they are
	// not pooled.
	pool sync.Pool
}

// New returns an initialised Group[T].
func New[T any]() *Group[T] {
	g := &Group[T]{calls: make(map[string]*Call[T])}
	g.pool.New = func() any { return &Call[T]{} }
	return g
}

// Do runs fn for the first caller of a given key and returns its result
// to every caller. The bool second return is true for the leader (the
// caller that actually ran fn) and false for followers.
func (g *Group[T]) Do(key string, fn func() (T, error)) (T, bool, error) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		// Follower. Register under the mutex so the leader's delete-time
		// read of waiters captures us before it decides whether to pool.
		c.waiters.Add(1)
		g.mu.Unlock()
		c.wg.Wait()
		return c.Result, false, c.Err
	}
	c := g.pool.Get().(*Call[T])
	c.waiters.Store(0)
	c.Err = nil
	var zero T
	c.Result = zero
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	result, err := fn()

	g.mu.Lock()
	delete(g.calls, key)
	// Snapshot waiters under the same mutex that gates follower
	// increments. No new follower can find this entry after the delete.
	numWaiters := c.waiters.Load()
	g.mu.Unlock()

	if numWaiters > 0 {
		// Followers will read these fields after wg.Done; populate.
		c.Result = result
		c.Err = err
	}
	c.wg.Done()

	if numWaiters == 0 {
		// No follower ever saw c; safe to recycle. Clear T-side fields
		// to avoid retaining pointers across pool cycles.
		c.Result = zero
		c.Err = nil
		g.pool.Put(c)
	}

	return result, true, err
}
