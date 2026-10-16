package h1

import (
	"bytes"
	"errors"
	"math"
)

var (
	errInvalidChunkSize       = errors.New("invalid chunk size")
	errInvalidChunkTerminator = errors.New("invalid chunk terminator")
	errTrailerTooLarge        = errors.New("chunked trailer too large")
	errChunkLineTooLong       = errors.New("chunk size line too long")
)

// MaxChunkLineSize bounds a chunk-size line (size and extensions, without its
// CRLF) in bytes. It is the same constant net/http uses (maxLineLength in
// net/http/internal/chunked.go), compared a little differently: net/http
// refuses a line whose length including the CRLF is 4096 or more, this refuses
// one of more than 4096 bytes. A longer line is refused whether or not its CRLF
// has arrived, so a client cannot make the server buffer an endless size line.
const MaxChunkLineSize = 4096

// PeekChunk reads the size line of the chunk that starts buf without
// consuming anything. It returns the declared size of the chunk's data and
// the length of the size line including its CRLF. hdr == 0 with a nil error
// means the line is not complete yet. It lets a caller refuse a chunk whose
// declared size is over a limit before the chunk's data has arrived
// (celeris#1029). A zero size is the terminating chunk: its trailer, if any,
// follows hdr.
func PeekChunk(buf []byte) (size int64, hdr int, err error) {
	lineEnd := bytes.Index(buf, bCRLF)
	if lineEnd == -1 {
		// A line of exactly MaxChunkLineSize bytes is accepted once its CRLF
		// is in (the lineEnd check below), so a buffer that ends with that
		// line's CR and not its LF yet is still within the bound: refusing
		// it would make the answer depend on where the segment ends.
		if len(buf) > MaxChunkLineSize+1 || (len(buf) == MaxChunkLineSize+1 && buf[MaxChunkLineSize] != '\r') {
			return 0, 0, errChunkLineTooLong
		}
		return 0, 0, nil
	}
	if lineEnd > MaxChunkLineSize {
		return 0, 0, errChunkLineTooLong
	}
	line := buf[:lineEnd]
	if semi := bytes.IndexByte(line, ';'); semi != -1 {
		line = line[:semi]
	}
	size, ok := parseHex64(trimSpace(line))
	if !ok {
		return 0, 0, errInvalidChunkSize
	}
	return size, lineEnd + 2, nil
}

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

	if lineEnd > MaxChunkLineSize {
		return nil, 0, errChunkLineTooLong
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
		// Terminal zero chunk. The size line ("0") has been consumed; pos now
		// points at the optional trailer section followed by the final CRLF
		// (RFC 9112 §7.1.2). Consume the trailer lines up to and including the
		// terminating empty line so they do not leak into the next request on
		// a keep-alive connection (request smuggling).
		rest := p.buf[p.pos:]
		// No trailers: "0\r\n\r\n" — pos points at the final CRLF.
		if bytes.HasPrefix(rest, bCRLF) {
			p.pos += 2
			return nil, p.pos - startPos, nil
		}
		// Trailers present: scan for the blank line that terminates them.
		// The first trailer line begins immediately at rest[0], so the
		// terminating sequence is the FIRST "\r\n\r\n" (a trailer line's own
		// CRLF followed by the section-ending CRLF).
		end := bytes.Index(rest, bCRLFCRLF)
		if end < 0 {
			// Trailer section incomplete: signal need-more. The driver treats
			// a zero consume as need-more; a nonzero consume here would desync.
			p.pos = startPos
			return nil, 0, nil
		}
		// Bound the trailer section to avoid a trailer DoS. `end` measures the
		// trailer bytes preceding the final CRLF.
		if end > MaxHeaderSize {
			return nil, 0, errTrailerTooLarge
		}
		p.pos += end + 4 // trailer lines + the section-terminating CRLF
		return nil, p.pos - startPos, nil
	}

	needed := p.pos + int(size) + 2
	if needed < p.pos || needed > len(p.buf) {
		p.pos = startPos
		return nil, 0, nil
	}

	// The two reserved bytes after the chunk data MUST be the CRLF terminator;
	// validate them so a malformed chunk (e.g. "5\r\nhelloXY") is rejected
	// rather than silently desyncing the framing. The needed>len(p.buf) guard
	// above already proved both indices are in bounds.
	if p.buf[p.pos+int(size)] != '\r' || p.buf[p.pos+int(size)+1] != '\n' {
		return nil, 0, errInvalidChunkTerminator
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
