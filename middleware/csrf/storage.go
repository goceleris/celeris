package csrf

import (
	"github.com/goceleris/celeris/middleware/store"
)

// NewMemoryStorage returns an in-memory [store.KV] for [Config].Storage,
// backed by [store.MemoryKV]. CSRF tokens (hex strings) are stored as their
// UTF-8 bytes. The returned *store.MemoryKV implements all optional store
// extensions, including the atomic [store.GetAndDeleter] that
// SingleUseToken relies on.
//
// This is a convenience constructor equivalent to [store.NewMemoryKV].
func NewMemoryStorage(config ...store.MemoryKVConfig) *store.MemoryKV {
	return store.NewMemoryKV(config...)
}
