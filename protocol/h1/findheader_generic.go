//go:build !amd64 && !arm64

package h1

func findHeaderEnd(buf []byte) int {
	n := len(buf)
	if n < 4 {
		return -1
	}
	for i := 0; i <= n-4; i++ {
		if buf[i] == '\r' && buf[i+1] == '\n' && buf[i+2] == '\r' && buf[i+3] == '\n' {
			return i + 4
		}
	}
	return -1
}
