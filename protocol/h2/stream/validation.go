package stream

import (
	"fmt"
	"strings"

	"golang.org/x/net/http2"
)

// validateRequestHeaders validates HTTP/2 request headers according to RFC 7540.
func validateRequestHeaders(headers [][2]string) error {
	var (
		hasMethod   bool
		hasScheme   bool
		hasPath     bool
		seenRegular bool
		seenPseudo  = make(map[string]bool)
	)

	for _, h := range headers {
		name := h[0]
		value := h[1]

		if name != strings.ToLower(name) {
			return fmt.Errorf("header field name must be lowercase: %s", name)
		}

		if strings.HasPrefix(name, ":") {
			if seenRegular {
				return fmt.Errorf("pseudo-header %s appears after regular header", name)
			}

			if seenPseudo[name] {
				return fmt.Errorf("duplicate pseudo-header: %s", name)
			}
			seenPseudo[name] = true

			switch name {
			case ":method":
				hasMethod = true
			case ":scheme":
				hasScheme = true
			case ":path":
				hasPath = true
				if value == "" {
					return fmt.Errorf("empty :path pseudo-header")
				}
			case ":authority":
			default:
				return fmt.Errorf("unknown pseudo-header: %s", name)
			}
		} else {
			seenRegular = true
			nameLower := strings.ToLower(name)

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

		if name != strings.ToLower(name) {
			return fmt.Errorf("header field name must be lowercase: %s", name)
		}

		if strings.HasPrefix(name, ":") {
			return fmt.Errorf("pseudo-header not allowed in trailers: %s", name)
		}

		switch strings.ToLower(name) {
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
		if strings.ToLower(h[0]) == "content-length" {
			var expectedLength int
			if _, err := fmt.Sscanf(h[1], "%d", &expectedLength); err != nil {
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

// sendStreamError sends an RST_STREAM frame with the specified error code.
func sendStreamError(writer FrameWriter, streamID uint32, code http2.ErrCode) {
	_ = writer.WriteRSTStream(streamID, code)
	if flusher, ok := writer.(interface{ Flush() error }); ok {
		_ = flusher.Flush()
	}
}

// validateStreamID validates HTTP/2 stream ID according to RFC 7540 specifications.
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
