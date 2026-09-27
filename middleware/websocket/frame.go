//line frame.go:1:1
package websocket; import _cover_atomic_ "sync/atomic"

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
func readFrameHeader(r io.Reader, buf []byte, h *frameHeader) error {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[0], 1);
	// Read first 2 bytes.
	if _, err := io.ReadFull(r, buf[:2]); err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[5], 1);
		return err
	}

	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[1], 1);b0, b1 := buf[0], buf[1]
	h.Fin = b0&0x80 != 0
	h.RSV1 = b0&0x40 != 0
	h.RSV2 = b0&0x20 != 0
	h.RSV3 = b0&0x10 != 0
	h.Opcode = Opcode(b0 & 0x0F)
	h.Masked = b1&0x80 != 0

	// Payload length.
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[2], 1);length := int64(b1 & 0x7F)
	switch {
	case length < 126:_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[6], 1);
		h.Length = length
	case length == 126:_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[7], 1);
		if _, err := io.ReadFull(r, buf[:2]); err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[11], 1);
			return err
		}
		_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[8], 1);h.Length = int64(binary.BigEndian.Uint16(buf[:2]))
	case length == 127:_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[9], 1);
		if _, err := io.ReadFull(r, buf[:8]); err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[12], 1);
			return err
		}
		_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[10], 1);h.Length = int64(binary.BigEndian.Uint64(buf[:8]))
		if h.Length < 0 {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[13], 1);
			return ErrFrameTooLarge
		}
	}

	// Masking key.
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[3], 1);if h.Masked {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[14], 1);
		if _, err := io.ReadFull(r, h.Mask[:]); err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[15], 1);
			return err
		}
	}

	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[4], 1);return nil
}

// writeFrame writes a complete WebSocket frame to w using hdr as scratch space.
// Server frames are never masked (RFC 6455 Section 5.1).
// hdr must be at least maxHeaderSize (14) bytes.
func writeFrame(w io.Writer, fin bool, opcode Opcode, payload []byte, hdr []byte) error {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[16], 1);
	pos := 0

	// Byte 0: FIN + opcode.
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[17], 1);b0 := byte(opcode & 0x0F)
	if fin {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[23], 1);
		b0 |= 0x80
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[18], 1);hdr[pos] = b0
	pos++

	// Byte 1+: payload length (no mask for server frames).
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[19], 1);length := len(payload)
	switch {
	case length <= 125:_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[24], 1);
		hdr[pos] = byte(length)
		pos++
	case length <= 65535:_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[25], 1);
		hdr[pos] = 126
		pos++
		binary.BigEndian.PutUint16(hdr[pos:], uint16(length))
		pos += 2
	default:_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[26], 1);
		hdr[pos] = 127
		pos++
		binary.BigEndian.PutUint64(hdr[pos:], uint64(length))
		pos += 8
	}

	// Write header then payload. When writing through bufio.Writer (the
	// normal path), both writes fill the internal buffer — no extra
	// syscalls. Avoids allocating a combined slice.
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[20], 1);if _, err := w.Write(hdr[:pos]); err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[27], 1);
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[21], 1);if len(payload) > 0 {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[28], 1);
		_, err := w.Write(payload)
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[22], 1);return nil
}

// writeFrameRaw writes a frame with a pre-built first byte (for RSV1 support).
// hdr must be at least maxHeaderSize bytes.
func writeFrameRaw(w io.Writer, b0 byte, payload []byte, hdr []byte) error {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[29], 1);
	pos := 0
	hdr[pos] = b0
	pos++

	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[30], 1);length := len(payload)
	switch {
	case length <= 125:_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[34], 1);
		hdr[pos] = byte(length)
		pos++
	case length <= 65535:_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[35], 1);
		hdr[pos] = 126
		pos++
		binary.BigEndian.PutUint16(hdr[pos:], uint16(length))
		pos += 2
	default:_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[36], 1);
		hdr[pos] = 127
		pos++
		binary.BigEndian.PutUint64(hdr[pos:], uint64(length))
		pos += 8
	}

	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[31], 1);if _, err := w.Write(hdr[:pos]); err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[37], 1);
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[32], 1);if len(payload) > 0 {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[38], 1);
		_, err := w.Write(payload)
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[33], 1);return nil
}

