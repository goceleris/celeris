//go:build amd64

package h1

import "golang.org/x/sys/cpu"

// useAVX2 is set at init time. The assembly (findheader_amd64.s) reads this
// to branch between the AVX2 (32-byte) and SSE2 (16-byte) scan loops.
var useAVX2 bool //nolint:unused // referenced by assembly via ·useAVX2(SB)

func init() {
	useAVX2 = cpu.X86.HasAVX2
}

//go:noescape
func findHeaderEnd(buf []byte) int
