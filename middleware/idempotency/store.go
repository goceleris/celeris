package idempotency

import (
	"github.com/goceleris/celeris/middleware/store"
)

// MemoryStoreConfig configures [NewMemoryStore]. It is the same type as
// [store.MemoryKVConfig], and either name may be used.
type MemoryStoreConfig = store.MemoryKVConfig

// NewMemoryStore returns an in-memory idempotency store. It wraps
// [store.MemoryKV], which implements both [store.KV] and
// [store.SetNXer] — the minimum surface [New] needs.
func NewMemoryStore(config ...MemoryStoreConfig) KVStore {
	return store.NewMemoryKV(config...)
}
