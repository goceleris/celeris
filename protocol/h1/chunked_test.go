package h1

import (
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

	var result []byte

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
