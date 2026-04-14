package websocket

import (
	"encoding/binary"
	"errors"
	"io"
	"unicode/utf8"
)

// Frame header size limits.
const (
	maxHeaderSize     = 14 // 2 + 8 (extended len) + 4 (mask)
	maxControlPayload = 125
)

// Errors.
var (
	ErrProtocol          = errors.New("websocket: protocol error")
	ErrFrameTooLarge     = errors.New("websocket: frame payload too large")
	ErrReservedBits      = errors.New("websocket: reserved bits set")
	ErrFragmentedControl = errors.New("websocket: fragmented control frame")
	ErrControlTooLarge   = errors.New("websocket: control frame payload > 125")
	ErrInvalidCloseData  = errors.New("websocket: invalid close frame data")
	ErrInvalidUTF8       = errors.New("websocket: invalid UTF-8 in text frame")
	ErrReadLimit         = errors.New("websocket: message exceeds read limit")
	ErrClosed            = errors.New("websocket: connection closed")
	ErrWriteClosed       = errors.New("websocket: write on closed connection")
	ErrWriteTimeout      = errors.New("websocket: write deadline exceeded")
)

// frameHeader is the parsed header of a WebSocket frame.
type frameHeader struct {
	Fin    bool
	RSV1   bool
	RSV2   bool
	RSV3   bool
	Opcode Opcode
	Masked bool
	Length int64
	Mask   [4]byte
}

// readFrameHeader reads a WebSocket frame header from r into h.
// h is passed as a pointer to avoid heap allocation on the return path.
func readFrameHeader(r io.Reader, buf []byte, h *frameHeader) error {
	// Read first 2 bytes.
	if _, err := io.ReadFull(r, buf[:2]); err != nil {
		return err
	}

	b0, b1 := buf[0], buf[1]
	h.Fin = b0&0x80 != 0
	h.RSV1 = b0&0x40 != 0
	h.RSV2 = b0&0x20 != 0
	h.RSV3 = b0&0x10 != 0
	h.Opcode = Opcode(b0 & 0x0F)
	h.Masked = b1&0x80 != 0

	// Payload length.
	length := int64(b1 & 0x7F)
	switch {
	case length < 126:
		h.Length = length
	case length == 126:
		if _, err := io.ReadFull(r, buf[:2]); err != nil {
			return err
		}
		h.Length = int64(binary.BigEndian.Uint16(buf[:2]))
	case length == 127:
		if _, err := io.ReadFull(r, buf[:8]); err != nil {
			return err
		}
		h.Length = int64(binary.BigEndian.Uint64(buf[:8]))
		if h.Length < 0 {
			return ErrFrameTooLarge
		}
	}

	// Masking key.
	if h.Masked {
		if _, err := io.ReadFull(r, h.Mask[:]); err != nil {
			return err
		}
	}

	return nil
}

