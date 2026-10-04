// Package sf provides a tiny generic singleflight-style coalescer used
// internally by middlewares that need to dedupe concurrent in-flight
// producers for the same key (middleware/cache — and, potentially,
// future adapters). The public middleware/singleflight package serves a
// different purpose (HTTP-response coalescing at the chain level) and
// owns its own implementation tuned for http/celeris types.
package sf

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/middleware/internal/handoff"
)

// ErrLeaderPanicked is what the followers of a call get when the leader's
// fn, or its then, panicked before the result was published. The panic
// itself stays the leader's: it continues, unchanged, in the leader's
// goroutine.
var ErrLeaderPanicked = errors.New("sf: the coalesced call's leader panicked")

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
	// expiresAt is when the published result stops being handed to a
	// caller that arrives while then still holds the key, in nanoseconds
	// since clockBase; 0 until the result is published, and when fn gave
	// it no limit.
	expiresAt atomic.Int64
	// replaced is set, under the group's mutex, when a newer call took
	// this one's place in the map (see expired); release then leaves the
	// key alone.
	replaced bool
	Result   T
	Err      error
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

// TestHookFollowerJoined is called, when set, under the group's mutex each
// time a caller joins an in-flight call as a follower. Nil in production;
// middleware/cache's tests use it to know a follower waits. Cost in
// production: one nil check per coalesced call.
var TestHookFollowerJoined func()

// New returns an initialised Group[T].
func New[T any]() *Group[T] {
	g := &Group[T]{calls: make(map[string]*Call[T])}
	g.pool.New = func() any { return &Call[T]{} }
	return g
}

// Do runs fn for the first caller of a given key and returns its result
// to every caller. The bool second return is true for the leader (the
// caller that actually ran fn) and false for followers. The leader gets
// fn's error itself; followers get it as [handoff.Error] returns it.
//
// Once fn has returned, its result is published and the followers are
// released. Then the leader runs then (when not nil) with that result while
// the key is still held, so a caller that arrives meanwhile takes the
// published result at once instead of running fn again; middleware/cache
// stores the result there, so its followers never wait for the store. The
// key is released when then returns. fn also returns how long its result
// stays fresh (0: no limit): a caller that arrives later than that after the
// result was published, while then still holds the key, leads a call of its
// own instead, so a then that never returns cannot keep handing out a result
// past its lifetime. When fn returns an error, the key is released before
// then runs: there is no result to hand to a later caller.
//
// A follower waits only as long as ctx lives: when ctx ends first, it gets
// ctx's error. A ctx without a Done channel (context.Background, which is
// what an HTTP/1 request on epoll or io_uring has unless a middleware such
// as timeout gives it a deadline) does not end, and its follower waits for
// the leader. When fn or then panics, the key is released all the same, and
// a follower that had not yet been given a result gets [ErrLeaderPanicked];
// the panic continues in the leader (celeris#921).
func (g *Group[T]) Do(ctx context.Context, key string, fn func() (T, time.Duration, error), then func(T, error)) (T, bool, error) {
	g.mu.Lock()
	cur, ok := g.calls[key]
	if ok && !cur.expired() {
		c := cur
		// Follower. Register under the mutex so the leader's delete-time
		// read of waiters captures us before it decides whether to pool.
		c.waiters.Add(1)
		if TestHookFollowerJoined != nil {
			TestHookFollowerJoined()
		}
		g.mu.Unlock()
		if err := wait(ctx, &c.wg); err != nil {
			var zero T
			return zero, false, err
		}
		return c.Result, false, c.Err
	}
	// No call for the key, or one whose published result is past its
	// freshness while its then still runs: this caller leads a call of its
	// own, which replaces that one in the map (release then leaves the key
	// to the newer call).
	if ok {
		cur.replaced = true
	}
	c := g.pool.Get().(*Call[T])
	c.waiters.Store(0)
	c.expiresAt.Store(0)
	c.replaced = false
	c.Err = nil
	var zero T
	c.Result = zero
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	// release runs on every way out of the leader, a panic in fn or then
	// included: without it a panic left the key's call in the map and its
	// followers in wg.Wait for good, and every later caller for the key
	// joined them (celeris#921).
	published, finished := false, false
	defer func() {
		if finished {
			return
		}
		g.mu.Lock()
		g.release(key, c)
		g.mu.Unlock()
		if !published {
			c.Result = zero
			c.Err = ErrLeaderPanicked
			c.wg.Done()
		}
		// Never pooled: a follower may still read it.
	}()

	result, fresh, err := fn()

	if err != nil {
		// An error has no result for a later caller to take, so the key
		// is released first; then the waiter count is final, and the
		// followers' copy of the error (handoff.Error, a few allocations)
		// is made only when there is a follower to read it.
		g.mu.Lock()
		g.release(key, c)
		numWaiters := c.waiters.Load()
		g.mu.Unlock()
		c.Result = result
		if numWaiters > 0 {
			c.Err = handoff.Error(err)
		}
		published, finished = true, true
		c.wg.Done()
		if numWaiters == 0 {
			c.Result = zero
			c.Err = nil
			g.pool.Put(c)
		}
		if then != nil {
			then(result, err)
		}
		return result, true, err
	}

	// Publish before then runs: a follower that joins from here on reads
	// Result/Err after wg.Done, so they are populated whatever the waiter
	// count is now.
	c.Result = result
	if fresh > 0 {
		c.expiresAt.Store(int64(time.Since(clockBase) + fresh))
	}
	published = true
	c.wg.Done()

	if then != nil {
		then(result, err)
	}

	g.mu.Lock()
	g.release(key, c)
	// Snapshot waiters under the same mutex that gates follower
	// increments. No new follower can find this entry after the release.
	numWaiters := c.waiters.Load()
	g.mu.Unlock()
	finished = true

	if numWaiters == 0 {
		// No follower ever saw c; safe to recycle. Clear T-side fields
		// to avoid retaining pointers across pool cycles.
		c.Result = zero
		c.Err = nil
		g.pool.Put(c)
	}

	return result, true, err
}

// release removes c from the map under key, unless a newer call already
// replaced it there (see expired). The caller holds g.mu.
func (g *Group[T]) release(key string, c *Call[T]) {
	if !c.replaced {
		delete(g.calls, key)
	}
}

// expired reports whether c's result was published with a freshness that has
// since run out. The caller holds the group's mutex.
func (c *Call[T]) expired() bool {
	at := c.expiresAt.Load()
	return at != 0 && int64(time.Since(clockBase)) >= at
}

// clockBase is the origin of expiresAt: time.Since reads the monotonic clock,
// which a wall-clock step does not move.
var clockBase = time.Now()

// wait waits for wg, or for ctx to end, whichever comes first, and returns
// ctx's error in the second case.
func wait(ctx context.Context, wg *sync.WaitGroup) error {
	if ctx == nil || ctx.Done() == nil {
		wg.Wait()
		return nil
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
