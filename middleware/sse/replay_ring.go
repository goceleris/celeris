package sse

import (
	"context"
	"strconv"
	"sync"
)

// NewRingBuffer constructs an in-memory [ReplayStore] retaining the last
// `size` appended events. Append is O(1); Since is O(retained-after-lastID).
// IDs are sequential decimal strings starting at 1, so a numerically
// larger ID is always more recent (within a single ring instance). Each
// entry holds a copy of the appended event's strings. In queued mode the ring
// keeps the copy Send made, with no second copy, also when another store wraps
// it and forwards the event as it got it (a decorator that changes a string of
// the event gets the ring's own copy of it).
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

// Append keeps a copy of e's strings: the ring holds the entry until it is
// overwritten, and the caller's strings can be another request's views
// (see [Client.Send]). e.ID is replaced by the ring's own ID, so it is not
// copied.
//
// An event the queue already copied is kept as it is: a ReplayStore that wraps
// the ring is handed that event by the drain and forwards it with its context
// (see ownedCtx; celeris#888 item 24).
func (r *ringStore) Append(ctx context.Context, e Event) (string, error) {
	e.ID = ""
	if oc, ok := ctx.(*ownedCtx); ok && oc.owns(&e) {
		return r.appendOwned(e), nil
	}
	return r.appendOwned(ownEvent(e)), nil
}

// appendOwned appends e as it is, for an event whose strings are already
// the ring's to keep: Append's copy, or a queued event, which sendQueued
// copied (ownEvent) before it queued it. Never pass it a caller's event:
// that precondition is what keeps the ring from holding request strings.
// It returns the ID it assigned.
func (r *ringStore) appendOwned(e Event) string {
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
	return id
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
