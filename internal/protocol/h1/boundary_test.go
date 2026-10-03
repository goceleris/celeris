package h1

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestHeadersTooLarge(t *testing.T) {
	// Build a request with headers exceeding 16MB
	var b strings.Builder
	b.WriteString("GET / HTTP/1.1\r\n")
	b.WriteString("Host: example.com\r\n")

	// Each header line: "X-Pad-NNNN: <value>\r\n"
	// Use 8KB values to reach 16MB quickly
	value := strings.Repeat("A", 8192)
	headerCount := (MaxHeaderSize / (8192 + 20)) + 10 // enough to exceed 16MB

	for i := 0; i < headerCount; i++ {
		fmt.Fprintf(&b, "X-Pad-%04d: %s\r\n", i, value)
	}
	b.WriteString("\r\n")

	raw := b.String()
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if !errors.Is(err, ErrHeadersTooLarge) {
		t.Fatalf("got error %v, want %v", err, ErrHeadersTooLarge)
	}
}

func TestHeadersJustUnderLimit(t *testing.T) {
	// Build headers just under 16MB
	var b strings.Builder
	b.WriteString("GET / HTTP/1.1\r\n")
	b.WriteString("Host: example.com\r\n")

	hostLineLen := len("Host: example.com\r\n")

	// Each line: "X: <value>\r\n" = 4 + len(value) + 2
	// Target: total header bytes just under MaxHeaderSize
	remaining := MaxHeaderSize - hostLineLen - 100 // leave margin
	value := strings.Repeat("B", remaining-6)      // "X: " + value + "\r\n"
	fmt.Fprintf(&b, "X: %s\r\n", value)
	b.WriteString("\r\n")

	raw := b.String()
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error for headers under limit: %v", err)
	}
}
