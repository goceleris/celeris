package h1

import (
	"bytes"
	"errors"
	"math"
)

var errInvalidChunkSize = errors.New("invalid chunk size")

// ParseChunkedBody parses a single chunk from chunked transfer encoding.
// Returns a zero-copy slice into the parser buffer, bytes consumed, or error.
// Returns (nil, 0, nil) when more data is needed.
func (p *Parser) ParseChunkedBody() ([]byte, int, error) {
	if p.pos >= len(p.buf) {
		return nil, 0, nil
	}

	startPos := p.pos

	lineEnd := bytes.Index(p.buf[p.pos:], bCRLF)
	if lineEnd == -1 {
		return nil, 0, nil
	}

	sizeLine := p.buf[p.pos : p.pos+lineEnd]
	p.pos += lineEnd + 2

	semiIdx := bytes.IndexByte(sizeLine, ';')
	if semiIdx != -1 {
		sizeLine = sizeLine[:semiIdx]
	}
	sizeLine = trimSpace(sizeLine)

	size, ok := parseHex64(sizeLine)
	if !ok {
		return nil, 0, errInvalidChunkSize
	}

	if size == 0 {
		if p.pos+2 <= len(p.buf) {
			p.pos += 2
		}
		return nil, p.pos - startPos, nil
	}

	needed := p.pos + int(size) + 2
	if needed < p.pos || needed > len(p.buf) {
		p.pos = startPos
		return nil, 0, nil
	}

	chunk := p.buf[p.pos : p.pos+int(size)]
	p.pos += int(size) + 2

	return chunk, p.pos - startPos, nil
}

func parseHex64(b []byte) (int64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	var n int64
	for _, c := range b {
		var digit int64
		switch {
		case '0' <= c && c <= '9':
			digit = int64(c - '0')
		case 'a' <= c && c <= 'f':
			digit = int64(c-'a') + 10
		case 'A' <= c && c <= 'F':
			digit = int64(c-'A') + 10
		default:
			return 0, false
		}
		if n > (math.MaxInt64-digit)/16 {
			return 0, false
		}
		n = n*16 + digit
	}
	return n, true
}
