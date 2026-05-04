package sse

import (
	"context"
	"errors"
)

// ErrLastIDUnknown is returned by [ReplayStore.Since] when the supplied
// lastID is non-empty but the store cannot interpret it (malformed,
// aged out of retention, or never recorded). The middleware treats this
// as "fresh start": the user's Handler still runs, but no replay events
// are written. The original Last-Event-ID header value remains
// observable via [Client.LastEventID].
var ErrLastIDUnknown = errors.New("sse: last-event-id unknown to replay store")

// ReplayStore persists events and serves them back on reconnect.
//
// Implementations:
//   - [NewRingBuffer]: in-memory, fixed-size; bounded memory; lost on
//     process restart.
//   - [NewKVReplayStore]: backed by a [store.KV] (memory / Redis /
//     Postgres / Memcached); durable across restarts when the underlying
//     KV is durable; multi-instance users must take care to share an ID
//     space (e.g. via a Redis SetNXer counter — see implementation notes).
//
// All methods must be safe for concurrent use.
type ReplayStore interface {
	// Append records the event and returns the canonical ID the
	// middleware will emit on the wire. Implementations SHOULD ignore
	// e.ID and assign their own monotonically increasing ID.
	Append(ctx context.Context, e Event) (id string, err error)

	// Since returns every event appended strictly after lastID, in
	// append order. lastID == "" means "no checkpoint" — implementations
	// may return the empty slice or a recent-N tail (default: empty).
	// Returns ErrLastIDUnknown if lastID is non-empty but cannot be
	// interpreted.
	Since(ctx context.Context, lastID string) ([]Event, error)
}
