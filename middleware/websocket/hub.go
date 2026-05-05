package websocket

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
)

// HubPolicy controls what the [Hub] does with a [Conn] whose
// [Conn.WritePreparedMessage] failed during a Broadcast.
type HubPolicy uint8

const (
	// HubPolicyDrop skips the Conn for this message but keeps it
	// registered. Use when transient errors are expected (slow networks
	// where retries from a higher layer make sense).
	HubPolicyDrop HubPolicy = iota

	// HubPolicyRemove unregisters the Conn from the Hub without closing
	// it. The connection's lifecycle stays with whoever owns the Conn.
	HubPolicyRemove

	// HubPolicyClose unregisters the Conn AND closes the underlying
	// connection. Default — matches the implicit behavior of the
	// hand-rolled hub patterns this type replaces.
	HubPolicyClose
)

// HubConfig tunes a [Hub]. All fields are optional.
type HubConfig struct {
	// OnSlowConn is consulted whenever Broadcast/BroadcastPrepared/
	// BroadcastFilter fails to deliver a message to a specific Conn.
	// The error is the underlying [Conn.WritePreparedMessage] error
	// (commonly [ErrWriteClosed], [ErrWriteTimeout], or a wrapped I/O
	// error). When nil, [HubPolicyClose] is used.
	OnSlowConn func(c *Conn, err error) HubPolicy

	// MaxConcurrency caps the number of in-flight per-Conn writes
	// during a Broadcast. Zero means [DefaultHubConcurrency] —
	// runtime.GOMAXPROCS(0)*4 — which keeps goroutine pressure
	// bounded on very-large fan-outs while still leaving slow conns
	// non-blocking for the rest. Set to a negative value to opt OUT
	// (true unbounded) for benchmarks; set to a positive integer to
	// override the default.
	MaxConcurrency int
}

// DefaultHubConcurrency is used when [HubConfig.MaxConcurrency] is
// zero. Sized at GOMAXPROCS*4 — enough headroom that fast-cohort
// dispatches (where a slow conn is rare) never queue, while bounding
// peak goroutine count under burst load to a small multiple of CPU
// cores.
func DefaultHubConcurrency() int { return runtime.GOMAXPROCS(0) * 4 }

// Hub is the connection-set abstraction for WebSocket fan-out: register
// connections, broadcast to all (or a filtered subset), unregister on
// disconnect. Uses [PreparedMessage] under the hood so the wire frame
// is built once per Broadcast regardless of the Conn count.
//
// The internal mutex is an [sync.RWMutex]. Register / unregister / Close
// take the write lock; broadcasts only take the read lock for the
// snapshot, so register-while-broadcast does not serialise. Per-Conn
// writes happen outside the lock.
//
// Safe for concurrent use from any number of publishers and from any
// number of Register / unregister call sites.
type Hub struct {
	cfg    HubConfig
	mu     sync.RWMutex
	conns  map[*Conn]struct{}
	closed bool
	// inflight counts in-flight Broadcast / BroadcastPrepared /
	// BroadcastFilter calls so Close can wait for them to drain. The
	// counter is incremented under RLock at snapshot time and
	// decremented when dispatch returns.
	inflight sync.WaitGroup
}

// NewHub constructs a Hub with the given config.
func NewHub(cfg HubConfig) *Hub {
	return &Hub{
		cfg:   cfg,
		conns: make(map[*Conn]struct{}),
	}
}

// Register adds c to the Hub. Returns an unregister function the caller
// MUST defer; calling it twice is safe. Registering a Conn on a Hub
// that has been Close()'d is a no-op — the returned unregister is also
// a no-op.
func (h *Hub) Register(c *Conn) func() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return func() {}
	}
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.conns, c)
			h.mu.Unlock()
		})
	}
}

// Len reports the current number of registered Conns.
func (h *Hub) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

// Broadcast builds a [PreparedMessage] from messageType+data and
// dispatches it to every registered Conn. Returns the count of Conns
// the message reached and the first per-Conn error encountered (if any
// — failures are routed through [HubConfig.OnSlowConn]).
//
// Ordering: each individual Conn's wire writes are serialised by
// Conn's own write semaphore, so a single Broadcast's frame arrives
// at every Conn intact. Across calls to Broadcast there is no
// cross-Conn ordering guarantee — two parallel publishers may
// interleave on different Conns.
func (h *Hub) Broadcast(messageType MessageType, data []byte) (delivered int, err error) {
	pm, err := NewPreparedMessage(messageType, data)
	if err != nil {
		return 0, err
	}
	return h.BroadcastPrepared(pm)
}

// BroadcastPrepared dispatches an already-prepared message — useful in
// dispatch loops where the same payload is published repeatedly.
func (h *Hub) BroadcastPrepared(pm *PreparedMessage) (int, error) {
	snap, ok := h.snapshot()
	if !ok {
		return 0, nil
	}
	defer h.inflight.Done()
	return h.dispatch(snap, pm, nil)
}

// BroadcastFilter sends only to Conns where pred returns true. Common
// for room / channel routing — filter on Conn.Locals without building a
// second Hub.
//
// The membership snapshot happens under the Hub's read lock; pred is
// invoked LOCK-FREE against that snapshot. Parallel Register / unregister
// calls during dispatch are not observed by this broadcast.
func (h *Hub) BroadcastFilter(messageType MessageType, data []byte, pred func(*Conn) bool) (int, error) {
	if pred == nil {
		return h.Broadcast(messageType, data)
	}
	pm, err := NewPreparedMessage(messageType, data)
	if err != nil {
		return 0, err
	}
	snap, ok := h.snapshot()
	if !ok {
		return 0, nil
	}
	defer h.inflight.Done()
	return h.dispatch(snap, pm, pred)
}

