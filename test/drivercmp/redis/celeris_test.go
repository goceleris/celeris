package redis_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

// openCeleris dials a celeris Client sized to match benchmark parallelism.
func openCeleris(b *testing.B) *celredis.Client {
	b.Helper()
	opts := []celredis.Option{
		celredis.WithPoolSize(64),
		celredis.WithDialTimeout(5 * time.Second),
	}
	if pw := password(); pw != "" {
		opts = append(opts, celredis.WithPassword(pw))
	}
	c, err := celredis.NewClient(addr(b), opts...)
	if err != nil {
		b.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		b.Skipf("PING: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	return c
}

// populateCeleris sets benchKeyCount short keys for GET/MGET benchmarks.
func populateCeleris(b *testing.B, c *celredis.Client) []string {
	b.Helper()
	ctx := context.Background()
	keys := make([]string, benchKeyCount)
	for i := 0; i < benchKeyCount; i++ {
		keys[i] = fmt.Sprintf("%s:k:%d", benchKeyPrefix, i)
		if err := c.Set(ctx, keys[i], benchValue, 0); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
	return keys
}

func BenchmarkGet_Celeris(b *testing.B) {
	c := openCeleris(b)
	keys := populateCeleris(b, c)
	ctx := context.Background()
	// Warmup.
	for i := 0; i < 10; i++ {
		_, _ = c.Get(ctx, keys[0])
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Get(ctx, keys[0]); err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}

func BenchmarkSet_Celeris(b *testing.B) {
	c := openCeleris(b)
	ctx := context.Background()
	key := benchKeyPrefix + ":set"
	// Warmup.
	for i := 0; i < 10; i++ {
		_ = c.Set(ctx, key, benchValue, 0)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Set(ctx, key, benchValue, 0); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
}

func BenchmarkMGet10_Celeris(b *testing.B) {
	c := openCeleris(b)
	keys := populateCeleris(b, c)[:10]
	ctx := context.Background()
	// Warmup.
	for i := 0; i < 5; i++ {
		_, _ = c.MGet(ctx, keys...)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.MGet(ctx, keys...); err != nil {
			b.Fatalf("MGet: %v", err)
		}
	}
}

func benchmarkPipeline_Celeris(b *testing.B, n int) {
	c := openCeleris(b)
	populateCeleris(b, c)
	ctx := context.Background()
	key := benchKeyPrefix + ":k:0"
	// Warmup: grow the pooled pipeline's slabs to steady-state capacity.
	for i := 0; i < 2; i++ {
		p := c.Pipeline()
		for j := 0; j < n; j++ {
			p.Get(key)
		}
		_ = p.Exec(ctx)
		p.Release()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := c.Pipeline()
		for j := 0; j < n; j++ {
			p.Get(key)
		}
		if err := p.Exec(ctx); err != nil {
			b.Fatalf("Exec: %v", err)
		}
		p.Release()
	}
}

func BenchmarkPipeline10_Celeris(b *testing.B)    { benchmarkPipeline_Celeris(b, 10) }
func BenchmarkPipeline100_Celeris(b *testing.B)   { benchmarkPipeline_Celeris(b, 100) }
func BenchmarkPipeline1000_Celeris(b *testing.B)  { benchmarkPipeline_Celeris(b, 1000) }
func BenchmarkPipeline10000_Celeris(b *testing.B) { benchmarkPipeline_Celeris(b, 10000) }

func BenchmarkPubSub1to1Latency_Celeris(b *testing.B) {
	c := openCeleris(b)
	pub := openCeleris(b)
	ctx := context.Background()
	channel := benchKeyPrefix + ":ch:celeris"

	ps, err := c.Subscribe(ctx, channel)
	if err != nil {
		b.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = ps.Close() }()
	time.Sleep(100 * time.Millisecond)

	// No typed Publish on *Client; use a Pipeline-free raw approach via
	// celeris's Set to emulate command flow. But PUBLISH is required; the
	// driver does not expose it today, so skip if unavailable.
	_ = pub
	b.Skip("celeris-redis does not expose a typed Publish helper on *Client (v1.3.4); add in follow-up and re-enable")
}

func BenchmarkPoolAcquireRelease_Celeris(b *testing.B) {
	c := openCeleris(b)
	ctx := context.Background()
	// Warmup.
	for i := 0; i < 10; i++ {
		_ = c.Ping(ctx)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Ping(ctx); err != nil {
			b.Fatalf("Ping: %v", err)
		}
	}
}

func BenchmarkParallel_Get_Celeris(b *testing.B) {
	c := openCeleris(b)
	keys := populateCeleris(b, c)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := c.Get(ctx, keys[0]); err != nil {
				b.Fatalf("Get: %v", err)
			}
		}
	})
}
