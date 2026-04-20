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
		b.Fatalf("redis.NewClient: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	return c
}

func BenchmarkGet(b *testing.B) {
	c := newBenchClient(b)
	s := New(c)
	ctx := context.Background()
	val := []byte(`{"user":"alice","role":"admin"}`)
	_ = s.Set(ctx, "sid", val, time.Minute)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = s.Get(ctx, "sid")
	}
}

func BenchmarkSet(b *testing.B) {
	c := newBenchClient(b)
	s := New(c)
	ctx := context.Background()
	val := []byte(`{"user":"alice","role":"admin"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = s.Set(ctx, "sid", val, time.Minute)
	}
}
