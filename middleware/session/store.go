package session

import (
	"github.com/goceleris/celeris/middleware/store"
)

// NewMemoryStore returns an in-memory [store.KV] for [Config].Store, backed
// by [store.MemoryKV]. Session data (map[string]any) is JSON-encoded when
// persisted. The returned *store.MemoryKV implements every optional store
// extension (GetAndDeleter, Scanner, PrefixDeleter, SetNXer), so callers may
// type-assert for additional capabilities.
//
// This is a convenience constructor equivalent to [store.NewMemoryKV].
func NewMemoryStore(config ...store.MemoryKVConfig) *store.MemoryKV {
	return store.NewMemoryKV(config...)
}
