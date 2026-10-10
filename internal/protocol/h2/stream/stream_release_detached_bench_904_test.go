package stream

import "testing"

// BenchmarkStreamAcquireReleaseDetached904 is the cost of a use that
// Context.Detach handed to a goroutine: the stream is reset on release but not
// returned to the pool, so the next NewStream allocates one (celeris#904).
// Compare with BenchmarkStreamAcquireRelease904.
func BenchmarkStreamAcquireReleaseDetached904(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := NewStream(1)
		s.MarkDetached()
		s.Release()
	}
}
