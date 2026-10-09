package h1

import (
	"errors"
	"testing"
)

func TestParseChunkedBody_SingleChunk(t *testing.T) {
	raw := "5\r\nhello\r\n0\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))

	chunk, n, err := p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n == 0 {
		t.Fatal("expected nonzero consumed")
	}
	if string(chunk) != "hello" {
		t.Fatalf("chunk = %q, want hello", chunk)
	}

	// Terminal chunk
	chunk, n, err = p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("terminal chunk error: %v", err)
	}
	if chunk != nil {
		t.Fatalf("terminal chunk should be nil, got %q", chunk)
	}
	if n == 0 {
		t.Fatal("terminal chunk should consume bytes")
	}
}

func TestParseChunkedBody_MultipleChunks(t *testing.T) {
	raw := "5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))

	result := make([]byte, 0, 64)

	chunk, _, err := p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("chunk 1 error: %v", err)
	}
	result = append(result, chunk...)

	chunk, _, err = p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("chunk 2 error: %v", err)
	}
	result = append(result, chunk...)

	if string(result) != "hello world" {
		t.Fatalf("result = %q, want 'hello world'", result)
	}

	// Terminal
	chunk, n, err := p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("terminal error: %v", err)
	}
	if chunk != nil {
		t.Fatal("terminal chunk should be nil")
	}
	if n == 0 {
		t.Fatal("terminal should consume bytes")
	}
}

func TestParseChunkedBody_TerminalChunk(t *testing.T) {
	raw := "0\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))

	chunk, n, err := p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk != nil {
		t.Fatal("terminal chunk should return nil data")
	}
	if n != 5 {
		t.Fatalf("consumed = %d, want 5", n)
	}
}

func TestParseChunkedBody_ChunkExtensions(t *testing.T) {
	raw := "5;ext=val\r\nhello\r\n0\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))

	chunk, _, err := p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(chunk) != "hello" {
		t.Fatalf("chunk = %q, want hello", chunk)
	}
}

func TestParseChunkedBody_PartialChunk(t *testing.T) {
	// Only the size line, no data yet
	raw := "5\r\nhel"
	p := NewParser()
	p.Reset([]byte(raw))

	chunk, n, err := p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk != nil {
		t.Fatal("partial data should return nil chunk")
	}
	if n != 0 {
		t.Fatalf("partial data consumed = %d, want 0", n)
	}
}

func TestParseChunkedBody_PartialSizeLine(t *testing.T) {
	raw := "5"
	p := NewParser()
	p.Reset([]byte(raw))

	chunk, n, err := p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk != nil || n != 0 {
		t.Fatal("incomplete size line should need more data")
	}
}

func TestParseChunkedBody_ZeroCopy(t *testing.T) {
	raw := []byte("5\r\nhello\r\n0\r\n\r\n")
	p := NewParser()
	p.Reset(raw)

	chunk, _, err := p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify zero-copy: modifying chunk should affect original buffer
	if len(chunk) == 0 {
		t.Fatal("chunk should not be empty")
	}
	chunk[0] = 'H'
	if raw[3] != 'H' {
		t.Fatal("zero-copy violated: modifying chunk did not affect source buffer")
	}
}

func TestParseChunkedBody_HexUpperLower(t *testing.T) {
	raw := "a\r\n0123456789\r\nA\r\n0123456789\r\n0\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))

	chunk1, _, err := p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("chunk 1 error: %v", err)
	}
	if len(chunk1) != 10 {
		t.Fatalf("chunk 1 len = %d, want 10", len(chunk1))
	}

	chunk2, _, err := p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("chunk 2 error: %v", err)
	}
	if len(chunk2) != 10 {
		t.Fatalf("chunk 2 len = %d, want 10", len(chunk2))
	}
}

func TestParseChunkedBody_EmptyBuffer(t *testing.T) {
	p := NewParser()
	p.Reset([]byte{})
	chunk, n, err := p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk != nil || n != 0 {
		t.Fatal("empty buffer should return nil, 0")
	}
}

func TestParseChunkedBody_InvalidSize(t *testing.T) {
	raw := "xyz\r\nhello\r\n"
	p := NewParser()
	p.Reset([]byte(raw))

	_, _, err := p.ParseChunkedBody()
	if err == nil {
		t.Fatal("expected error for invalid chunk size")
	}
}