// snapshot copies the current conn set under RLock and registers an
// inflight broadcast with the Hub's WaitGroup. Returns ok=false if the
// Hub is already closed (so the caller skips dispatch and the WaitGroup
// is NOT incremented).
func (h *Hub) snapshot() ([]*Conn, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return nil, false
	}
	h.inflight.Add(1)
	out := make([]*Conn, 0, len(h.conns))
	for c := range h.conns {
		out = append(out, c)
	}
	return out, true
}

// dispatch performs the per-Conn write for snap. Each conn's write
// runs in its own goroutine so a slow conn cannot gate the rest;
// MaxConcurrency, if positive, caps goroutine pressure via a semaphore.
// Removals/closes triggered by OnSlowConn are deferred until after the
// broadcast so the snapshot we are iterating is not mutated mid-loop.
func (h *Hub) dispatch(snap []*Conn, pm *PreparedMessage, pred func(*Conn) bool) (int, error) {
	var (
		delivered atomic.Int64
		mu        sync.Mutex
		firstErr  error
		toRemove  []*Conn
		toClose   []*Conn
	)

	// Concurrency cap: 0 ⇒ DefaultHubConcurrency (GOMAXPROCS*4);
	// negative ⇒ unbounded (one goroutine per matching conn). The
	// default keeps peak goroutine count bounded on a 10K-conn fan-
	// out without hurting the small-N case (the semaphore is unused
	// when concurrent dispatches stay below the cap).
	var sema chan struct{}
	maxConc := h.cfg.MaxConcurrency
	if maxConc == 0 {
		maxConc = DefaultHubConcurrency()
	}
	if maxConc > 0 {
		sema = make(chan struct{}, maxConc)
	}

	var wg sync.WaitGroup
	for _, c := range snap {
		if pred != nil && !pred(c) {
			continue
		}
		wg.Add(1)
		if sema != nil {
			sema <- struct{}{}
		}
		go func(c *Conn) {
			defer wg.Done()
			if sema != nil {
				defer func() { <-sema }()
			}
			// Recover from panics in c.WritePreparedMessage or in the
			// user's OnSlowConn callback. Without this, one bad
			// callback brings down the entire process — every other
			// in-flight broadcast goroutine takes the panic with it.
			defer func() {
				if r := recover(); r != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("panic in dispatch goroutine: %v", r)
					}
					mu.Unlock()
				}
			}()
			err := c.WritePreparedMessage(pm)
			if err == nil {
				delivered.Add(1)
				return
			}
			policy := HubPolicyClose
			if h.cfg.OnSlowConn != nil {
				policy = h.cfg.OnSlowConn(c, err)
			}
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			switch policy {
			case HubPolicyDrop:
				// keep registered; skip this delivery
			case HubPolicyRemove:
				toRemove = append(toRemove, c)
			case HubPolicyClose:
				toClose = append(toClose, c)
			}
			mu.Unlock()
		}(c)
	}
	wg.Wait()

	for _, c := range toRemove {
		h.unregister(c)
	}
	for _, c := range toClose {
		h.unregister(c)
		_ = c.Close()
	}
	return int(delivered.Load()), firstErr
}

func (h *Hub) unregister(c *Conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// Close unregisters every Conn (applying [HubPolicyClose] to each) and
// blocks subsequent Register calls. Idempotent.
//
// Ordering guarantee: any in-flight Broadcast / BroadcastPrepared /
// BroadcastFilter that already snapshotted the conn set runs to
// completion before Close returns. Subsequent broadcasts return
// (delivered=0, err=nil) without dispatching. This makes Close safe to
// call from a shutdown path that needs to know "no more wire writes
// will happen after this returns".
//
// Concurrency: per-Conn Close calls fan out under the same
// [HubConfig.MaxConcurrency] cap that gates Broadcast (default
// [DefaultHubConcurrency], i.e. GOMAXPROCS*4; negative opts out). The
// semaphore is acquired INSIDE each goroutine, so a hung Conn.Close
// cannot deadlock the fan-out — Close still returns once every per-
// Conn Close returns.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	conns := make([]*Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.conns = map[*Conn]struct{}{}
	h.mu.Unlock()
	// Wait for any broadcast that already snapshotted the conn set to
	// finish dispatching before we close the conns out from under it.
	h.inflight.Wait()
	// Fan out the per-conn Close calls. With the same MaxConcurrency
	// budget as Broadcast (default unbounded), 10k conns close in
	// parallel rather than 10k sequential calls.
	//
	// Sema acquire is INSIDE the goroutine — unlike dispatch — because
	// Conn.Close has no inherent timeout. If a Conn.Close hung, an
	// outside-goroutine sema acquire on a low MaxConcurrency would
	// freeze the loop and Hub.Close would never return. Inside-the-
	// goroutine sema acquire trades a small goroutine-burst (up to
	// len(conns) goroutines blocked on sema) for the deadlock
	// guarantee — Hub.Close always returns once every Close returns.
	var sema chan struct{}
	maxConc := h.cfg.MaxConcurrency
	if maxConc == 0 {
		maxConc = DefaultHubConcurrency()
	}
	if maxConc > 0 {
		sema = make(chan struct{}, maxConc)
	}
	var wg sync.WaitGroup
	wg.Add(len(conns))
	for _, c := range conns {
		go func(c *Conn) {
			defer wg.Done()
			if sema != nil {
				sema <- struct{}{}
				defer func() { <-sema }()
			}
			_ = c.Close()
		}(c)
	}
	wg.Wait()
}
