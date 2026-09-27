//line hub.go:1:1
package websocket; import _cover_atomic_ "sync/atomic"

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
func DefaultHubConcurrency() int {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[0], 1); return runtime.GOMAXPROCS(0) * 4 }

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
func NewHub(cfg HubConfig) *Hub {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[1], 1);
	return &Hub{
		cfg:   cfg,
		conns: make(map[*Conn]struct{}),
	}
}

// Register adds c to the Hub. Returns an unregister function the caller
// MUST defer; calling it twice is safe. Registering a Conn on a Hub
// that has been Close()'d is a no-op — the returned unregister is also
// a no-op.
func (h *Hub) Register(c *Conn) func() {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[2], 1);
	h.mu.Lock()
	if h.closed {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[4], 1);
		h.mu.Unlock()
		return func() {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[5], 1);}
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[3], 1);h.conns[c] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return func() {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[6], 1);
		once.Do(func() {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[7], 1);
			h.mu.Lock()
			delete(h.conns, c)
			h.mu.Unlock()
		})
	}
}

// Len reports the current number of registered Conns.
func (h *Hub) Len() int {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[8], 1);
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
func (h *Hub) Broadcast(messageType MessageType, data []byte) (delivered int, err error) {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[9], 1);
	pm, err := NewPreparedMessage(messageType, data)
	if err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[11], 1);
		return 0, err
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[10], 1);return h.BroadcastPrepared(pm)
}

// BroadcastPrepared dispatches an already-prepared message — useful in
// dispatch loops where the same payload is published repeatedly.
func (h *Hub) BroadcastPrepared(pm *PreparedMessage) (int, error) {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[12], 1);
	snap, ok := h.snapshot()
	if !ok {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[14], 1);
		return 0, nil
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[13], 1);defer h.inflight.Done()
	return h.dispatch(snap, pm, nil)
}

// BroadcastFilter sends only to Conns where pred returns true. Common
// for room / channel routing — filter on Conn.Locals without building a
// second Hub.
//
// The membership snapshot happens under the Hub's read lock; pred is
// invoked LOCK-FREE against that snapshot. Parallel Register / unregister
// calls during dispatch are not observed by this broadcast.
func (h *Hub) BroadcastFilter(messageType MessageType, data []byte, pred func(*Conn) bool) (int, error) {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[15], 1);
	if pred == nil {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[19], 1);
		return h.Broadcast(messageType, data)
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[16], 1);pm, err := NewPreparedMessage(messageType, data)
	if err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[20], 1);
		return 0, err
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[17], 1);snap, ok := h.snapshot()
	if !ok {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[21], 1);
		return 0, nil
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[18], 1);defer h.inflight.Done()
	return h.dispatch(snap, pm, pred)
}

// snapshot copies the current conn set under RLock and registers an
// inflight broadcast with the Hub's WaitGroup. Returns ok=false if the
// Hub is already closed (so the caller skips dispatch and the WaitGroup
// is NOT incremented).
func (h *Hub) snapshot() ([]*Conn, bool) {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[22], 1);
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[25], 1);
		return nil, false
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[23], 1);h.inflight.Add(1)
	out := make([]*Conn, 0, len(h.conns))
	for c := range h.conns {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[26], 1);
		out = append(out, c)
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[24], 1);return out, true
}

// dispatch performs the per-Conn write for snap. Each conn's write
// runs in its own goroutine so a slow conn cannot gate the rest;
// MaxConcurrency, if positive, caps goroutine pressure via a semaphore.
// Removals/closes triggered by OnSlowConn are deferred until after the
// broadcast so the snapshot we are iterating is not mutated mid-loop.
func (h *Hub) dispatch(snap []*Conn, pm *PreparedMessage, pred func(*Conn) bool) (int, error) {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[27], 1);
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
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[28], 1);var sema chan struct{}
	maxConc := h.cfg.MaxConcurrency
	if maxConc == 0 {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[35], 1);
		maxConc = DefaultHubConcurrency()
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[29], 1);if maxConc > 0 {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[36], 1);
		sema = make(chan struct{}, maxConc)
	}

	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[30], 1);var wg sync.WaitGroup
	for _, c := range snap {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[37], 1);
		if pred != nil && !pred(c) {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[40], 1);
			continue
		}
		_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[38], 1);wg.Add(1)
		if sema != nil {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[41], 1);
			sema <- struct{}{}
		}
		_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[39], 1);go func(c *Conn) {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[42], 1);
			defer wg.Done()
			if sema != nil {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[49], 1);
				defer func() {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[50], 1); <-sema }()
			}
			// Recover from panics in c.WritePreparedMessage or in the
			// user's OnSlowConn callback. Without this, one bad
			// callback brings down the entire process — every other
			// in-flight broadcast goroutine takes the panic with it.
			_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[43], 1);defer func() {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[51], 1);
				if r := recover(); r != nil {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[52], 1);
					mu.Lock()
					if firstErr == nil {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[54], 1);
						firstErr = fmt.Errorf("panic in dispatch goroutine: %v", r)
					}
					// Treat a panicking conn as Close-policy: a Conn
					// or callback that panics on this Broadcast is
					// likely to panic on the next, and leaving it
					// registered would amplify the failure. The
					// recover already swallowed the panic so the
					// goroutine exits cleanly via wg.Done.
					_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[53], 1);toClose = append(toClose, c)
					mu.Unlock()
				}
			}()
			_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[44], 1);err := c.WritePreparedMessage(pm)
			if err == nil {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[55], 1);
				delivered.Add(1)
				return
			}
			_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[45], 1);policy := HubPolicyClose
			if h.cfg.OnSlowConn != nil {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[56], 1);
				policy = h.cfg.OnSlowConn(c, err)
			}
			_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[46], 1);mu.Lock()
			if firstErr == nil {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[57], 1);
				firstErr = err
			}
			_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[47], 1);switch policy {
			case HubPolicyDrop:_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[58], 1);
				// keep registered; skip this delivery
			case HubPolicyRemove:_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[59], 1);
				toRemove = append(toRemove, c)
			case HubPolicyClose:_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[60], 1);
				toClose = append(toClose, c)
			}
			_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[48], 1);mu.Unlock()
		}(c)
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[31], 1);wg.Wait()

	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[32], 1);for _, c := range toRemove {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[61], 1);
		h.unregister(c)
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[33], 1);for _, c := range toClose {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[62], 1);
		h.unregister(c)
		_ = c.Close()
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[34], 1);return int(delivered.Load()), firstErr
}