// writeCloseFrame writes a close frame with the given status code and optional text.
// hdr must be at least maxHeaderSize bytes.
func writeCloseFrame(w io.Writer, code int, text string, hdr []byte) error {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[39], 1);
	if code == 0 {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[43], 1);
		return writeFrame(w, true, OpClose, nil, hdr)
	}
	// Cap reason to 123 bytes (max control payload = 125, minus 2 for code).
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[40], 1);if len(text) > maxControlPayload-2 {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[44], 1);
		text = text[:maxControlPayload-2]
	}
	// Close payload: 2-byte code + text. Use stack buffer to avoid allocation.
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[41], 1);var stackBuf [maxControlPayload]byte
	n := 2 + len(text)
	var payload []byte
	if n <= len(stackBuf) {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[45], 1);
		payload = stackBuf[:n]
	} else{ _cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[46], 1);{
		payload = make([]byte, n)
	}}
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[42], 1);binary.BigEndian.PutUint16(payload, uint16(code))
	copy(payload[2:], text)
	return writeFrame(w, true, OpClose, payload, hdr)
}

// parseClosePayload extracts the status code and reason from a close frame payload.
func parseClosePayload(data []byte) (code int, reason string, err error) {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[47], 1);
	if len(data) == 0 {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[52], 1);
		return CloseNoStatusReceived, "", nil
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[48], 1);if len(data) == 1 {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[53], 1);
		return 0, "", ErrInvalidCloseData
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[49], 1);code = int(binary.BigEndian.Uint16(data[:2]))
	if code < 1000 || code == 1004 || code == 1005 || code == 1006 ||
		code == 1015 || (code >= 1016 && code < 3000) || code >= 5000 {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[54], 1);
		return 0, "", ErrInvalidCloseData
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[50], 1);reasonBytes := data[2:]
	if len(reasonBytes) > 0 && !utf8.Valid(reasonBytes) {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[55], 1);
		return 0, "", ErrInvalidCloseData
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[51], 1);reason = string(reasonBytes)
	return code, reason, nil
}

// validateFrameHeader checks a frame header for protocol violations.
// compressionEnabled allows RSV1 to be set (permessage-deflate, RFC 7692).
func validateFrameHeader(h *frameHeader, compressionEnabled bool) error {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[56], 1);
	// RSV2 and RSV3 must always be 0 (no extensions use them).
	if h.RSV2 || h.RSV3 {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[62], 1);
		return ErrReservedBits
	}
	// RSV1 is allowed when compression is negotiated (data frames only).
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[57], 1);if h.RSV1 && !compressionEnabled {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[63], 1);
		return ErrReservedBits
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[58], 1);if h.RSV1 && h.Opcode.IsControl() {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[64], 1);
		return ErrReservedBits // control frames must never have RSV1
	}

	// Reject reserved opcodes (RFC 6455 Section 5.2).
	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[59], 1);switch h.Opcode {
	case OpContinuation, OpText, OpBinary, OpClose, OpPing, OpPong:_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[65], 1);
		// valid
	default:_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[66], 1);
		return ErrProtocol
	}

	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[60], 1);if h.Opcode.IsControl() {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[67], 1);
		// Control frames must not be fragmented.
		if !h.Fin {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[69], 1);
			return ErrFragmentedControl
		}
		// Control frame payload <= 125 bytes.
		_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[68], 1);if h.Length > maxControlPayload {_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[70], 1);
			return ErrControlTooLarge
		}
	}

	_cover_atomic_.AddUint32(&GoCover_b2b_frame.Count[61], 1);return nil
}

