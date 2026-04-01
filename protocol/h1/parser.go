package h1

import (
	"bytes"
	"errors"
)

// H1 parser sentinel errors.
var (
	ErrBufferExhausted        = errors.New("buffer exhausted")
	ErrInvalidRequestLine     = errors.New("invalid request line")
	ErrInvalidHeader          = errors.New("invalid header line")
	ErrMissingHost            = errors.New("missing Host header")
	ErrUnsupportedVersion     = errors.New("unsupported HTTP version")
	ErrHeadersTooLarge        = errors.New("headers too large")
	ErrInvalidContentLength   = errors.New("invalid content-length")
	ErrDuplicateContentLength = errors.New("duplicate content-length with conflicting values")
)

// Parser is a zero-allocation HTTP/1.x request parser.
type Parser struct {
	buf             []byte
	pos             int
	noStringHeaders bool
}

// NewParser returns a new Parser ready for use.
func NewParser() *Parser {
	return &Parser{}
}

// SetZeroCopy enables zero-copy header parsing. When enabled, the parser
// only populates RawHeaders (not Headers). The caller must convert
// RawHeaders to strings — typically via unsafe.String for zero allocation.
func (p *Parser) SetZeroCopy(enabled bool) {
	p.noStringHeaders = enabled
}

// Reset reinitializes the parser with a new input buffer.
func (p *Parser) Reset(buf []byte) {
	p.buf = buf
	p.pos = 0
}

// ParseRequest parses a complete HTTP/1.x request from the buffer into req.
func (p *Parser) ParseRequest(req *Request) (int, error) {
	if p.pos >= len(p.buf) {
		return 0, ErrBufferExhausted
	}

	complete, err := p.parseRequestLine(req)
	if err != nil {
		return 0, err
	}
	if !complete {
		return 0, nil
	}

	remaining := p.buf[p.pos:]
	// Quick check: if remaining starts with \r\n, headers are empty (no headers).
	// Otherwise use SIMD-accelerated findHeaderEnd to verify \r\n\r\n is present.
	if len(remaining) < 2 || remaining[0] != '\r' || remaining[1] != '\n' {
		if findHeaderEnd(remaining) < 0 {
			return 0, nil
		}
	}

	if p.noStringHeaders {
		req.Headers = req.Headers[:0]
	} else {
		if cap(req.Headers) >= 16 {
			req.Headers = req.Headers[:0]
		} else {
			req.Headers = make([][2]string, 0, 16)
		}
	}
	req.ContentLength = -1
	req.KeepAlive = req.Version == sHTTP11

	complete, err = p.parseHeaders(req)
	if err != nil {
		return 0, err
	}
	if !complete {
		return 0, nil
	}

	if req.Host == "" {
		return 0, ErrMissingHost
	}
	return p.pos, nil
}

func (p *Parser) parseRequestLine(req *Request) (bool, error) {
	lineEnd := bytes.Index(p.buf[p.pos:], bCRLF)
	if lineEnd == -1 {
		return false, nil
	}
	line := p.buf[p.pos : p.pos+lineEnd]
	p.pos += lineEnd + 2

	sp1 := bytes.IndexByte(line, ' ')
	if sp1 == -1 {
		return false, ErrInvalidRequestLine
	}
	methodBytes := line[:sp1]

	rest := line[sp1+1:]
	sp2 := bytes.IndexByte(rest, ' ')
	if sp2 == -1 {
		return false, ErrInvalidRequestLine
	}
	pathBytes := rest[:sp2]
	versionBytes := rest[sp2+1:]

	req.Method = internMethod(methodBytes)
	req.Path = internPath(pathBytes)
	req.Version = internVersion(versionBytes)

	if req.Version != sHTTP11 && req.Version != sHTTP10 {
		return false, ErrUnsupportedVersion
	}
	return true, nil
}

