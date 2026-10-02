//go:build linux && celeris_closeprobe

package websocket

import (
	"net"
	"time"
)

// writeAll writes every byte of buf, retrying partial writes until done or the
// overall deadline. A frame written this way is never truncated, so the stream
// stays well-formed regardless of backpressure timing. Returns false on error
// or deadline (e.g. the server killed the conn -- the celeris#482 symptom).
func writeAll(c net.Conn, buf []byte, within time.Duration) bool {
	end := time.Now().Add(within)
	for len(buf) > 0 {
		_ = c.SetWriteDeadline(end)
		n, err := c.Write(buf)
		buf = buf[n:]
		if err != nil {
			return len(buf) == 0
		}
	}
	return true
}
