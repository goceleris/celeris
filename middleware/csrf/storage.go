package csrf

import (
	"github.com/goceleris/celeris/middleware/store"
)

// Storage is a type alias for [store.KV] — the unified byte-level
// key-value interface shared across celeris middleware. CSRF tokens
// (hex strings) are stored as their UTF-8 bytes.
//
// For single-use token semantics (Config.SingleUseToken=true), the
// backend should also implement [store.GetAndDeleter]; backends without
// atomic GET+DEL fall back to a non-atomic Get+Delete pair.
//
// Deprecated: use [store.KV] directly. This alias is retained for
// source compatibility with the pre-unified-store API.
type Storage = store.KV

// MemoryStorageConfig is a type alias for [store.MemoryKVConfig].
//
// Deprecated: use [store.MemoryKVConfig].
type MemoryStorageConfig = store.MemoryKVConfig

// NewMemoryStorage returns an in-memory [Storage] backed by [store.MemoryKV].
// The returned *store.MemoryKV implements all optional store extensions;
// callers needing atomic GetAndDelete for SingleUseToken can rely on the
// built-in GetAndDeleter implementation.
func NewMemoryStorage(config ...MemoryStorageConfig) *store.MemoryKV {
	return store.NewMemoryKV(config...)
}
