package benchcmp_db

// Component-level head-to-head: Redis client throughput.
//
// This is NOT an HTTP benchmark — it measures the raw Redis client
// throughput so framework overhead is factored out. Two calls per
// iteration (GET + SET) against a primed key. Run with:
//
//   CELERIS_REDIS_ADDR=127.0.0.1:6379 \
//     go test -run=^$ -bench=BenchmarkRedisClient -benchmem .

import (
	"context"
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
	goredis "github.com/redis/go-redis/v9"
)

func BenchmarkRedisClient_Celeris(b *testing.B) {
	addr := skipIfNoRedis(b)
	cli, err := celredis.NewClient(addr)
	if err != nil {
		b.Fatalf("celeris redis: %v", err)
	}
	defer func() { _ = cli.Close() }()
	ctx := context.Background()
	_ = cli.Set(ctx, "bench:rk", "value", time.Minute)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, err := cli.Get(ctx, "bench:rk")
		if err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}

func BenchmarkRedisClient_GoRedis(b *testing.B) {
	addr := skipIfNoRedis(b)
	cli := goredis.NewClient(&goredis.Options{Addr: addr})
	defer func() { _ = cli.Close() }()
	ctx := context.Background()
	_ = cli.Set(ctx, "bench:rk", "value", time.Minute).Err()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, err := cli.Get(ctx, "bench:rk").Result()
		if err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}
