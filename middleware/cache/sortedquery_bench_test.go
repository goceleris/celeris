package cache

import "testing"

func BenchmarkSortedQuerySingle(b *testing.B) {
	const q = "id=123"
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = sortedQuery(q)
	}
}

func BenchmarkSortedQueryMulti(b *testing.B) {
	const q = "foo=1&bar=2&baz=3"
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = sortedQuery(q)
	}
}
