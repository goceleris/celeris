package redis

import (
	"bufio"
	"context"
	"testing"
	"time"
)

// BenchmarkSetVsSetBytes isolates the per-call alloc cost of Client.Set
// (value any + argify string conversion) against Client.SetBytes
// (unsafe.String, zero-copy). R20 should show at least -1 alloc/op.
func BenchmarkSetVsSetBytes(b *testing.B) {
	srv := startFakeRedisBench(b, func(cmd []string, w *bufio.Writer) {
		// Minimal "OK" reply for SET.
		_, _ = w.WriteString("+OK\r\n")
	})
	cli, err := NewClient(srv.Addr())
	if err != nil {
		b.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = cli.Close() }()
	payload := []byte("value-12345678901234567890")

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
