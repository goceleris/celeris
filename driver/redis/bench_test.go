package redis

import (
	"context"
	"testing"
)

func BenchmarkGetSet(b *testing.B) {
	mem := newMem()
	fake := startFakeRedisBench(b, mem.handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := c.Set(ctx, "k", "v", 0); err != nil {
			b.Fatal(err)
		}
		if _, err := c.Get(ctx, "k"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGetBytes isolates the single-op GET fast path: the fixed-arity
// command encoder must not heap-allocate the argument slice per call.
func BenchmarkGetBytes(b *testing.B) {
	mem := newMem()
	fake := startFakeRedisBench(b, mem.handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	if err := c.Set(ctx, "k", "value-12345678901234567890", 0); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := c.GetBytes(ctx, "k"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSetBytes isolates the single-op SET fast path (no expiry).
func BenchmarkSetBytes(b *testing.B) {
	mem := newMem()
	fake := startFakeRedisBench(b, mem.handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	payload := []byte("value-12345678901234567890")
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := c.SetBytes(ctx, "k", payload, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPipeline100(b *testing.B) {
	mem := newMem()
	fake := startFakeRedisBench(b, mem.handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p := c.Pipeline()
		for j := 0; j < 100; j++ {
			p.Incr("ctr")
		}
		if err := p.Exec(ctx); err != nil {
			b.Fatal(err)
		}
		p.Release()
	}
}

func BenchmarkPipeline1000(b *testing.B) {
	mem := newMem()
	fake := startFakeRedisBench(b, mem.handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p := c.Pipeline()
		for j := 0; j < 1000; j++ {
			p.Incr("ctr")
		}
		if err := p.Exec(ctx); err != nil {
			b.Fatal(err)
		}
		p.Release()
	}
}

func BenchmarkPipeline100_Parallel(b *testing.B) {
	mem := newMem()
	fake := startFakeRedisBench(b, mem.handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p := c.Pipeline()
			for j := 0; j < 100; j++ {
				p.Incr("ctr")
			}
			if err := p.Exec(ctx); err != nil {
				b.Fatal(err)
			}
			p.Release()
		}
	})
}
