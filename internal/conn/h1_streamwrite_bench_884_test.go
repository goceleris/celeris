package conn

import (
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// BenchmarkH1StreamWrite884 is the per-call cost of the H1 response adapter's
// streaming methods (Write, Flush) over a write hook that appends to a buffer,
// as the engines' hooks do: the cost celeris#884 item 45 and celeris#885 item
// 30 added to them, a nil test of the engine's write-error query and a test of
// the latch. The buffer is reset every 256 writes (outside the arms'
// difference). This file compiles without the query, so the same file runs on
// main and on the branch for the A/B; the arms with a query are in
// h1_streamwrite_query_bench_884_test.go.
func BenchmarkH1StreamWrite884(b *testing.B) {
	chunk := make([]byte, 1024)
	for _, arm := range []string{"write", "flush"} {
		b.Run(arm, func(b *testing.B) {
			var buf []byte
			a := &h1ResponseAdapter{write: func(p []byte) { buf = append(buf, p...) }, keepAlive: true}
			var s *stream.Stream
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				if i&255 == 0 {
					buf = buf[:0]
				}
				i++
				if arm == "write" {
					_ = a.Write(s, chunk)
				} else {
					_ = a.Flush(s)
				}
			}
		})
	}
}
