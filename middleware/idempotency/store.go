package idempotency

import (
	"github.com/goceleris/celeris/middleware/store"
)

// MemoryStoreConfig is a type alias for [store.MemoryKVConfig] provided
// for symmetry with other middleware packages.
type MemoryStoreConfig = store.MemoryKVConfig

// NewMemoryStore returns an in-memory idempotency store. It wraps
// [store.MemoryKV], which implements both [store.KV] and
// [store.SetNXer] — the minimum surface [New] needs.
func NewMemoryStore(config ...MemoryStoreConfig) KVStore {
	return store.NewMemoryKV(config...)
}
