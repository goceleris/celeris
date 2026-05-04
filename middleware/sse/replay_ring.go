package sse

import (
	"context"
	"strconv"
	"sync"
)

// NewRingBuffer constructs an in-memory [ReplayStore] retaining the last
// `size` appended events. Append is O(1); Since is O(retained-after-lastID).
// IDs are sequential decimal strings starting at 1, so a numerically
// larger ID is always more recent (within a single ring instance).
//
// `size` is clamped to 1 if non-positive.
func NewRingBuffer(size int) ReplayStore {
	if size <= 0 {
		size = 1
	}
	return &ringStore{
		entries: make([]ringEntry, size),
		cap:     size,
	}
}

type ringEntry struct {
	seq   uint64
	event Event
}

type ringStore struct {
	mu      sync.Mutex
	entries []ringEntry
	head    int // index of the next slot to write
	count   int // number of valid entries (capped at cap)
	cap     int
	nextSeq uint64
}

func (r *ringStore) Append(_ context.Context, e Event) (string, error) {
	r.mu.Lock()
	r.nextSeq++
	seq := r.nextSeq
	id := strconv.FormatUint(seq, 10)
	e.ID = id
	r.entries[r.head] = ringEntry{seq: seq, event: e}
	r.head = (r.head + 1) % r.cap
	if r.count < r.cap {
		r.count++
	}
	r.mu.Unlock()
	return id, nil
}

func (r *ringStore) Since(_ context.Context, lastID string) ([]Event, error) {
	var lastSeq uint64
	if lastID != "" {
		n, err := strconv.ParseUint(lastID, 10, 64)
		if err != nil {
			return nil, ErrLastIDUnknown
		}
		lastSeq = n
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == 0 {
		if lastID != "" {
			return nil, ErrLastIDUnknown
		}
		return nil, nil
	}

	start := (r.head - r.count + r.cap) % r.cap
	oldestSeq := r.entries[start].seq

	// Cursor older than oldest retained entry — caller has fallen out
	// of the retention window. The middleware logs this and falls
	// through to a fresh handler invocation.
	if lastID != "" && lastSeq+1 < oldestSeq {
		return nil, ErrLastIDUnknown
	}

	out := make([]Event, 0, r.count)
	for i := 0; i < r.count; i++ {
		idx := (start + i) % r.cap
		entry := r.entries[idx]
		if entry.seq <= lastSeq {
			continue
		}
		out = append(out, entry.event)
	}
	return out, nil
}