func (p *Parser) parseHeaders(req *Request) (bool, error) {
	var totalHeaderBytes int
	for {
		lineEnd := bytes.Index(p.buf[p.pos:], bCRLF)
		if lineEnd == -1 {
			return false, nil
		}
		line := p.buf[p.pos : p.pos+lineEnd]
		p.pos += lineEnd + 2

		totalHeaderBytes += lineEnd + 2
		if totalHeaderBytes > MaxHeaderSize {
			return false, ErrHeadersTooLarge
		}

		if len(line) == 0 {
			req.HeadersComplete = true
			return true, nil
		}
		colonIdx := bytes.IndexByte(line, ':')
		if colonIdx == -1 {
			return false, ErrInvalidHeader
		}
		rawName := trimSpace(line[:colonIdx])
		rawValue := trimSpace(line[colonIdx+1:])
		if err := p.appendHeader(req, rawName, rawValue); err != nil {
			return false, err
		}
	}
}

func (p *Parser) appendHeader(req *Request, rawName, rawValue []byte) error {
	req.RawHeaders = append(req.RawHeaders, [2][]byte{rawName, rawValue})
	if !p.noStringHeaders {
		name := internOrLowerHeader(rawName)
		value := string(rawValue)
		req.Headers = append(req.Headers, [2]string{name, value})
		switch name {
		case "host":
			req.Host = value
		case "content-length":
			if req.ChunkedEncoding {
				// RFC 7230 §3.3.3: if Transfer-Encoding is present, Content-Length is ignored
				return nil
			}
			cl, ok := parseInt64Bytes(rawValue)
			if !ok {
				return ErrInvalidContentLength
			}
			if req.ContentLength >= 0 && req.ContentLength != cl {
				return ErrDuplicateContentLength
			}
			req.ContentLength = cl
		case "transfer-encoding":
			if asciiContainsFoldString(value, "chunked") {
				req.ChunkedEncoding = true
				req.ContentLength = -1
			}
		case "connection":
			if asciiContainsFoldString(value, "close") {
				req.KeepAlive = false
			} else if asciiContainsFoldString(value, "keep-alive") {
				req.KeepAlive = true
			}
		case "expect":
			if asciiContainsFoldString(value, "100-continue") {
				req.ExpectContinue = true
			}
		}
		return nil
	}
	// First-byte dispatch: skip asciiEqualFold for unrecognized headers.
	// Reduces average header matching from ~5 comparisons to ~1 per header.
	if len(rawName) == 0 {
		return nil
	}
	switch rawName[0] | 0x20 { // lowercase first byte
	case 'h':
		if asciiEqualFold(rawName, "Host") {
			req.Host = UnsafeString(rawValue)
			return nil
		}
	case 'c':
		if asciiEqualFold(rawName, "Content-Length") {
			if req.ChunkedEncoding {
				return nil
			}
			cl, ok := parseInt64Bytes(rawValue)
			if !ok {
				return ErrInvalidContentLength
			}
			if req.ContentLength >= 0 && req.ContentLength != cl {
				return ErrDuplicateContentLength
			}
			req.ContentLength = cl
			return nil
		}
		if asciiEqualFold(rawName, "Connection") {
			if asciiContainsFoldBytes(rawValue, "close") {
				req.KeepAlive = false
			} else if asciiContainsFoldBytes(rawValue, "keep-alive") {
				req.KeepAlive = true
			}
			return nil
		}
	case 't':
		if asciiEqualFold(rawName, "Transfer-Encoding") {
			if asciiContainsFoldBytes(rawValue, "chunked") {
				req.ChunkedEncoding = true
				req.ContentLength = -1
			}
			return nil
		}
	case 'e':
		if asciiEqualFold(rawName, "Expect") {
			if asciiContainsFoldBytes(rawValue, "100-continue") {
				req.ExpectContinue = true
			}
			return nil
		}
	}
	return nil
}

// Remaining returns the number of unconsumed bytes in the buffer.
func (p *Parser) Remaining() int {
	return len(p.buf) - p.pos
}

// GetBody returns a zero-copy slice of the request body for the given content length.
func (p *Parser) GetBody(contentLength int64) []byte {
	if contentLength <= 0 {
		return nil
	}
	available := len(p.buf) - p.pos
	if int64(available) < contentLength {
		return p.buf[p.pos:]
	}
	body := p.buf[p.pos : p.pos+int(contentLength)]
	p.pos += int(contentLength)
	return body
}

// ConsumeBody advances the parser position by n bytes.
func (p *Parser) ConsumeBody(n int) {
	p.pos += n
}
