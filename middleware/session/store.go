package session

import (
	"github.com/goceleris/celeris/middleware/store"
)

// Store is a type alias for [store.KV] — the unified byte-level
// key-value interface shared across celeris middleware. Session data
// (map[string]any) is JSON-encoded when persisted through Store.
//
// Deprecated: use [store.KV] directly. This alias is retained for
// source compatibility with v1.3.x and will be removed in v2.0.0.
type Store = store.KV

// MemoryStoreConfig is a type alias for [store.MemoryKVConfig].
//
// Deprecated: use [store.MemoryKVConfig].
type MemoryStoreConfig = store.MemoryKVConfig

// NewMemoryStore returns an in-memory store backed by [store.MemoryKV].
// The returned *store.MemoryKV implements [Store] plus all optional
// store extensions (GetAndDeleter, Scanner, PrefixDeleter, SetNXer),
// so callers may type-assert for additional capabilities.
//
// This is a convenience constructor equivalent to [store.NewMemoryKV].
func NewMemoryStore(config ...MemoryStoreConfig) *store.MemoryKV {
	return store.NewMemoryKV(config...)
}
