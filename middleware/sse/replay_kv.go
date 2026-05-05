package sse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/middleware/store"
)

// MaxKVReplayIndex is the default soft cap on the in-memory ID index a
// [NewKVReplayStore] retains. Once reached, the oldest 25 % of entries
// are dropped; the underlying KV blobs age out via ttl independently.
// Set [KVReplayStoreConfig.MaxIndex] to override.
const MaxKVReplayIndex = 65536

// KVReplayStoreConfig tunes [NewKVReplayStoreWithConfig].
type KVReplayStoreConfig struct {
	// MaxIndex bounds the in-memory ID index. Zero or negative ⇒
	// [MaxKVReplayIndex].
	MaxIndex int

	// AsyncAppend, when true, makes [ReplayStore.Append] return as
	// soon as the local ID has been allocated — the actual KV.Set
	// fires in a background goroutine. Trades durability for latency:
	// an Append() that returns successfully is NOT guaranteed to be
	// in the KV when the next reconnect happens. Use only when wire-
	// write latency dominates and replay can tolerate eventual
	// consistency.
	AsyncAppend bool

	// CounterKey is the KV key under which the cross-instance counter
	// lives. Zero value ⇒ "<prefix>seq". Only consulted when the
	// supplied [store.KV] also implements [store.Counter]; otherwise
	// the constructor falls back to a per-process counter.
	CounterKey string
}

// NewKVReplayStore backs the replay log with a [store.KV] for durability
// across restarts and (with a shared backend) across processes. Events
// live at "<prefix>events/<id>"; ttl bounds retention. IDs are the same
// monotonic decimal strings the ring buffer emits, generated locally
// from an in-process counter.
//
// Multi-instance caveat: each process maintains its own ID counter, so
// ID collisions are possible across instances. For strict cross-instance
// monotonicity use a backend that exposes a counter (e.g. Redis INCR via
// a custom adapter); this default implementation is best-effort.
//
// ttl == 0 stores events without expiry (the underlying KV decides what
// "no expiry" means). Returns an error when kv is nil.
func NewKVReplayStore(kv store.KV, prefix string, ttl time.Duration) (ReplayStore, error) {
	return NewKVReplayStoreWithConfig(kv, prefix, ttl, KVReplayStoreConfig{})
}

// NewKVReplayStoreWithConfig is the explicit-tuning constructor; the
// zero-value [KVReplayStoreConfig] reproduces [NewKVReplayStore]'s
// defaults.
//
// When the supplied [store.KV] also implements [store.Counter] (e.g.
// the Redis adapter via INCR), IDs are allocated atomically against
// that backend — multiple processes sharing the KV will see a single
// monotonic ID space. When the KV does not implement Counter, the
// store falls back to a per-process counter; multi-instance setups
// will see ID collisions across instances.
func NewKVReplayStoreWithConfig(kv store.KV, prefix string, ttl time.Duration, cfg KVReplayStoreConfig) (ReplayStore, error) {
	if kv == nil {
		return nil, errors.New("sse: NewKVReplayStore requires a non-nil store.KV")
	}
	maxIdx := cfg.MaxIndex
	if maxIdx <= 0 {
		maxIdx = MaxKVReplayIndex
	}
	counterKey := cfg.CounterKey
	if counterKey == "" {
		counterKey = prefix + "seq"
	}
	s := &kvStore{
		kv:          kv,
		prefix:      prefix,
		ttl:         ttl,
		maxIndex:    maxIdx,
		asyncAppend: cfg.AsyncAppend,
		counterKey:  counterKey,
	}
	if c, ok := kv.(store.Counter); ok {
		s.counter = c
	}
	return s, nil
}

type kvStore struct {
	kv          store.KV
	counter     store.Counter // nil ⇒ fall back to localSeq
	prefix      string
	counterKey  string
	ttl         time.Duration
	maxIndex    int
	asyncAppend bool
	localSeq    atomic.Uint64

	indexMu sync.Mutex
	index   []uint64 // ordered seqs the local instance has appended
}

func (s *kvStore) eventKey(id string) string {
	return s.prefix + "events/" + id
}

func (s *kvStore) nextID(ctx context.Context) (uint64, string, error) {
	if s.counter != nil {
		n, err := s.counter.Increment(ctx, s.counterKey, s.ttl)
		if err != nil {
			return 0, "", fmt.Errorf("kv counter increment: %w", err)
		}
		if n <= 0 {
			return 0, "", fmt.Errorf("kv counter returned non-positive value: %d", n)
		}
		return uint64(n), strconv.FormatInt(n, 10), nil
	}
	seq := s.localSeq.Add(1)
	return seq, strconv.FormatUint(seq, 10), nil
}

func (s *kvStore) Append(ctx context.Context, e Event) (string, error) {
	seq, id, err := s.nextID(ctx)
	if err != nil {
		return "", err
	}
	e.ID = id
	blob, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	if s.asyncAppend {
		// Fire-and-log the KV write. Caller gets the id immediately;
		// the event reaches the store after wire delivery (or never,
		// if the KV rejects it). Suitable for low-latency publishing
		// where a small replay-store gap during a backend hiccup is
		// preferable to gating Send on backend latency.
		go func() {
			_ = s.kv.Set(context.Background(), s.eventKey(id), blob, s.ttl)
		}()
	} else if err := s.kv.Set(ctx, s.eventKey(id), blob, s.ttl); err != nil {
		return "", err
	}
	s.indexMu.Lock()
	s.index = append(s.index, seq)
	// Soft cap on the in-memory index: when full, drop the oldest 25 %.
	// The blobs themselves age out via ttl in the KV; what we shed here
	// is only the local cursor list, so a Since() against an aged-out
	// cursor returns ErrLastIDUnknown — the documented behaviour.
	if len(s.index) > s.maxIndex {
		drop := s.maxIndex / 4
		if drop < 1 {
			drop = 1
		}
		s.index = append(s.index[:0], s.index[drop:]...)
	}
	s.indexMu.Unlock()
	return id, nil
}

func (s *kvStore) Since(ctx context.Context, lastID string) ([]Event, error) {
	var lastSeq uint64
	if lastID != "" {
		n, err := strconv.ParseUint(lastID, 10, 64)
		if err != nil {
			return nil, ErrLastIDUnknown
		}
		lastSeq = n
	}

	s.indexMu.Lock()
	snap := append([]uint64(nil), s.index...)
	s.indexMu.Unlock()

	if len(snap) == 0 {
		if lastID != "" {
			return nil, ErrLastIDUnknown
		}
		return nil, nil
	}

	// Cursor older than oldest known seq — gap in the local instance's
	// retention. Multi-instance setups should treat this as "fresh
	// start"; the middleware does so via ErrLastIDUnknown.
	if lastID != "" && lastSeq+1 < snap[0] {
		return nil, ErrLastIDUnknown
	}

	// Snapshot is monotonic (each Append appends in order). Find the
	// first seq strictly greater than lastSeq.
	startIdx := 0
	for startIdx < len(snap) && snap[startIdx] <= lastSeq {
		startIdx++
	}

	out := make([]Event, 0, len(snap)-startIdx)
	for i := startIdx; i < len(snap); i++ {
		id := strconv.FormatUint(snap[i], 10)
		blob, err := s.kv.Get(ctx, s.eventKey(id))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // event aged out, skip
			}
			return out, err
		}
		var e Event
		if err := json.Unmarshal(blob, &e); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, nil
}
