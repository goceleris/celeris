package sse

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/middleware/store"
)

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
// "no expiry" means).
func NewKVReplayStore(kv store.KV, prefix string, ttl time.Duration) ReplayStore {
	if kv == nil {
		panic("sse: NewKVReplayStore requires a non-nil store.KV")
	}
	return &kvStore{
		kv:     kv,
		prefix: prefix,
		ttl:    ttl,
	}
}

type kvStore struct {
	kv      store.KV
	prefix  string
	ttl     time.Duration
	nextSeq atomic.Uint64

	indexMu sync.Mutex
	index   []uint64 // ordered seqs the local instance has appended
}

func (s *kvStore) eventKey(id string) string {
	return s.prefix + "events/" + id
}

func (s *kvStore) Append(ctx context.Context, e Event) (string, error) {
	seq := s.nextSeq.Add(1)
	id := strconv.FormatUint(seq, 10)
	e.ID = id
	blob, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	if err := s.kv.Set(ctx, s.eventKey(id), blob, s.ttl); err != nil {
		return "", err
	}
	s.indexMu.Lock()
	s.index = append(s.index, seq)
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