// writeFrame writes a complete WebSocket frame to w using hdr as scratch space.
// Server frames are never masked (RFC 6455 Section 5.1).
// hdr must be at least maxHeaderSize (14) bytes.
func writeFrame(w io.Writer, fin bool, opcode Opcode, payload []byte, hdr []byte) error {
	pos := 0

	// Byte 0: FIN + opcode.
	b0 := byte(opcode & 0x0F)
	if fin {
		b0 |= 0x80
	}
	hdr[pos] = b0
	pos++

	// Byte 1+: payload length (no mask for server frames).
	length := len(payload)
	switch {
	case length <= 125:
		hdr[pos] = byte(length)
		pos++
	case length <= 65535:
		hdr[pos] = 126
		pos++
		binary.BigEndian.PutUint16(hdr[pos:], uint16(length))
		pos += 2
	default:
		hdr[pos] = 127
		pos++
		binary.BigEndian.PutUint64(hdr[pos:], uint64(length))
		pos += 8
	}

	// Write header then payload. When writing through bufio.Writer (the
	// normal path), both writes fill the internal buffer — no extra
	// syscalls. Avoids allocating a combined slice.
	if _, err := w.Write(hdr[:pos]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

// writeFrameRaw writes a frame with a pre-built first byte (for RSV1 support).
// hdr must be at least maxHeaderSize bytes.
func writeFrameRaw(w io.Writer, b0 byte, payload []byte, hdr []byte) error {
	pos := 0
	hdr[pos] = b0
	pos++

	length := len(payload)
	switch {
	case length <= 125:
		hdr[pos] = byte(length)
		pos++
	case length <= 65535:
		hdr[pos] = 126
		pos++
		binary.BigEndian.PutUint16(hdr[pos:], uint16(length))
		pos += 2
	default:
		hdr[pos] = 127
		pos++
		binary.BigEndian.PutUint64(hdr[pos:], uint64(length))
		pos += 8
	}

	if _, err := w.Write(hdr[:pos]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

// writeCloseFrame writes a close frame with the given status code and optional text.
// hdr must be at least maxHeaderSize bytes.
func writeCloseFrame(w io.Writer, code int, text string, hdr []byte) error {
	if code == 0 {
		return writeFrame(w, true, OpClose, nil, hdr)
	}
	// Cap reason to 123 bytes (max control payload = 125, minus 2 for code).
	if len(text) > maxControlPayload-2 {
		text = text[:maxControlPayload-2]
	}
	// Close payload: 2-byte code + text. Use stack buffer to avoid allocation.
	var stackBuf [maxControlPayload]byte
	n := 2 + len(text)
	var payload []byte
	if n <= len(stackBuf) {
		payload = stackBuf[:n]
	} else {
		payload = make([]byte, n)
	}
	binary.BigEndian.PutUint16(payload, uint16(code))
	copy(payload[2:], text)
	return writeFrame(w, true, OpClose, payload, hdr)
}

// parseClosePayload extracts the status code and reason from a close frame payload.
func parseClosePayload(data []byte) (code int, reason string, err error) {
	if len(data) == 0 {
		return CloseNoStatusReceived, "", nil
	}
	if len(data) == 1 {
		return 0, "", ErrInvalidCloseData
	}
	code = int(binary.BigEndian.Uint16(data[:2]))
	if code < 1000 || code == 1004 || code == 1005 || code == 1006 ||
		code == 1015 || (code >= 1016 && code < 3000) || code >= 5000 {
		return 0, "", ErrInvalidCloseData
	}
	reasonBytes := data[2:]
	if len(reasonBytes) > 0 && !utf8.Valid(reasonBytes) {
		return 0, "", ErrInvalidCloseData
	}
	reason = string(reasonBytes)
	return code, reason, nil
}

// validateFrameHeader checks a frame header for protocol violations.
// compressionEnabled allows RSV1 to be set (permessage-deflate, RFC 7692).
func validateFrameHeader(h *frameHeader, compressionEnabled bool) error {
	// RSV2 and RSV3 must always be 0 (no extensions use them).
	if h.RSV2 || h.RSV3 {
		return ErrReservedBits
	}
	// RSV1 is allowed when compression is negotiated (data frames only).
	if h.RSV1 && !compressionEnabled {
		return ErrReservedBits
	}
	if h.RSV1 && h.Opcode.IsControl() {
		return ErrReservedBits // control frames must never have RSV1
	}

	// Reject reserved opcodes (RFC 6455 Section 5.2).
	switch h.Opcode {
	case OpContinuation, OpText, OpBinary, OpClose, OpPing, OpPong:
		// valid
	default:
		return ErrProtocol
	}

	if h.Opcode.IsControl() {
		// Control frames must not be fragmented.
		if !h.Fin {
			return ErrFragmentedControl
		}
		// Control frame payload <= 125 bytes.
		if h.Length > maxControlPayload {
			return ErrControlTooLarge
		}
	}

	return nil
}
