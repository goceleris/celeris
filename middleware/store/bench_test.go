package store

import (
	"context"
	"testing"
)

func BenchmarkMemoryKVGet(b *testing.B) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()
	_ = m.Set(ctx, "k", []byte("v"), 0)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = m.Get(ctx, "k")
	}
}

func BenchmarkMemoryKVSet(b *testing.B) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()
	v := []byte("v")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = m.Set(ctx, "k", v, 0)
	}
}

func BenchmarkMemoryKVGetAndDelete(b *testing.B) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()
	v := []byte("v")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = m.Set(ctx, "k", v, 0)
		_, _ = m.GetAndDelete(ctx, "k")
	}
}

func BenchmarkMemoryKVSetNXAcquire(b *testing.B) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		_, _ = m.SetNX(ctx, "k", []byte("v"), 0)
		_ = m.Delete(ctx, "k")
	}
}

// BenchmarkMemoryKVSetNXContended covers the failure path where an
// existing non-expired item blocks the SetNX. The idempotency
// middleware's duplicate-key detection hits this repeatedly under
// replay-heavy traffic.
func BenchmarkMemoryKVSetNXContended(b *testing.B) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()
	_ = m.Set(ctx, "k", []byte("existing"), 0)
	v := []byte("v")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = m.SetNX(ctx, "k", v, 0)
	}
}
