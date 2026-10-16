package conn

import (
	"bytes"
	"testing"
)

// TestCloseH1DropsChunkedState1029: closing a connection that is mid-body
// leaves nothing behind.
func TestCloseH1DropsChunkedState1029(t *testing.T) {
	st := NewH1State()
	h := &rec1029{}
	var out bytes.Buffer
	if _, err := feed1029(st, h, &out, append([]byte(head1029), "5\r\nhel"...), []byte("lo\r\n")); err != nil {
		t.Fatal(err)
	}
	if !st.chunkedInProgress || len(st.chunkBuf) == 0 {
		t.Fatalf("setup: chunkedInProgress=%v chunkBuf=%d", st.chunkedInProgress, len(st.chunkBuf))
	}
	CloseH1(st)
	if st.chunkedInProgress || st.chunkBuf != nil {
		t.Fatal("CloseH1 left chunked state behind")
	}
}

// TestChunkedBufferNotRetained1029: a large chunk grows state.buffer to hold
// it; once the request is served the conn does not keep that array.
func TestChunkedBufferNotRetained1029(t *testing.T) {
	st := NewH1State()
	st.MaxRequestBodySize = 4 << 20
	h := &rec1029{}
	var out bytes.Buffer
	body := pattern1029(2 << 20)
	wire := append([]byte(head1029), chunked1029(body, len(body), "", "")...)
	if _, err := feed1029(st, h, &out, cut1029(wire, 60000)...); err != nil {
		t.Fatal(err)
	}
	if len(h.bodies) != 1 || !bytes.Equal(h.bodies[0], body) {
		t.Fatal("the body was not served")
	}
	if c := st.buffer.Cap(); c > maxRetainedChunkedBuffer {
		t.Fatalf("state.buffer keeps %d bytes of capacity after the request, want at most %d", c, maxRetainedChunkedBuffer)
	}
}
