//go:build linux

package iouring

import (
	"context"
	"testing"
)

// BenchmarkAcquireConnState measures the accept-path cost of drawing a
// connection generation (celeris#470 changed it from a plain field
// increment to a process-monotonic atomic).
func BenchmarkAcquireConnState(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cs := acquireConnState(ctx, 42, 0, false)
		releaseConnState(cs)
	}
}
