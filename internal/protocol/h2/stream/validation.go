package stream

import (
	"fmt"

	"golang.org/x/net/http2"
)

// hasForbiddenValueByte reports whether a field value holds a NUL, LF or CR,
// which RFC 9113 section 8.2.1 forbids "at any position": a request that has
// one is malformed (section 8.1.1) and is refused with a stream error. The
// header block is HPACK-decoded, so these bytes can be sent (the decoder
// takes any octet in a string), in a value or in the :path, where they would
// reach the handler and anything that joins or logs the field.
//
// It reads eight bytes at a time and looks at single bytes only in a word that
// has a byte below 0x0E (NUL, LF and CR among them; a bit-twiddling test with
// no false negative): every request header block pays for this scan.
func hasForbiddenValueByte(v string) bool {
	const (
		ones = 0x0101010101010101
		high = 0x8080808080808080
	)
	i := 0
	for ; i+8 <= len(v); i += 8 {
		s := v[i : i+8]
		w := uint64(s[0]) | uint64(s[1])<<8 | uint64(s[2])<<16 | uint64(s[3])<<24 |
			uint64(s[4])<<32 | uint64(s[5])<<40 | uint64(s[6])<<48 | uint64(s[7])<<56
		if (w-0x0E*ones)&^w&high != 0 && forbiddenByteIn(s) {
			return true
		}
	}
	return forbiddenByteIn(v[i:])
}

func forbiddenByteIn(v string) bool {
	for i := 0; i < len(v); i++ {
		if c := v[i]; c <= '\r' && (c == 0 || c == '\n' || c == '\r') {
			return true
		}
	}
	return false
}

// validateRequestHeaders validates HTTP/2 request headers according to RFC 7540.
func validateRequestHeaders(headers [][2]string) error {
	var (
		hasMethod    bool
		hasScheme    bool
		hasPath      bool
		hasAuthority bool
		seenRegular  bool
	)

	for _, h := range headers {
		name := h[0]
		value := h[1]

		if !isValidFieldName(name) {
			return fmt.Errorf("invalid header field name: %q", name)
		}
		if hasForbiddenValueByte(value) {
			return fmt.Errorf("field value of %s holds a NUL, CR or LF", name)
		}

		if len(name) > 0 && name[0] == ':' {
			if seenRegular {
				return fmt.Errorf("pseudo-header %s appears after regular header", name)
			}

			switch name {
			case ":method":
				if hasMethod {
					return fmt.Errorf("duplicate pseudo-header: %s", name)
				}
				hasMethod = true
			case ":scheme":
				if hasScheme {
					return fmt.Errorf("duplicate pseudo-header: %s", name)
				}
				hasScheme = true
			case ":path":
				if hasPath {
					return fmt.Errorf("duplicate pseudo-header: %s", name)
				}
				hasPath = true
				if value == "" {
					return fmt.Errorf("empty :path pseudo-header")
				}
			case ":authority":
				if hasAuthority {
					return fmt.Errorf("duplicate pseudo-header: %s", name)
				}
				hasAuthority = true
			default:
				return fmt.Errorf("unknown pseudo-header: %s", name)
			}
		} else {
			seenRegular = true
			nameLower := name

			switch nameLower {
			case "connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade":
				return fmt.Errorf("connection-specific header not allowed: %s", name)
			case "te":
				if value != "trailers" {
					return fmt.Errorf("TE header must be 'trailers', got: %s", value)
				}
			}
		}
	}

	if !hasMethod {
		return fmt.Errorf("missing required :method pseudo-header")
	}
	if !hasScheme {
		return fmt.Errorf("missing required :scheme pseudo-header")
	}
	if !hasPath {
		return fmt.Errorf("missing required :path pseudo-header")
	}

	return nil
}

// validateTrailerHeaders validates trailing headers for HTTP/2 requests.
func validateTrailerHeaders(headers [][2]string) error {
	for _, h := range headers {
		name := h[0]
		value := h[1]

		if !isValidFieldName(name) {
			return fmt.Errorf("invalid header field name: %q", name)
		}
		if hasForbiddenValueByte(value) {
			return fmt.Errorf("field value of %s holds a NUL, CR or LF", name)
		}

		if len(name) > 0 && name[0] == ':' {
			return fmt.Errorf("pseudo-header not allowed in trailers: %s", name)
		}

		switch name {
		case "connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade":
			return fmt.Errorf("connection-specific header not allowed in trailers: %s", name)
		case "te":
			if value != "trailers" {
				return fmt.Errorf("TE header must be 'trailers', got: %s", value)
			}
		}
	}

	return nil
}

