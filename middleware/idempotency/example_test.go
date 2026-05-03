package idempotency_test

import (
	"time"

	"github.com/goceleris/celeris/middleware/idempotency"
	"github.com/goceleris/celeris/middleware/store"
)

// ExampleConfig — minimum useful configuration. Clients send
// `Idempotency-Key: <uuid>` on POST/PUT/DELETE; retries within the
// TTL replay the cached response and concurrent duplicates while the
// original is still in-flight return 409 Conflict. `store.MemoryKV`
// satisfies [idempotency.KVStore] (KV + SetNXer) out of the box.
func ExampleConfig() {
	_ = idempotency.Config{
		Store: store.NewMemoryKV(),
		TTL:   5 * time.Minute,
	}
}
