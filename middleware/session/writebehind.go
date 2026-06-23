package session

import (
	"context"
	"sync"
	"time"

	"github.com/goceleris/celeris/middleware/store"
)

// writeBehindOp is a single deferred store write, captured off the request
// critical path. buf is an immutable snapshot of the encoded session body —
// it MUST be taken before the request releases its (pooled, recycled)
// session map, so the worker never reads memory the next request is mutating.
type writeBehindOp struct {
	id     string
	buf    []byte
	expiry time.Duration
}

// writeBehindWriter dispatches session store writes off the response critical
// path. A single background worker drains a bounded channel and performs the
// store.Set, so the response goroutine returns the moment the bytes are
// snapshotted and enqueued — the SET overlaps the next request instead of
// gating it.
//
// Concurrency is bounded by design: exactly one worker goroutine, never a
// per-request fan-out, so a slow or stalled store applies natural
// backpressure (the enqueue blocks on a full channel) rather than spawning
// unbounded goroutines. Writes for the same session ID are serialized through
// the single worker in enqueue order, so a later Set for a session never
// races or reorders ahead of an earlier one.
//
// Durability tradeoff: a write acknowledged to the HTTP client is NOT yet
// guaranteed to be in the store when the response is sent. A process crash
// (SIGKILL, panic, power loss) between enqueue and drain loses that write.
// Close drains every in-flight and queued op before returning, so a graceful
// shutdown loses nothing.
type writeBehindWriter struct {
	kv      store.KV
	ch      chan writeBehindOp
	onError func(err error)

	// writeCtx is a writer-owned, process-lifetime context detached from any
	// request context. The request context is canceled the moment the
	// response completes, so reusing it here would cancel the deferred Set;
	// the deferred write outlives the request by construction.
	writeCtx context.Context

	// mu guards closed and serializes channel sends against Close so a
	// request racing shutdown can never send on a closed channel (which
	// would panic). The mutex is held only for the duration of a buffered
	// send/flag flip, never across the store write itself.
	mu     sync.Mutex
	closed bool
	done   chan struct{} // closed when the worker goroutine has exited
}

// writeBehindQueueDepth bounds the number of pending deferred writes. A
// modest depth absorbs request bursts while capping memory and guaranteeing
// the enqueue blocks (backpressure) rather than dropping writes when a slow
// store falls behind. Sized to overlap a few requests' worth of writes
// without letting the backlog grow without bound.
const writeBehindQueueDepth = 256

// newWriteBehindWriter starts the background worker. onError, if non-nil, is
// invoked for every failed deferred Set (the request goroutine has already
// returned, so the error cannot flow back up the middleware chain).
func newWriteBehindWriter(kv store.KV, onError func(err error)) *writeBehindWriter {
	w := &writeBehindWriter{
		kv:       kv,
		ch:       make(chan writeBehindOp, writeBehindQueueDepth),
		onError:  onError,
		writeCtx: context.Background(),
		done:     make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *writeBehindWriter) run() {
	defer close(w.done)
	for op := range w.ch {
		if err := w.kv.Set(w.writeCtx, op.id, op.buf, op.expiry); err != nil && w.onError != nil {
			w.onError(err)
		}
	}
}

// enqueue hands a snapshotted write to the background worker. buf MUST be a
// fresh, immutable slice (the JSON-encoded body), never the live session map
// or a buffer the next request may reuse. Blocks when the queue is full,
// applying backpressure instead of unbounded goroutine growth.
//
// After Close, the worker is gone and enqueue degrades to a synchronous
// store write on the calling goroutine, so a request racing shutdown is never
// silently dropped. The mutex makes the send-vs-close decision atomic: a send
// can never land on a channel Close has already closed.
func (w *writeBehindWriter) enqueue(id string, buf []byte, expiry time.Duration) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		// Worker stopped; write synchronously rather than lose the update.
		if err := w.kv.Set(w.writeCtx, id, buf, expiry); err != nil && w.onError != nil {
			w.onError(err)
		}
		return
	}
	w.ch <- writeBehindOp{id: id, buf: buf, expiry: expiry}
	w.mu.Unlock()
}

// Close stops accepting new deferred writes and blocks until every queued and
// in-flight write has been applied to the store. It is safe to call multiple
// times and from multiple goroutines. This is the shutdown-flush guarantee:
// after Close returns, no enqueued write has been lost.
func (w *writeBehindWriter) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		<-w.done
		return nil
	}
	w.closed = true
	close(w.ch)
	w.mu.Unlock()
	<-w.done
	return nil
}
