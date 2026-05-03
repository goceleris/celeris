package redisstore

import (
	"context"
	"testing"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/internal/fakeredis"
)

func newBenchStore(b *testing.B) *Store {
	b.Helper()
	srv := fakeredis.Start(b)
	client, err := redis.NewClient(srv.Addr(), redis.WithForceRESP2())
	if err != nil {
		b.Fatalf("redis.NewClient: %v", err)
	}
	b.Cleanup(func() { _ = client.Close() })
	s, err := New(context.Background(), client, Options{RPS: 1000, Burst: 100})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	return s
}

func BenchmarkAllow(b *testing.B) {
	s := newBenchStore(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _, _, _ = s.Allow("user:alice")
	}
}

func BenchmarkUndo(b *testing.B) {
	s := newBenchStore(b)
	// Prime a bucket.
	_, _, _, _ = s.Allow("user:alice")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = s.Undo("user:alice")
	}
}
