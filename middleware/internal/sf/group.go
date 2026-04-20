// Package sf provides a tiny generic singleflight-style coalescer used
// internally by middlewares that need to dedupe concurrent in-flight
// producers for the same key (middleware/cache — and, potentially,
// future adapters). The public middleware/singleflight package serves a
// different purpose (HTTP-response coalescing at the chain level) and
// owns its own implementation tuned for http/celeris types.
package sf

import "sync"

// Call is a single in-flight call for a key. Followers block on wg; the
// leader populates Result/Err before calling wg.Done.
type Call[T any] struct {
	wg     sync.WaitGroup
	Result T
	Err    error
}

// Group coalesces concurrent calls keyed by string. The leader runs the
// producer; followers block on the leader's result.
type Group[T any] struct {
	mu    sync.Mutex
	calls map[string]*Call[T]
}

// New returns an initialised Group[T].
func New[T any]() *Group[T] {
	return &Group[T]{calls: make(map[string]*Call[T])}
}

// Do runs fn for the first caller of a given key and returns its result
// to every caller. The bool second return is true for the leader (the
// caller that actually ran fn) and false for followers.
func (g *Group[T]) Do(key string, fn func() (T, error)) (T, bool, error) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.Result, false, c.Err
	}
	c := &Call[T]{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
		c.wg.Done()
	}()

	result, err := fn()
	c.Result = result
	c.Err = err
	return result, true, err
}
