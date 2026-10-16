package idempotency

import (
	"github.com/goceleris/celeris/middleware/store"
)

// MemoryStoreConfig is a type alias for [store.MemoryKVConfig]: the options
// [NewMemoryStore] takes. It is kept so existing callers compile; new code can
// pass a [store.MemoryKVConfig] directly (the two are the same type).
type MemoryStoreConfig = store.MemoryKVConfig

// NewMemoryStore returns an in-memory idempotency store. It wraps
// [store.MemoryKV], which implements both [store.KV] and
// [store.SetNXer] — the minimum surface [New] needs.
func NewMemoryStore(config ...MemoryStoreConfig) KVStore {
	return store.NewMemoryKV(config...)
}
