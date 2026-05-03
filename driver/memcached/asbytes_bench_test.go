package memcached

import "testing"

// BenchmarkAsBytes exercises the numeric/float branches of asBytes. The
// []byte and string branches have no allocation to optimize (direct cast)
// and are omitted.
func BenchmarkAsBytes(b *testing.B) {
	cases := []struct {
		name string
		v    any
	}{
		{"int", int(1234567890)},
		{"int64", int64(1234567890)},
		{"uint64", uint64(1234567890)},
		{"float64", float64(1.234567890)},
		{"bool", true},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_, _ = asBytes(tc.v)
			}
		})
	}
}
