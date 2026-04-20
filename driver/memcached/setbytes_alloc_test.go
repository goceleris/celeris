package memcached

import (
	"context"
	"testing"
	"time"
)

// BenchmarkSetBytesVsSet isolates the alloc delta between the []byte-typed
// SetBytes and the `any`-typed Set on the same in-process fake.
//
// The fake is in-memory text protocol; it returns immediately. Differences
// between the two methods come entirely from the caller-side value handling.
func BenchmarkSetBytesVsSet(b *testing.B) {
	srv := startFakeB(b)
	defer func() { _ = srv.ln.Close() }()
	cli, err := NewClient(srv.Addr())
	if err != nil {
		b.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = cli.Close() }()
	payload := []byte("v-12345678901234567890")

	b.Run("Set_any", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = cli.Set(context.Background(), "k", payload, time.Minute)
		}
	})
	b.Run("SetBytes", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = cli.SetBytes(context.Background(), "k", payload, time.Minute)
		}
	})
}