func (h *Hub) unregister(c *Conn) {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[63], 1);
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// Close unregisters every Conn and force-closes the underlying
// connection (via [Conn.Close], NOT via [HubConfig.OnSlowConn] —
// shutdown is unconditional). Blocks subsequent Register calls.
// Idempotent.
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
// [DefaultHubConcurrency], i.e. GOMAXPROCS*4; negative opts out).
// The semaphore is acquired INSIDE each goroutine, so the fan-out
// always spawns N goroutines that then queue on the sema. The
// alternative — outside-goroutine acquire — would gate spawn on the
// slowest Close, costing the same wall-clock for the *common* case
// (every Close returns) but letting a single hung Conn.Close stall
// the entire spawn loop. Inside-acquire trades a goroutine-burst
// (up to len(conns) parked on the sema) for resilience to a hung
// Close — Hub.Close still returns once every per-Conn Close returns,
// without serialising the rest of the fan-out behind it.
func (h *Hub) Close() {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[64], 1);
	h.mu.Lock()
	if h.closed {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[72], 1);
		h.mu.Unlock()
		return
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[65], 1);h.closed = true
	conns := make([]*Conn, 0, len(h.conns))
	for c := range h.conns {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[73], 1);
		conns = append(conns, c)
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[66], 1);h.conns = map[*Conn]struct{}{}
	h.mu.Unlock()
	// Wait for any broadcast that already snapshotted the conn set to
	// finish dispatching before we close the conns out from under it.
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[67], 1);h.inflight.Wait()
	// Fan out the per-conn Close calls. With the same MaxConcurrency
	// budget as Broadcast (default GOMAXPROCS*4; negative opts out),
	// 10k conns close in parallel rather than 10k sequential calls.
	//
	// Sema acquire is INSIDE the goroutine — unlike dispatch — because
	// Conn.Close has no inherent timeout. If a Conn.Close hung, an
	// outside-goroutine sema acquire on a low MaxConcurrency would
	// freeze the loop and Hub.Close would never return. Inside-the-
	// goroutine sema acquire trades a small goroutine-burst (up to
	// len(conns) goroutines blocked on sema) for the deadlock
	// guarantee — Hub.Close always returns once every Close returns.
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[68], 1);var sema chan struct{}
	maxConc := h.cfg.MaxConcurrency
	if maxConc == 0 {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[74], 1);
		maxConc = DefaultHubConcurrency()
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[69], 1);if maxConc > 0 {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[75], 1);
		sema = make(chan struct{}, maxConc)
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[70], 1);var wg sync.WaitGroup
	wg.Add(len(conns))
	for _, c := range conns {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[76], 1);
		go func(c *Conn) {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[77], 1);
			defer wg.Done()
			if sema != nil {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[79], 1);
				sema <- struct{}{}
				defer func() {_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[80], 1); <-sema }()
			}
			_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[78], 1);_ = c.Close()
		}(c)
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_hub.Count[71], 1);wg.Wait()
}

