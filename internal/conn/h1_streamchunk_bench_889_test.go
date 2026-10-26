package conn

import (
	"fmt"
	"testing"
)

// BenchmarkH1StreamChunk889 is the cost of one streamed chunk through the H1
// response adapter, over a write hook that copies into a buffer as the engines'
// hooks do (celeris#889 item 54), for the sizes of an SSE event (64 B), a
// NDJSON row or a small JSON chunk (1 KiB), a file read (8 KiB, 32 KiB), and a
// chunk over the scratch bound (128 KiB, framed in a buffer of its own). The
// buffer is reset every 64 KiB of writes. It uses only what main has, so the
// same file runs on main and on the branch for the A/B.
func BenchmarkH1StreamChunk889(b *testing.B) {
	for _, size := range []int{64, 1 << 10, 8 << 10, 32 << 10, 64 << 10, 128 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			chunk := make([]byte, size)
			buf := make([]byte, 0, 256<<10)
			a := &h1ResponseAdapter{write: func(p []byte) { buf = append(buf, p...) }, keepAlive: true}
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				if len(buf) > 128<<10 {
					buf = buf[:0]
				}
				_ = a.Write(nil, chunk)
			}
		})
	}
}
