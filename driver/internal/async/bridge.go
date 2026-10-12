package async

import (
	"context"
	"sync"
)

// PendingRequest is any driver-specific request that expects a response on
// the wire. Implementations supply their own result plumbing (typically a
// doneCh channel owned by the driver); the bridge only manages the FIFO
// queue and completion notification on drain.
type PendingRequest interface {
	// Ctx returns the request context. If the context is done before the
	// response arrives, the bridge still leaves the request on the queue —
	// cancellation is the driver's responsibility (e.g. Postgres
	// CancelRequest on a side connection).
	Ctx() context.Context
}

// Bridge manages an ordered queue of pending requests for a single
// connection. The wire protocol must guarantee responses are delivered in
// request order (true for both Postgres and Redis on a single connection).
//
// Bridge is safe for concurrent use: a writer goroutine enqueues requests
// while the reader goroutine pops them as responses arrive.
//
// The backing store is a power-of-two ring buffer so steady-state
// enqueue/pop cycles at a bounded depth (the common case for pipelining) do
// not allocate — a plain append+reslice queue would re-grow its backing
// array every time Pop walked it forward past the capacity boundary.
type Bridge struct {
	mu   sync.Mutex
	ring []PendingRequest
	mask int
	head int // next index to pop
	size int // live entries
}

// NewBridge returns an empty Bridge.
func NewBridge() *Bridge {
	const initialCap = 16
	return &Bridge{ring: make([]PendingRequest, initialCap), mask: initialCap - 1}
}

// grow doubles the ring capacity, unwrapping live entries to [0, size).
// Caller must hold b.mu.
func (b *Bridge) grow() {
	n := len(b.ring) * 2
	if n == 0 {
		n = 16
	}
	next := make([]PendingRequest, n)
	for i := 0; i < b.size; i++ {
		next[i] = b.ring[(b.head+i)&b.mask]
	}
	b.ring = next
	b.mask = n - 1
	b.head = 0
}

// Enqueue appends req to the back of the queue.
func (b *Bridge) Enqueue(req PendingRequest) {
	b.mu.Lock()
	if b.size == len(b.ring) {
		b.grow()
	}
	b.ring[(b.head+b.size)&b.mask] = req
	b.size++
	b.mu.Unlock()
}

// Head returns the front-of-queue request without removing it, or nil if
// the queue is empty.
func (b *Bridge) Head() PendingRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.size == 0 {
		return nil
	}
	return b.ring[b.head]
}

// Pop removes and returns the front-of-queue request, or nil if empty.
func (b *Bridge) Pop() PendingRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.size == 0 {
		return nil
	}
	r := b.ring[b.head]
	b.ring[b.head] = nil
	b.head = (b.head + 1) & b.mask
	b.size--
	return r
}

// PopTail removes and returns the back-of-queue request, or nil if empty.
// Drivers use this to unwind a request that was enqueued but whose wire bytes
// never reached the server (for example, when Write returned an error). Do
// NOT use it to pop arbitrary entries from the middle — the underlying
// protocols (Postgres, Redis) are FIFO, so out-of-order removal desyncs
// subsequent responses.
func (b *Bridge) PopTail() PendingRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.size == 0 {
		return nil
	}
	idx := (b.head + b.size - 1) & b.mask
	r := b.ring[idx]
	b.ring[idx] = nil
	b.size--
	return r
}

// Len returns the current queue depth.
func (b *Bridge) Len() int {
	b.mu.Lock()
	n := b.size
	b.mu.Unlock()
	return n
}

// DrainWithError pops every pending request and invokes notify with the
// given error for each one, in queue order. The queue is left empty on
// return. notify must not call back into Bridge on the same instance — do
// that from a separate goroutine if needed.
//
// The requests are taken out of the ring, and their slots cleared, under b.mu;
// notify runs after the lock is released and touches only that copy. The ring
// is shared with Enqueue and Pop, which reuse its slots as soon as head and
// size are reset: a drain that walked the ring after unlocking would read
// and clear slots a concurrent Enqueue had just filled, notifying the new
// request with the old requests' error and losing an old one (celeris#887).
func (b *Bridge) DrainWithError(err error, notify func(PendingRequest, error)) {
	b.mu.Lock()
	var drained []PendingRequest
	if notify != nil && b.size > 0 {
		drained = make([]PendingRequest, 0, b.size)
	}
	for i := 0; i < b.size; i++ {
		idx := (b.head + i) & b.mask
		if drained != nil {
			drained = append(drained, b.ring[idx])
		}
		b.ring[idx] = nil
	}
	b.head = 0
	b.size = 0
	b.mu.Unlock()
	for _, req := range drained {
		if req != nil {
			notify(req, err)
		}
	}
}
