package csrf_test

// deprecation_shim_test.go validates that user code written against the
// deprecated csrf.Storage / csrf.NewMemoryStorage / csrf.MemoryStorageConfig
// names continues to compile and run under v1.5.0.

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/middleware/csrf"
	"github.com/goceleris/celeris/middleware/store"
)

func TestDeprecatedStorageAliasRoundTrip(t *testing.T) {
	var s csrf.Storage = csrf.NewMemoryStorage(csrf.MemoryStorageConfig{Shards: 2})
	defer func() {
		if closer, ok := s.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	ctx := context.Background()
	if err := s.Set(ctx, "csrf-token-abc", []byte("value"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, "csrf-token-abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "value" {
		t.Fatalf("Get: got %q, want %q", got, "value")
	}
	// SingleUseToken path relies on GetAndDeleter — the MemoryKV the
	// alias returns must implement it.
	gad, ok := s.(store.GetAndDeleter)
	if !ok {
		t.Fatalf("NewMemoryStorage must return a GetAndDeleter")
	}
	v, err := gad.GetAndDelete(ctx, "csrf-token-abc")
	if err != nil {
		t.Fatalf("GetAndDelete: %v", err)
	}
	if string(v) != "value" {
		t.Fatalf("GetAndDelete: got %q", v)
	}
	if _, err := s.Get(ctx, "csrf-token-abc"); err == nil {
		t.Fatal("expected ErrNotFound after GetAndDelete")
	}
}

func TestDeprecatedAliasIsExactlyKV(t *testing.T) {
	var kv store.KV = csrf.NewMemoryStorage()
	var s csrf.Storage = kv
	_ = s
	var back store.KV = s
	_ = back
}