// drainChunked consumes the whole chunked body via repeated ParseChunkedBody
// and reports the total bytes consumed plus any decode error. Stops at the
// terminal chunk (chunk==nil, n>0) or at need-more (n==0).
func drainChunked(t *testing.T, p *Parser) (consumed int, err error) {
	t.Helper()
	for {
		chunk, n, e := p.ParseChunkedBody()
		if e != nil {
			return consumed, e
		}
		if n == 0 {
			return consumed, nil // need more data
		}
		consumed += n
		if chunk == nil {
			return consumed, nil // terminal zero chunk (trailers consumed)
		}
	}
}

// 4.1: the terminal zero chunk must consume the optional trailer section so it
// does not leak into the next request on a keep-alive connection.
func TestParseChunkedBody_TrailerConsumed(t *testing.T) {
	raw := "0\r\nX-Trailer: v\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))

	consumed, err := drainChunked(t, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if consumed != len(raw) {
		t.Fatalf("consumed = %d, want %d (whole trailer section)", consumed, len(raw))
	}
	if p.Remaining() != 0 {
		t.Fatalf("remaining = %d, want 0 (trailers must be fully consumed)", p.Remaining())
	}
}

// 4.1: a request pipelined after a trailer section must be parsed as the NEXT
// request, not swallowed by the trailer scan or mis-offset.
func TestParseChunkedBody_PipelinedAfterTrailers(t *testing.T) {
	chunked := "5\r\nhello\r\n0\r\nT: 1\r\n\r\n"
	next := "GET /next HTTP/1.1\r\nHost: example.com\r\n\r\n"
	raw := chunked + next
	p := NewParser()
	p.Reset([]byte(raw))

	consumed, err := drainChunked(t, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if consumed != len(chunked) {
		t.Fatalf("consumed = %d, want %d (chunked body incl. trailers only)", consumed, len(chunked))
	}

	// The parser position now sits at the start of the next request. Re-parse
	// from there to confirm framing did not desync. ParseRequest returns the
	// absolute buffer position, so it should advance to the end of the buffer.
	var req Request
	pos, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("next request parse error: %v", err)
	}
	if pos != len(raw) {
		t.Fatalf("next request end position = %d, want %d", pos, len(raw))
	}
	if req.Path != "/next" {
		t.Fatalf("next request path = %q, want /next", req.Path)
	}
}

// 4.1: a trailer section that is incomplete in the buffer (no terminating
// blank line yet) must signal need-more (n==0) and not advance the position,
// so a later retry with more bytes succeeds.
func TestParseChunkedBody_IncompleteTrailers(t *testing.T) {
	raw := "0\r\nX-Trailer: v\r\n" // no terminating blank line
	p := NewParser()
	p.Reset([]byte(raw))

	chunk, n, err := p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk != nil || n != 0 {
		t.Fatalf("incomplete trailers should need more data, got chunk=%v n=%d", chunk, n)
	}
	if p.Remaining() != len(raw) {
		t.Fatalf("position advanced on incomplete trailers: remaining = %d, want %d", p.Remaining(), len(raw))
	}
}

// 4.1: the plain no-trailer terminator "0\r\n\r\n" still works.
func TestParseChunkedBody_NoTrailers(t *testing.T) {
	raw := "0\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))

	chunk, n, err := p.ParseChunkedBody()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk != nil {
		t.Fatal("terminal chunk should return nil data")
	}
	if n != len(raw) {
		t.Fatalf("consumed = %d, want %d", n, len(raw))
	}
}

// 4.4: a chunk whose two reserved trailing bytes are not CRLF must be rejected,
// not silently accepted (which desyncs framing).
func TestParseChunkedBody_InvalidTerminator(t *testing.T) {
	raw := "5\r\nhelloXY" // "XY" where CRLF is required
	p := NewParser()
	p.Reset([]byte(raw))

	_, _, err := p.ParseChunkedBody()
	if !errors.Is(err, errInvalidChunkTerminator) {
		t.Fatalf("got error %v, want errInvalidChunkTerminator", err)
	}
}
