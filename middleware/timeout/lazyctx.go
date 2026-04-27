package timeout

import (
	"context"
	"sync"
	"time"
)

// closedChan is a singleton closed channel returned by lazyDeadlineCtx.Done
// after release() in the case where no real timer was ever armed.
var closedChan = func() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// lazyDeadlineCtx wraps a parent [context.Context] with a deadline that
// is enforced lazily. [context.WithTimeout] eagerly allocates a
// timerCtx, a cancel closure, and schedules a runtime timer — ~8 heap
// objects on the order of a few hundred nanoseconds per request, even
// when the handler returns long before the deadline and never reads
// [context.Context.Done].
//
// lazyDeadlineCtx defers the timer machinery until something actually
// observes Done() (e.g. a downstream goroutine watching for cancellation,
// an http.Client respecting the deadline). For the dominant timeout
// workload — a synchronous handler that returns inside the deadline
// without touching Done() — lazyDeadlineCtx allocates only itself, and
// the middleware checks elapsed wall time after Next() returns to detect
// any deadline slip.
//
// Cancellation semantics match [context.WithTimeout]: once release() is
// called, Done() returns a closed channel and Err() returns
// [context.Canceled], even if Done() is observed for the first time
// after release. This preserves the "ctx held past middleware return"
// pattern that exposes a cancelled context to whoever holds the handle.
type lazyDeadlineCtx struct {
	parent   context.Context
	deadline time.Time

	mu        sync.Mutex
	realCtx   context.Context
	cancel    context.CancelFunc
	cancelled bool
}

// newLazyDeadlineCtx constructs a lazy-deadline context wrapping parent.
func newLazyDeadlineCtx(parent context.Context, deadline time.Time) *lazyDeadlineCtx {
	return &lazyDeadlineCtx{parent: parent, deadline: deadline}
}

// release cancels any timer that was lazily created. After release,
// any subsequent Done() returns a closed channel and Err() returns
// [context.Canceled] — matching the eager [context.WithTimeout] +
// `defer cancel()` semantics.
func (c *lazyDeadlineCtx) release() {
	c.mu.Lock()
	c.cancelled = true
	if c.cancel != nil {
		cancel := c.cancel
		c.cancel = nil
		c.mu.Unlock()
		cancel()
		return
	}
	c.mu.Unlock()
}

// expired reports whether the deadline has passed. Used by the timeout
// middleware after Next() returns to synthesize a DeadlineExceeded
// when the lazy timer was never armed (the handler returned before
// reading Done()).
func (c *lazyDeadlineCtx) expired() bool {
	return !time.Now().Before(c.deadline)
}

// Deadline returns the configured deadline. Always available without
// triggering timer allocation.
func (c *lazyDeadlineCtx) Deadline() (time.Time, bool) {
	return c.deadline, true
}

// Done lazily promotes to a real [context.WithDeadline]. The first
// caller pays the same allocation cost the eager path would have paid;
// subsequent calls return the cached channel. After release(), returns
// a singleton already-closed channel without allocating a timer.
func (c *lazyDeadlineCtx) Done() <-chan struct{} {
	c.mu.Lock()
	if c.realCtx != nil {
		ctx := c.realCtx
		c.mu.Unlock()
		return ctx.Done()
	}
	if c.cancelled {
		c.mu.Unlock()
		return closedChan
	}
	c.realCtx, c.cancel = context.WithDeadline(c.parent, c.deadline)
	ctx := c.realCtx
	c.mu.Unlock()
	return ctx.Done()
}

// Err returns context.Canceled if release() ran (without forcing the
// timer up); context.DeadlineExceeded if the wall-clock deadline has
// passed; otherwise propagates the parent's Err. Once Done() has been
// called the real context's Err is authoritative.
func (c *lazyDeadlineCtx) Err() error {
	c.mu.Lock()
	if c.realCtx != nil {
		ctx := c.realCtx
		c.mu.Unlock()
		return ctx.Err()
	}
	if c.cancelled {
		c.mu.Unlock()
		return context.Canceled
	}
	c.mu.Unlock()
	if c.expired() {
		return context.DeadlineExceeded
	}
	return c.parent.Err()
}

// Value delegates to the parent context so middleware-installed values
// (request IDs, auth principals, etc.) remain visible.
func (c *lazyDeadlineCtx) Value(key any) any {
	return c.parent.Value(key)
}
