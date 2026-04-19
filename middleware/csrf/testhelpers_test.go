package csrf

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goceleris/celeris/middleware/store"
)

// storeSet writes value as the UTF-8 bytes of token under key. Helper
// for csrf tests that previously called the old Storage.Set (sync).
func storeSet(tb testing.TB, kv store.KV, key, token string, ttl time.Duration) {
	tb.Helper()
	if err := kv.Set(context.Background(), key, []byte(token), ttl); err != nil {
		tb.Fatalf("store.Set %q: %v", key, err)
	}
}

// storeGet reads key and returns (token, true) on hit or ("", false)
// on miss. Maps [store.ErrNotFound] to a bool for the old test shape.
func storeGet(tb testing.TB, kv store.KV, key string) (string, bool) {
	tb.Helper()
	raw, err := kv.Get(context.Background(), key)
	if errors.Is(err, store.ErrNotFound) {
		return "", false
	}
	if err != nil {
		tb.Fatalf("store.Get %q: %v", key, err)
	}
	return string(raw), true
}

// storeGetAndDelete wraps [store.GetAndDeleter] (or a non-atomic
// Get+Delete fallback) for csrf single-use token tests.
func storeGetAndDelete(tb testing.TB, kv store.KV, key string) (string, bool, error) {
	tb.Helper()
	if gd, ok := kv.(store.GetAndDeleter); ok {
		raw, err := gd.GetAndDelete(context.Background(), key)
		if errors.Is(err, store.ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return string(raw), true, nil
	}
	raw, err := kv.Get(context.Background(), key)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	_ = kv.Delete(context.Background(), key)
	return string(raw), true, nil
}