var GoCover_b2b_hub = struct {
	Count     [81]uint32
	Pos       [3 * 81]uint32
	NumStmt   [81]uint16
} {
	Pos: [3 * 81]uint32{
		54, 54, 0x460024, // [0]
		82, 85, 0x10002, // [1]
		93, 94, 0xe0002, // [2]
		98, 101, 0x100002, // [3]
		95, 96, 0x110003, // [4]
		96, 96, 0x120012, // [5]
		102, 102, 0x120003, // [6]
		103, 106, 0x10004, // [7]
		112, 115, 0x10002, // [8]
		128, 129, 0x100002, // [9]
		132, 132, 0x200002, // [10]
		130, 131, 0x10003, // [11]
		138, 139, 0x90002, // [12]
		142, 143, 0x220002, // [13]
		140, 141, 0x10003, // [14]
		154, 154, 0x110002, // [15]
		157, 158, 0x100002, // [16]
		161, 162, 0x90002, // [17]
		165, 166, 0x230002, // [18]
		155, 156, 0x10003, // [19]
		159, 160, 0x10003, // [20]
		163, 164, 0x10003, // [21]
		174, 176, 0xe0002, // [22]
		179, 181, 0x190002, // [23]
		184, 184, 0x120002, // [24]
		177, 178, 0x10003, // [25]
		182, 183, 0x10003, // [26]
		193, 200, 0x10002, // [27]
		206, 208, 0x120002, // [28]
		211, 211, 0x110002, // [29]
		215, 216, 0x190002, // [30]
		273, 274, 0x10002, // [31]
		275, 275, 0x1d0002, // [32]
		278, 278, 0x1c0002, // [33]
		282, 282, 0x280002, // [34]
		209, 210, 0x10003, // [35]
		212, 213, 0x10003, // [36]
		217, 217, 0x1e0003, // [37]
		220, 221, 0x120003, // [38]
		224, 224, 0x140003, // [39]
		218, 218, 0xc0004, // [40]
		222, 223, 0x10004, // [41]
		225, 226, 0x130004, // [42]
		233, 233, 0x110004, // [43]
		249, 250, 0x120004, // [44]
		254, 255, 0x1f0004, // [45]
		258, 259, 0x170004, // [46]
		262, 262, 0x120004, // [47]
		270, 270, 0xf0004, // [48]
		227, 227, 0x120005, // [49]
		227, 227, 0x1c0014, // [50]
		234, 234, 0x210005, // [51]
		235, 236, 0x190006, // [52]
		245, 246, 0x110006, // [53]
		237, 238, 0x10007, // [54]
		251, 253, 0x10005, // [55]
		256, 257, 0x10005, // [56]
		260, 261, 0x10005, // [57]
		263, 263, 0x170017, // [58]
		266, 266, 0x230005, // [59]
		268, 268, 0x210005, // [60]
		276, 277, 0x10003, // [61]
		279, 281, 0x10003, // [62]
		286, 289, 0x10002, // [63]
		316, 317, 0xe0002, // [64]
		321, 323, 0x190002, // [65]
		326, 328, 0x10002, // [66]
		330, 331, 0x10002, // [67]
		342, 344, 0x120002, // [68]
		347, 347, 0x110002, // [69]
		350, 352, 0x1a0002, // [70]
		362, 362, 0xb0002, // [71]
		318, 320, 0x10003, // [72]
		324, 325, 0x10003, // [73]
		345, 346, 0x10003, // [74]
		348, 349, 0x10003, // [75]
		353, 353, 0x140003, // [76]
		354, 355, 0x130004, // [77]
		359, 359, 0x110004, // [78]
		356, 357, 0x120005, // [79]
		357, 357, 0x1c0014, // [80]
	},
	NumStmt: [81]uint16{
		1, // 0
		1, // 1
		2, // 2
		4, // 3
		2, // 4
		0, // 5
		1, // 6
		3, // 7
		3, // 8
		2, // 9
		1, // 10
		1, // 11
		2, // 12
		2, // 13
		1, // 14
		1, // 15
		2, // 16
		2, // 17
		2, // 18
		1, // 19
		1, // 20
		1, // 21
		3, // 22
		3, // 23
		1, // 24
		1, // 25
		1, // 26
		4, // 27
		4, // 28
		1, // 29
		2, // 30
		2, // 31
		2, // 32
		1, // 33
		1, // 34
		1, // 35
		1, // 36
		1, // 37
		2, // 38
		1, // 39
		1, // 40
		1, // 41
		2, // 42
		1, // 43
		2, // 44
		2, // 45
		2, // 46
		1, // 47
		1, // 48
		1, // 49
		1, // 50
		1, // 51
		2, // 52
		2, // 53
		1, // 54
		2, // 55
		1, // 56
		1, // 57
		0, // 58
		1, // 59
		1, // 60
		1, // 61
		2, // 62
		3, // 63
		2, // 64
		3, // 65
		6, // 66
		6, // 67
		6, // 68
		1, // 69
		3, // 70
		1, // 71
		2, // 72
		1, // 73
		1, // 74
		1, // 75
		1, // 76
		2, // 77
		1, // 78
		2, // 79
		1, // 80
	},
}

var _ = _cover_atomic_.LoadUint32
