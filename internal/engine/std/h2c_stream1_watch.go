package std

import (
	"net"

	"golang.org/x/net/http2"
)

// h2cStream1Watch wraps the connection of an h2c upgrade (RFC 7540 3.2) and
// calls onReset, once, when the client sends RST_STREAM for stream 1.
//
// The upgrade request is stream 1, but the HTTP/2 server does not make its
// context from the stream, as it does for every other stream (the context that
// RST_STREAM cancels). It keeps the context of the request it was given, the
// HTTP/1 request, whose context ends with the connection. go1.27 net/http
// rebuilds that request as HTTP/2 but keeps only its context, so this holds
// there too; under -tags http2legacy x/net passes the request on as it came in
// (h2c.go marks it HTTP/2, so the bridge binds the request's context to the
// stream; the watcher is what cancels that context).
// Without this, the handler of the upgrade request is not told that the client
// reset stream 1, where epoll and io_uring cancel it (celeris#949). The watcher
// only reads the frame headers that pass; it never changes a byte, and Read,
// which the HTTP/2 server calls from one goroutine, is the only method that
// touches its state.
type h2cStream1Watch struct {
	net.Conn
	onReset func()

	preface int     // client preface bytes still to pass (the 101 is followed by them)
	skip    int     // payload bytes of the current frame still to pass
	hdr     [9]byte // the frame header being read
	nhdr    int     // bytes of hdr read
	done    bool    // RST_STREAM for stream 1 has been seen
}

func newH2CStream1Watch(c net.Conn, onReset func()) *h2cStream1Watch {
	return &h2cStream1Watch{Conn: c, onReset: onReset, preface: len(http2.ClientPreface)}
}

func (w *h2cStream1Watch) Read(p []byte) (int, error) {
	n, err := w.Conn.Read(p)
	if n > 0 && !w.done {
		w.scan(p[:n])
	}
	return n, err
}

// scan follows the frame boundaries in b, which continues where the last call
// ended, and fires onReset on a RST_STREAM header for stream 1.
func (w *h2cStream1Watch) scan(b []byte) {
	for len(b) > 0 {
		switch {
		case w.preface > 0:
			k := min(w.preface, len(b))
			w.preface -= k
			b = b[k:]
		case w.skip > 0:
			k := min(w.skip, len(b))
			w.skip -= k
			b = b[k:]
		default:
			k := min(len(w.hdr)-w.nhdr, len(b))
			copy(w.hdr[w.nhdr:], b[:k])
			w.nhdr += k
			b = b[k:]
			if w.nhdr < len(w.hdr) {
				return
			}
			w.nhdr = 0
			length := int(w.hdr[0])<<16 | int(w.hdr[1])<<8 | int(w.hdr[2])
			typ := http2.FrameType(w.hdr[3])
			id := uint32(w.hdr[5]&0x7f)<<24 | uint32(w.hdr[6])<<16 | uint32(w.hdr[7])<<8 | uint32(w.hdr[8])
			if typ == http2.FrameRSTStream && id == 1 {
				w.done = true
				w.onReset()
				return
			}
			w.skip = length
		}
	}
}
