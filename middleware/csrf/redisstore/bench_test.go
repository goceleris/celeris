package redisstore

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/internal/fakeredis"
)

func newBenchClient(b *testing.B) *redis.Client {
	b.Helper()
	srv := fakeredis.Start(b)
	c, err := redis.NewClient(srv.Addr(), redis.WithForceRESP2())
	if err != nil {
		b.Fatalf("NewClient: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	return c
}

func BenchmarkGetAndDelete(b *testing.B) {
	client := newBenchClient(b)
	s := New(client)
	ctx := context.Background()
	val := []byte("token-value-abcdef")
	// Seed a key we re-set before each GetAndDelete.
	key := "tok1"
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = s.Set(ctx, key, val, time.Minute)
		_, _ = s.GetAndDelete(ctx, key)
	}
}