var GoCover_b2b_frame = struct {
	Count     [71]uint32
	Pos       [3 * 71]uint32
	NumStmt   [71]uint16
} {
	Pos: [3 * 71]uint32{
		47, 47, 0x330002, // [0]
		51, 58, 0x10002, // [1]
		60, 61, 0x90002, // [2]
		80, 80, 0xe0002, // [3]
		86, 86, 0xc0002, // [4]
		48, 49, 0x10003, // [5]
		63, 63, 0x140003, // [6]
		65, 65, 0x340003, // [7]
		68, 68, 0x350003, // [8]
		70, 70, 0x340003, // [9]
		73, 74, 0x130003, // [10]
		66, 67, 0x10004, // [11]
		71, 72, 0x10004, // [12]
		75, 76, 0x10004, // [13]
		81, 81, 0x360003, // [14]
		82, 83, 0x10004, // [15]
		93, 94, 0x10002, // [16]
		96, 97, 0x90002, // [17]
		100, 102, 0x10002, // [18]
		104, 105, 0x90002, // [19]
		124, 124, 0x2e0002, // [20]
		127, 127, 0x160002, // [21]
		131, 131, 0xc0002, // [22]
		98, 99, 0x10003, // [23]
		107, 108, 0x80003, // [24]
		110, 113, 0xb0003, // [25]
		115, 118, 0xb0003, // [26]
		125, 126, 0x10003, // [27]
		128, 130, 0x10003, // [28]
		137, 140, 0x10002, // [29]
		141, 142, 0x90002, // [30]
		158, 158, 0x2e0002, // [31]
		161, 161, 0x160002, // [32]
		165, 165, 0xc0002, // [33]
		144, 145, 0x80003, // [34]
		147, 150, 0xb0003, // [35]
		152, 155, 0xb0003, // [36]
		159, 160, 0x10003, // [37]
		162, 164, 0x10003, // [38]
		171, 171, 0xf0002, // [39]
		175, 175, 0x250002, // [40]
		179, 182, 0x180002, // [41]
		187, 189, 0x330002, // [42]
		172, 173, 0x10003, // [43]
		176, 177, 0x10003, // [44]
		183, 184, 0x10003, // [45]
		185, 186, 0x10003, // [46]
		194, 194, 0x140002, // [47]
		197, 197, 0x140002, // [48]
		200, 202, 0x410002, // [49]
		205, 206, 0x360002, // [50]
		209, 210, 0x1a0002, // [51]
		195, 196, 0x10003, // [52]
		198, 199, 0x10003, // [53]
		203, 204, 0x10003, // [54]
		207, 208, 0x10003, // [55]
		217, 217, 0x160002, // [56]
		221, 221, 0x230002, // [57]
		224, 224, 0x240002, // [58]
		229, 229, 0x120002, // [59]
		236, 236, 0x1a0002, // [60]
		247, 247, 0xc0002, // [61]
		218, 219, 0x10003, // [62]
		222, 223, 0x10003, // [63]
		225, 226, 0x10003, // [64]
		230, 230, 0x410041, // [65]
		233, 233, 0x150003, // [66]
		238, 238, 0xd0003, // [67]
		242, 242, 0x230003, // [68]
		239, 240, 0x10004, // [69]
		243, 244, 0x10004, // [70]
	},
	NumStmt: [71]uint16{
		1, // 0
		9, // 1
		9, // 2
		1, // 3
		1, // 4
		1, // 5
		1, // 6
		1, // 7
		1, // 8
		1, // 9
		2, // 10
		1, // 11
		1, // 12
		1, // 13
		1, // 14
		1, // 15
		3, // 16
		3, // 17
		4, // 18
		4, // 19
		1, // 20
		1, // 21
		1, // 22
		1, // 23
		2, // 24
		4, // 25
		4, // 26
		1, // 27
		2, // 28
		5, // 29
		5, // 30
		1, // 31
		1, // 32
		1, // 33
		2, // 34
		4, // 35
		4, // 36
		1, // 37
		2, // 38
		1, // 39
		1, // 40
		4, // 41
		3, // 42
		1, // 43
		1, // 44
		1, // 45
		1, // 46
		1, // 47
		1, // 48
		2, // 49
		2, // 50
		2, // 51
		1, // 52
		1, // 53
		1, // 54
		1, // 55
		1, // 56
		1, // 57
		1, // 58
		1, // 59
		1, // 60
		1, // 61
		1, // 62
		1, // 63
		1, // 64
		0, // 65
		1, // 66
		1, // 67
		1, // 68
		1, // 69
		1, // 70
	},
}

var _ = _cover_atomic_.LoadUint32
