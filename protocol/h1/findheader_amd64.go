//go:build amd64

package h1

import "golang.org/x/sys/cpu"

// useAVX2 is set at init time. The assembly reads this to branch between
// the AVX2 (32-byte) and SSE2 (16-byte) scan loops.
var useAVX2 bool

func init() {
	useAVX2 = cpu.X86.HasAVX2
}

//go:noescape
func findHeaderEnd(buf []byte) int
