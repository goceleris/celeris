package websocket

import "sync"

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
}

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
	snap := h.snapshot()
	return h.dispatch(snap, pm, nil)
}

// BroadcastFilter sends only to Conns where pred returns true. Common
// for room / channel routing — filter on Conn.Locals without building a
// second Hub. The scan happens under the read lock, so a parallel
// Register does not serialise.
func (h *Hub) BroadcastFilter(messageType MessageType, data []byte, pred func(*Conn) bool) (int, error) {
	if pred == nil {
		return h.Broadcast(messageType, data)
	}
	pm, err := NewPreparedMessage(messageType, data)
	if err != nil {
		return 0, err
	}
	snap := h.snapshot()
	return h.dispatch(snap, pm, pred)
}

func (h *Hub) snapshot() []*Conn {
	h.mu.RLock()
	out := make([]*Conn, 0, len(h.conns))
	for c := range h.conns {
		out = append(out, c)
	}
	h.mu.RUnlock()
	return out
}

// dispatch performs the per-Conn write for snap. Removals/closes
// triggered by OnSlowConn are deferred until after the broadcast so the
// snapshot we are iterating is not mutated mid-loop.
func (h *Hub) dispatch(snap []*Conn, pm *PreparedMessage, pred func(*Conn) bool) (int, error) {
	delivered := 0
	var firstErr error
	var toRemove []*Conn
	var toClose []*Conn

	for _, c := range snap {
		if pred != nil && !pred(c) {
			continue
		}
		if err := c.WritePreparedMessage(pm); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			policy := HubPolicyClose
			if h.cfg.OnSlowConn != nil {
				policy = h.cfg.OnSlowConn(c, err)
			}
			switch policy {
			case HubPolicyDrop:
				// keep registered; skip this delivery
			case HubPolicyRemove:
				toRemove = append(toRemove, c)
			case HubPolicyClose:
				toClose = append(toClose, c)
			}
			continue
		}
		delivered++
	}

	for _, c := range toRemove {
		h.unregister(c)
	}
	for _, c := range toClose {
		h.unregister(c)
		_ = c.Close()
	}
	return delivered, firstErr
}

func (h *Hub) unregister(c *Conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// Close unregisters every Conn (applying [HubPolicyClose] to each) and
// blocks subsequent Register calls. Idempotent. After Close returns the
// Hub is empty and broadcasts deliver to zero conns.
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
	for _, c := range conns {
		_ = c.Close()
	}
}
