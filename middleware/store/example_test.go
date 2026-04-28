package store_test

import (
	"context"
	"fmt"
	"time"

	"github.com/goceleris/celeris/middleware/store"
)

// ExampleNewMemoryKV — the in-memory sharded LRU is the default
// backing store for [middleware/session], [middleware/csrf],
// [middleware/idempotency] and [middleware/cache]. It implements every
// optional extension (Scanner, PrefixDeleter, GetAndDeleter, SetNXer)
// so any middleware that requires one will accept it.
func ExampleNewMemoryKV() {
	kv := store.NewMemoryKV()
	defer kv.Close()

	ctx := context.Background()
	_ = kv.Set(ctx, "session:abc", []byte("payload"), 30*time.Minute)
	v, _ := kv.Get(ctx, "session:abc")
	fmt.Println(string(v))
	// Output: payload
}

// ExamplePrefixed wraps any [store.KV] with a key namespace. Useful
// when several middlewares share one Redis instance — give each one
// its own prefix so keys never collide.
func ExamplePrefixed() {
	kv := store.NewMemoryKV()
	defer kv.Close()

	sessions := store.Prefixed(kv, "sess:")
	_ = sessions.Set(context.Background(), "abc", []byte("hi"), time.Minute)

	// Underlying key is "sess:abc".
	raw, _ := kv.Get(context.Background(), "sess:abc")
	fmt.Println(string(raw))
	// Output: hi
}
