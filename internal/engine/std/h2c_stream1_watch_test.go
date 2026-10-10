package std

import (
	"bytes"
	"io"
	"net"
	"testing"

	"golang.org/x/net/http2"
)

// frame builds one HTTP/2 frame.
func frame949(typ http2.FrameType, stream uint32, payload int) []byte {
	b := []byte{byte(payload >> 16), byte(payload >> 8), byte(payload), byte(typ), 0,
		byte(stream >> 24), byte(stream >> 16), byte(stream >> 8), byte(stream)}
	return append(b, make([]byte, payload)...)
}

// chunkConn serves its bytes in reads of at most chunk bytes.
type chunkConn struct {
	net.Conn
	r     io.Reader
	chunk int
}

func (c *chunkConn) Read(p []byte) (int, error) {
	if len(p) > c.chunk {
		p = p[:c.chunk]
	}
	return c.r.Read(p)
}

// TestH2CStream1WatchSeesOnlyResetOfStreamOne949: the watcher fires for
// RST_STREAM on stream 1, however the bytes are split, and not for other
// frames, other streams, or payload bytes that look like a header.
func TestH2CStream1WatchSeesOnlyResetOfStreamOne949(t *testing.T) {
	// A DATA payload that contains a RST_STREAM(1) header must not fire.
	fake := frame949(http2.FrameRSTStream, 1, 4)
	data := append(frame949(http2.FrameData, 3, 0)[:9:9], fake...)
	data[0], data[1], data[2] = 0, 0, byte(len(fake)) // DATA stream 3 carrying the fake frame as payload

	build := func(frames ...[]byte) []byte {
		out := []byte(http2.ClientPreface)
		for _, f := range frames {
			out = append(out, f...)
		}
		return out
	}
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"reset-of-1", build(frame949(http2.FrameSettings, 0, 6), frame949(http2.FrameRSTStream, 1, 4)), true},
		{"reset-of-1-after-payload-frames", build(frame949(http2.FrameSettings, 0, 18), frame949(http2.FrameWindowUpdate, 0, 4), frame949(http2.FrameRSTStream, 1, 4)), true},
		{"reset-of-3", build(frame949(http2.FrameRSTStream, 3, 4)), false},
		{"settings-and-ping-only", build(frame949(http2.FrameSettings, 0, 6), frame949(http2.FramePing, 0, 8)), false},
		{"header-bytes-inside-a-payload", build(data), false},
		{"data-on-1", build(frame949(http2.FrameData, 1, 10)), false},
		{"reset-of-1-then-more", build(frame949(http2.FrameRSTStream, 1, 4), frame949(http2.FrameRSTStream, 1, 4)), true},
	}
	for _, tc := range cases {
		for _, chunk := range []int{1, 2, 7, 9, 10, 4096} {
			var fired int
			c := &chunkConn{r: bytes.NewReader(tc.in), chunk: chunk}
			w := newH2CStream1Watch(c, func() { fired++ })
			var got []byte
			buf := make([]byte, 64)
			for {
				n, err := w.Read(buf)
				got = append(got, buf[:n]...)
				if err != nil {
					break
				}
			}
			if !bytes.Equal(got, tc.in) {
				t.Fatalf("%s/chunk=%d: the watcher changed the bytes that pass", tc.name, chunk)
			}
			if tc.want && fired != 1 {
				t.Fatalf("%s/chunk=%d: fired %d times, want exactly 1", tc.name, chunk, fired)
			}
			if !tc.want && fired != 0 {
				t.Fatalf("%s/chunk=%d: fired %d times, want 0", tc.name, chunk, fired)
			}
		}
	}
}
