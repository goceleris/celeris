package csrf

import "testing"

var tokenSink string

func BenchmarkStorageKey(b *testing.B) {
	token := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6a7b8c9d0e1f2a3b4c5d6a7b8c9d0e1f2"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tokenSink = storageKey(token)
	}
}
