package cache

import (
	"context"
	"testing"
	"time"
)

func BenchmarkMemoryStoreSetFresh(b *testing.B) {
	s := NewMemoryStore()
	defer s.Close()
	ctx := context.Background()
	v := []byte("hello world")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		_ = s.Set(ctx, "k", v, time.Minute)
		_ = s.Delete(ctx, "k")
	}
}

// BenchmarkMemoryStoreSetReplace exercises the value-update path where
// the same key gets re-set with a same-sized payload. The backing array
// can be reused — no heap alloc per update.
func BenchmarkMemoryStoreSetReplace(b *testing.B) {
	s := NewMemoryStore()
	defer s.Close()
	ctx := context.Background()
	v := []byte("hello world")
	_ = s.Set(ctx, "k", v, time.Minute)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = s.Set(ctx, "k", v, time.Minute)
	}
}