// validateContentLength validates that the Content-Length header matches the actual body length.
func validateContentLength(headers [][2]string, bodyLength int) error {
	for _, h := range headers {
		if h[0] == "content-length" {
			expectedLength, ok := parseUint(h[1])
			if !ok {
				return fmt.Errorf("invalid content-length value: %s", h[1])
			}
			if expectedLength != bodyLength {
				return fmt.Errorf("content-length (%d) does not match body length (%d)",
					expectedLength, bodyLength)
			}
		}
	}
	return nil
}

// parseUint parses a non-negative integer from a string without allocations.
func parseUint(s string) (int, bool) {
	if len(s) == 0 {
		return 0, false
	}
	n := 0
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// sendStreamError sends an RST_STREAM frame with the specified error code.
func sendStreamError(writer FrameWriter, streamID uint32, code http2.ErrCode) {
	_ = writer.WriteRSTStream(streamID, code)
	if flusher, ok := writer.(interface{ Flush() error }); ok {
		_ = flusher.Flush()
	}
}

// validateStreamID validates HTTP/2 stream ID according to RFC 7540 specifications.
//
//nolint:unparam // isClient is designed for both client and server validation
func validateStreamID(streamID uint32, lastClientStream uint32, isClient bool) error {
	if streamID == 0 {
		return fmt.Errorf("stream ID 0 is reserved")
	}

	if !isClient {
		if streamID%2 == 0 {
			return fmt.Errorf("client sent even-numbered stream ID: %d", streamID)
		}

		if streamID <= lastClientStream {
			return fmt.Errorf("stream ID %d is not greater than last stream %d", streamID, lastClientStream)
		}
	}

	return nil
}

// validateStreamState validates that a frame type is allowed for the current stream state.
func validateStreamState(state State, frameType http2.FrameType, _ bool) error {
	switch state {
	case StateIdle:
		if frameType != http2.FrameHeaders && frameType != http2.FramePriority {
			return fmt.Errorf("cannot send %v frame on idle stream", frameType)
		}

	case StateHalfClosedRemote:
		if frameType == http2.FrameData || frameType == http2.FrameHeaders {
			return fmt.Errorf("cannot send %v frame on half-closed (remote) stream", frameType)
		}

	case StateClosed:
		if frameType != http2.FramePriority && frameType != http2.FrameRSTStream && frameType != http2.FrameWindowUpdate {
			return fmt.Errorf("cannot send %v frame on closed stream", frameType)
		}
	}

	return nil
}

// fieldNameTable[c] is true iff c may be in a field name of a request: a
// lower-case letter, a digit or one of the other token characters (RFC 9110
// section 5.6.2, without the upper case, which HTTP/2 forbids).
var fieldNameTable [256]bool

func init() {
	for c := 'a'; c <= 'z'; c++ {
		fieldNameTable[c] = true
	}
	for c := '0'; c <= '9'; c++ {
		fieldNameTable[c] = true
	}
	for _, c := range "!#$%&'*+-.^_`|~" {
		fieldNameTable[c] = true
	}
}

// isValidFieldName reports whether name is a field name an HTTP/2 request may
// carry (RFC 9113 section 8.2.1: no byte in 0x00-0x20, 0x41-0x5a or
// 0x7f-0xff, no colon but the first byte of a pseudo-header's, and not empty;
// only the token characters are allowed, which is what net/http accepts and
// what an HTTP/1 upstream can carry). Such a name that does not meet it makes
// the request malformed (section 8.1.1): a CR LF in a name would be a header
// of its own to anything that writes the list out.
func isValidFieldName(name string) bool {
	i := 0
	if len(name) > 0 && name[0] == ':' {
		i = 1
	}
	if i == len(name) {
		return false
	}
	for ; i < len(name); i++ {
		if !fieldNameTable[name[i]] {
			return false
		}
	}
	return true
}
