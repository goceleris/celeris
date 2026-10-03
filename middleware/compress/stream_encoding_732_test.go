package compress

import (
	"testing"
	"unsafe"

	"github.com/goceleris/celeris"
)

// TestCompressedStreamOwnsItsEncoding pins the streaming compressor's site of
// celeris#732. NewCompressedStream keeps the encoding for WriteHeader, which
// sends it as Content-Encoding and can run after the handler has returned (a
// detached stream). The caller's string can be a request string, on epoll and
// io_uring a view of the connection's receive buffer, which the engine keeps
// receiving into. Here it shares a byte slice that then changes: the stream
// must still name its own encoding.
func TestCompressedStreamOwnsItsEncoding(t *testing.T) {
	for _, enc := range []string{"gzip", "br"} {
		buf := []byte(enc)
		cs := NewCompressedStream(&celeris.StreamWriter{}, unsafe.String(unsafe.SliceData(buf), len(buf)))
		for i := range buf {
			buf[i] = 'z'
		}
		if cs == nil {
			t.Fatalf("NewCompressedStream(%q) returned nil", enc)
		}
		if cs.encoding != enc {
			t.Errorf("the stream for %q keeps Content-Encoding %q after the caller's bytes changed", enc, cs.encoding)
		}
	}
}
