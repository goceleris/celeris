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
	defer c.Close()
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

func BenchmarkPipeline100(b *testing.B) {
	mem := newMem()
	fake := startFakeRedisBench(b, mem.handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
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
	defer c.Close()
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
	defer c.Close()
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
