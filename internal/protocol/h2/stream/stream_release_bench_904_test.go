package stream

import "testing"

// BenchmarkStreamAcquireRelease904 is the cost of one use of a pooled stream
// object, from NewStream to Release: the release resets the object under its
// lock since celeris#904 (a StreamWriter that outlives its handler takes the
// same lock), and the H2 processor moves the stream's use token when the
// handler returns (EndUse; included in the Release arm of this benchmark by
// the reset's own bump, and measured with the dispatch benchmarks).
func BenchmarkStreamAcquireRelease904(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := NewStream(1)
		s.Release()
	}
}

func BenchmarkStreamAcquireReleaseParallel904(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s := NewStream(1)
			s.Release()
		}
	})
}
