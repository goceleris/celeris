package h1

import (
	"strings"
	"testing"
)

// TestPeekChunk1029 locks the helper the body limit uses to refuse a chunk by
// its declared size before its data arrives (celeris#1029).
func TestPeekChunk1029(t *testing.T) {
	cases := []struct {
		in   string
		size int64
		hdr  int
		err  bool
	}{
		{"5\r\nhel", 5, 3, false},
		{"ff;x=1\r\n", 255, 8, false},
		{"FF \r\n", 255, 5, false},
		{"7fffffffffffffff\r\n", 1<<63 - 1, 18, false},
		{"ffffffffffffffff\r\n", 0, 0, true}, // over int64
		{"5", 0, 0, false},
		{"5\r", 0, 0, false},
		{"", 0, 0, false},
		{"zz\r\n", 0, 0, true},
		{"\r\n", 0, 0, true},
		{"0\r\n", 0, 3, false},
		{strings.Repeat("0", MaxChunkLineSize+1), 0, 0, true},               // no CRLF, over the bound
		{strings.Repeat("0", MaxChunkLineSize-1), 0, 0, false},              // no CRLF, within it
		{"1;" + strings.Repeat("e", MaxChunkLineSize) + "\r\n", 0, 0, true}, // complete but over the bound
	}
	for _, tc := range cases {
		size, hdr, err := PeekChunk([]byte(tc.in))
		if (err != nil) != tc.err || size != tc.size || hdr != tc.hdr {
			t.Errorf("PeekChunk(%.30q) = %d, %d, %v; want %d, %d, err=%v", tc.in, size, hdr, err, tc.size, tc.hdr, tc.err)
		}
	}
}

// ParseChunkedBody refuses the same over-long size line PeekChunk does, so a
// line is judged alike whichever way it arrives.
func TestParseChunkedBodyLongLine1029(t *testing.T) {
	p := NewParser()
	p.Reset([]byte("1;" + strings.Repeat("e", MaxChunkLineSize) + "\r\na\r\n0\r\n\r\n"))
	if _, _, err := p.ParseChunkedBody(); err == nil {
		t.Fatal("a size line over MaxChunkLineSize was accepted")
	}
}
