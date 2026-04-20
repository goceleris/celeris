package session_test

// deprecation_shim_test.go validates that user code written against the
// deprecated session.Store / session.NewMemoryStore / session.MemoryStoreConfig
// names continues to compile and run under v1.5.0 without any source change
// beyond the signature migration documented in CHANGELOG.md § Migration.
//
// This is a smoke test for the deprecation alias strategy — it does not
// cover the old v1.4.x method signatures (Get(sid string)...), which are a
// breaking change called out in the CHANGELOG.

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/middleware/session"
	"github.com/goceleris/celeris/middleware/store"
)

func TestDeprecatedStoreAliasRoundTrip(t *testing.T) {
	// session.Store is a type alias for store.KV.
	var s session.Store = session.NewMemoryStore(session.MemoryStoreConfig{Shards: 2})
	defer func() {
		if closer, ok := s.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	ctx := context.Background()
	if err := s.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v" {
		t.Fatalf("Get: got %q, want %q", got, "v")
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestDeprecatedAliasIsExactlyKV(t *testing.T) {
	// session.Store must be assignable to and from store.KV without any
	// conversion — that's the whole point of the type-alias approach.
	// The explicit type declarations below are intentional (testing the
	// alias surface), so staticcheck's ST1023 warning is silenced.
	var kv store.KV = session.NewMemoryStore()
	var s session.Store = kv //nolint:staticcheck // ST1023: explicit type tests alias
	_ = s
	var back store.KV = s //nolint:staticcheck // ST1023
	_ = back
}

func TestDeprecatedMemoryStoreConfigAlias(t *testing.T) {
	// MemoryStoreConfig is a type alias for store.MemoryKVConfig — the
	// fields should be usable under either name.
	var cfg session.MemoryStoreConfig = store.MemoryKVConfig{Shards: 4} //nolint:staticcheck // ST1023: explicit type tests alias
	s := session.NewMemoryStore(cfg)
	defer s.Close()
}
