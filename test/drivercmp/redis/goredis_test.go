package redis_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// openGoRedis dials a go-redis client sized to match benchmark parallelism.
func openGoRedis(b *testing.B) *goredis.Client {
	b.Helper()
	opts := &goredis.Options{
		Addr:         addr(b),
		Password:     password(),
		PoolSize:     64,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	}
	c := goredis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		b.Skipf("PING: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	return c
}

func populateGoRedis(b *testing.B, c *goredis.Client) []string {
	b.Helper()
	ctx := context.Background()
	keys := make([]string, benchKeyCount)
	for i := 0; i < benchKeyCount; i++ {
		keys[i] = fmt.Sprintf("%s:k:%d", benchKeyPrefix, i)
		if err := c.Set(ctx, keys[i], benchValue, 0).Err(); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
	return keys
}

func BenchmarkGet_GoRedis(b *testing.B) {
	c := openGoRedis(b)
	keys := populateGoRedis(b, c)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_ = c.Get(ctx, keys[0]).Err()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Get(ctx, keys[0]).Err(); err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}

func BenchmarkSet_GoRedis(b *testing.B) {
	c := openGoRedis(b)
	ctx := context.Background()
	key := benchKeyPrefix + ":set"
	for i := 0; i < 10; i++ {
		_ = c.Set(ctx, key, benchValue, 0).Err()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Set(ctx, key, benchValue, 0).Err(); err != nil {
			b.Fatalf("Set: %v", err)
		}
	}
}

func BenchmarkMGet10_GoRedis(b *testing.B) {
	c := openGoRedis(b)
	keys := populateGoRedis(b, c)[:10]
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = c.MGet(ctx, keys...).Err()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.MGet(ctx, keys...).Err(); err != nil {
			b.Fatalf("MGet: %v", err)
		}
	}
}

func benchmarkPipeline_GoRedis(b *testing.B, n int) {
	c := openGoRedis(b)
	populateGoRedis(b, c)
	ctx := context.Background()
	key := benchKeyPrefix + ":k:0"
	for i := 0; i < 2; i++ {
		p := c.Pipeline()
		for j := 0; j < n; j++ {
			p.Get(ctx, key)
		}
		_, _ = p.Exec(ctx)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := c.Pipeline()
		for j := 0; j < n; j++ {
			p.Get(ctx, key)
		}
		if _, err := p.Exec(ctx); err != nil {
			b.Fatalf("Exec: %v", err)
		}
	}
}

func BenchmarkPipeline10_GoRedis(b *testing.B)    { benchmarkPipeline_GoRedis(b, 10) }
func BenchmarkPipeline100_GoRedis(b *testing.B)   { benchmarkPipeline_GoRedis(b, 100) }
func BenchmarkPipeline1000_GoRedis(b *testing.B)  { benchmarkPipeline_GoRedis(b, 1000) }
func BenchmarkPipeline10000_GoRedis(b *testing.B) { benchmarkPipeline_GoRedis(b, 10000) }

func BenchmarkPubSub1to1Latency_GoRedis(b *testing.B) {
	c := openGoRedis(b)
	pub := openGoRedis(b)
	ctx := context.Background()
	channel := benchKeyPrefix + ":ch:goredis"

	ps := c.Subscribe(ctx, channel)
	defer func() { _ = ps.Close() }()
	// Wait for subscription confirmation.
	if _, err := ps.Receive(ctx); err != nil {
		b.Fatalf("Receive subscribe ack: %v", err)
	}

	msgCh := ps.Channel()
	payload := "x"
	// Warmup.
	for i := 0; i < 5; i++ {
		if err := pub.Publish(ctx, channel, payload).Err(); err != nil {
			b.Fatalf("Publish: %v", err)
		}
		select {
		case <-msgCh:
		case <-time.After(2 * time.Second):
			b.Fatalf("warmup timeout")
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := pub.Publish(ctx, channel, payload).Err(); err != nil {
			b.Fatalf("Publish: %v", err)
		}
		select {
		case <-msgCh:
		case <-time.After(2 * time.Second):
			b.Fatalf("timeout")
		}
	}
}

func BenchmarkPoolAcquireRelease_GoRedis(b *testing.B) {
	c := openGoRedis(b)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_ = c.Ping(ctx).Err()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Ping(ctx).Err(); err != nil {
			b.Fatalf("Ping: %v", err)
		}
	}
}

func BenchmarkParallel_Get_GoRedis(b *testing.B) {
	c := openGoRedis(b)
	keys := populateGoRedis(b, c)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := c.Get(ctx, keys[0]).Err(); err != nil {
				b.Fatalf("Get: %v", err)
			}
		}
	})
}
