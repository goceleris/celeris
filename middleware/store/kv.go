// Package store defines the unified key-value interface that middleware
// stores (session, csrf, cache, idempotency, jwks cache) build on.
//
// The core interface is [KV] — a small, byte-level, context-aware
// Get/Set/Delete surface. Backends implement KV plus any optional
// extensions they can support atomically (GETDEL, SCAN, SETNX, EVALSHA).
// Middleware adapters feature-detect extensions via type assertion.
//
// All implementations must follow the [KV.Get] contract: return
// nil, [ErrNotFound] on a missing key. Returning nil, nil is forbidden.
package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by [KV.Get] and extension methods when the key
// does not exist or has expired. Adapters map their native "missing key"
// signal (redis.ErrNil, sql.ErrNoRows, 0 rows) onto this sentinel.
var ErrNotFound = errors.New("store: key not found")

// KV is the unified byte-level key-value interface for middleware stores.
//
// Contract:
//   - Get returns (nil, ErrNotFound) on a missing key. A (nil, nil) return
//     is forbidden; callers may rely on err != nil when the value is absent.
//   - Set with ttl == 0 stores a value with no expiry. Positive ttl is
//     honored by the backend; sub-second precision is best-effort.
//   - Delete returning nil means "the key is not present" regardless of
//     whether it was present before the call.
//   - All methods are safe for concurrent use from multiple goroutines.
//   - Values returned by Get are owned by the caller; backends must copy
//     any internal buffer before returning.
type KV interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}

// GetAndDeleter is implemented by backends that support atomic GET+DEL
// (e.g., Redis GETDEL). Used by csrf for TOCTOU-safe single-use token
// validation. When a KV does not implement GetAndDeleter, callers fall
// back to a non-atomic Get+Delete pair.
type GetAndDeleter interface {
	GetAndDelete(ctx context.Context, key string) ([]byte, error)
}

// Scanner is implemented by backends that support key enumeration by
// prefix (e.g., Redis SCAN). Used by session Reset to delete all keys
// matching a prefix. When a KV does not implement Scanner, Reset is a
// documented no-op.
type Scanner interface {
	Scan(ctx context.Context, prefix string) ([]string, error)
}

// PrefixDeleter is implemented by backends that can atomically (or
// efficiently) delete all keys matching a prefix. Used by Cache for
// InvalidatePrefix operations. Backends that do not implement this may
// emulate it via Scan + Delete, but with weaker atomicity guarantees.
type PrefixDeleter interface {
	DeletePrefix(ctx context.Context, prefix string) error
}

// SetNXer is implemented by backends that support atomic "set if not
// exists" semantics (e.g., Redis SET NX, Postgres INSERT ON CONFLICT DO
// NOTHING). Used by idempotency for lock acquisition. A backend without
// SetNX cannot serve as an idempotency store.
type SetNXer interface {
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (acquired bool, err error)
}

// Scripter is implemented by backends that support server-side atomic
// scripts (e.g., Redis EVALSHA + ScriptLoad). Used by the ratelimit
// Redis adapter for atomic token-bucket updates via a Lua script.
// Most KV backends do not implement Scripter; it is strictly optional.
type Scripter interface {
	EvalSHA(ctx context.Context, sha string, keys []string, args ...any) (any, error)
	ScriptLoad(ctx context.Context, script string) (string, error)
}

// Counter is implemented by backends that expose an atomic
// monotonically-increasing counter (e.g. Redis INCR, Postgres
// `UPDATE ... RETURNING id`). Used by [middleware/sse.NewKVReplayStore]
// to share a single ID space across processes that hit the same KV
// backend — without a shared counter, multi-instance replay cannot
// guarantee unique IDs across reconnects. When a KV does not implement
// Counter the SSE replay store falls back silently to a per-process
// counter; callers that need cross-instance monotonicity should
// type-assert their KV against this interface at startup.
//
// Increment returns the value AFTER the increment, so a fresh counter
// hands out 1, 2, 3, ... ttl bounds the counter's lifetime in the
// backend; ttl == 0 means "no expiry", same convention as [KV.Set].
// Backends that don't support TTL on counters may ignore it.
type Counter interface {
	Increment(ctx context.Context, key string, ttl time.Duration) (int64, error)
}
