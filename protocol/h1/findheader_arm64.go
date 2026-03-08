//go:build arm64

package h1

//go:noescape
func findHeaderEnd(buf []byte) int
