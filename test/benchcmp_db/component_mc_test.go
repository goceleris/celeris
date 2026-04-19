package benchcmp_db

// Component-level head-to-head: memcached GET throughput.
// celeris driver/memcached vs bradfitz/gomemcache.

import (
	"context"
	"testing"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	celmc "github.com/goceleris/celeris/driver/memcached"
)

func BenchmarkMCClient_Celeris(b *testing.B) {
	addr := skipIfNoMemcached(b)
	cli, err := celmc.NewClient(addr)
	if err != nil {
		b.Fatalf("celeris mc: %v", err)
	}
	defer func() { _ = cli.Close() }()
	ctx := context.Background()
	_ = cli.Set(ctx, "bench:mck", "value", time.Minute)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, err := cli.Get(ctx, "bench:mck")
		if err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}

func BenchmarkMCClient_GoMemcache(b *testing.B) {
	addr := skipIfNoMemcached(b)
	cli := memcache.New(addr)
	cli.Timeout = 2 * time.Second
	_ = cli.Set(&memcache.Item{Key: "bench:mck", Value: []byte("value"), Expiration: 60})

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, err := cli.Get("bench:mck")
		if err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}
