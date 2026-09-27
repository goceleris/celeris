//line middleware/websocket/reader.go:1:1
package websocket; import _cover_atomic_ "sync/atomic"

import (
	"bytes"
	"io"
	"unicode/utf8"
)

// messageReader implements io.Reader for a single WebSocket message.
// It reads frame payloads transparently across fragmentation boundaries
// and handles interleaved control frames.
type messageReader struct {
	c         *Conn
	frameData []byte // remaining data in current frame
	final     bool   // true when last frame has been consumed
	opcode    Opcode // message opcode (text or binary)
}

func (r *messageReader) Read(p []byte) (int, error) {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[0], 1);
	for len(r.frameData) == 0 {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[2], 1);
		if r.final {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[4], 1);
			return 0, io.EOF
		}
		// Read next frame.
		_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[3], 1);if err := r.nextFrame(); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[5], 1);
			return 0, err
		}
	}

	_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[1], 1);n := copy(p, r.frameData)
	r.frameData = r.frameData[n:]
	return n, nil
}

func (r *messageReader) nextFrame() error {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[6], 1);
	for {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[7], 1);
		payload, err := r.c.readFrameFast()
		if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[13], 1);
			return err
		}
		_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[8], 1);h := &r.c.readHdr

		// Handle control frames inline (ping/pong/close).
		_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[9], 1);if h.Opcode.IsControl() {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[14], 1);
			if err := r.c.handleControl(h.Opcode, payload); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[16], 1);
				return err
			}
			_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[15], 1);continue
		}

		// Data frame or continuation.
		_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[10], 1);if h.Opcode != OpContinuation && r.opcode != 0 {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[17], 1);
			// Interleaved data frame → protocol error.
			r.c.writeCloseProtocol(CloseProtocolError, "interleaved data frame")
			return ErrProtocol
		}

		_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[11], 1);r.frameData = payload
		r.final = h.Fin

		// Decompress if this message is compressed (RSV1 on first frame).
		// For streaming, we decompress per-frame which is incorrect for
		// permessage-deflate (context spans entire message). For now,
		// streaming + compression is not supported together.

		_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[12], 1);return nil
	}
}

// NextReader returns the next data message received. The io.Reader
// returned reads the message payload across fragmented frames. The reader
// is valid until the next call to NextReader, ReadMessage, or Close.
//
// Control frames (ping, pong, close) are handled transparently.
//
// For compressed messages, NextReader decompresses the entire message
// before returning. Use [Conn.ReadMessage] for the same behavior with
// a simpler API.
func (c *Conn) NextReader() (MessageType, io.Reader, error) {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[18], 1);
	for {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[19], 1);
		payload, err := c.readFrameFast()
		if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[27], 1);
			return 0, nil, err
		}
		_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[20], 1);h := &c.readHdr

		_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[21], 1);if h.Opcode.IsControl() {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[28], 1);
			if err := c.handleControl(h.Opcode, payload); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[30], 1);
				return 0, nil, err
			}
			_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[29], 1);continue
		}

		// Track compression.
		_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[22], 1);compressed := h.RSV1

		_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[23], 1);if h.Fin && !compressed {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[31], 1);
			// Single unfragmented, uncompressed message.
			// Return a simple bytes reader (no streaming needed).
			if h.Opcode == OpText && !utf8.Valid(payload) {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[33], 1);
				c.writeCloseProtocol(CloseInvalidPayload, "invalid UTF-8")
				return 0, nil, ErrInvalidUTF8
			}
			_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[32], 1);return h.Opcode, bytes.NewReader(payload), nil
		}

		_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[24], 1);if compressed {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[34], 1);
			// Compressed messages must be fully assembled before decompression.
			// Fall back to buffered ReadMessage behavior.
			if !h.Fin {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[38], 1);
				c.readFrag = h.Opcode
				c.readCompressed = true
				c.readFragBuf = append(c.readFragBuf[:0], payload...)
				// Read remaining fragments.
				_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[39], 1);mt, data, err := c.ReadMessage()
				if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[41], 1);
					return 0, nil, err
				}
				_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[40], 1);return mt, bytes.NewReader(data), nil
			}
			// Single compressed frame — decompress, bounded by readLimit.
			_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[35], 1);data, derr := decompressMessage(payload, c.readLimit)
			if derr != nil {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[42], 1);
				if derr == ErrReadLimit {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[44], 1);
					c.writeCloseProtocol(CloseMessageTooBig, "decompressed message too large")
				} else{ _cover_atomic_.AddUint32(&GoCover_c633_reader.Count[45], 1);{
					c.writeCloseProtocol(CloseProtocolError, "decompression error")
				}}
				_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[43], 1);return 0, nil, derr
			}
			_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[36], 1);if h.Opcode == OpText && !utf8.Valid(data) {_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[46], 1);
				c.writeCloseProtocol(CloseInvalidPayload, "invalid UTF-8")
				return 0, nil, ErrInvalidUTF8
			}
			_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[37], 1);return h.Opcode, bytes.NewReader(data), nil
		}

		// Multi-frame uncompressed message — return streaming reader.
		_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[25], 1);mr := &messageReader{
			c:         c,
			frameData: payload,
			final:     false,
			opcode:    h.Opcode,
		}
		_cover_atomic_.AddUint32(&GoCover_c633_reader.Count[26], 1);return h.Opcode, mr, nil
	}
}

var GoCover_c633_reader = struct {
	Count     [47]uint32
	Pos       [3 * 47]uint32
	NumStmt   [47]uint16
} {
	Pos: [3 * 47]uint32{
		20, 20, 0x1c0002, // [0]
		30, 32, 0xf0002, // [1]
		21, 21, 0xe0003, // [2]
		25, 25, 0x270003, // [3]
		22, 23, 0x10004, // [4]
		26, 27, 0x10004, // [5]
		36, 36, 0x60002, // [6]
		37, 38, 0x110003, // [7]
		41, 42, 0x10003, // [8]
		44, 44, 0x1b0003, // [9]
		52, 52, 0x320003, // [10]
		58, 60, 0x10003, // [11]
		66, 66, 0xd0003, // [12]
		39, 40, 0x10004, // [13]
		45, 45, 0x3f0004, // [14]
		48, 48, 0xc0004, // [15]
		46, 47, 0x10005, // [16]
		54, 56, 0x10004, // [17]
		80, 80, 0x60002, // [18]
		81, 82, 0x110003, // [19]
		85, 86, 0x10003, // [20]
		87, 87, 0x1b0003, // [21]
		95, 96, 0x10003, // [22]
		97, 97, 0x1b0003, // [23]
		107, 107, 0x110003, // [24]
		139, 144, 0x10003, // [25]
		145, 145, 0x1b0003, // [26]
		83, 84, 0x10004, // [27]
		88, 88, 0x3d0004, // [28]
		91, 91, 0xc0004, // [29]
		89, 90, 0x10005, // [30]
		100, 100, 0x320004, // [31]
		104, 104, 0x320004, // [32]
		101, 103, 0x10005, // [33]
		110, 110, 0xe0004, // [34]
		122, 123, 0x130004, // [35]
		131, 131, 0x2f0004, // [36]
		135, 135, 0x2f0004, // [37]
		111, 114, 0x10005, // [38]
		115, 116, 0x130005, // [39]
		119, 119, 0x2a0005, // [40]
		117, 118, 0x10006, // [41]
		124, 124, 0x1d0005, // [42]
		129, 129, 0x180005, // [43]
		125, 126, 0x10006, // [44]
		127, 128, 0x10006, // [45]
		132, 134, 0x10005, // [46]
	},
	NumStmt: [47]uint16{
		1, // 0
		3, // 1
		1, // 2
		1, // 3
		1, // 4
		1, // 5
		1, // 6
		2, // 7
		2, // 8
		2, // 9
		1, // 10
		3, // 11
		3, // 12
		1, // 13
		1, // 14
		1, // 15
		1, // 16
		2, // 17
		1, // 18
		2, // 19
		2, // 20
		2, // 21
		2, // 22
		2, // 23
		1, // 24
		2, // 25
		2, // 26
		1, // 27
		1, // 28
		1, // 29
		1, // 30
		1, // 31
		1, // 32
		2, // 33
		1, // 34
		2, // 35
		1, // 36
		1, // 37
		5, // 38
		5, // 39
		1, // 40
		1, // 41
		1, // 42
		1, // 43
		1, // 44
		1, // 45
		2, // 46
	},
}

var _ = _cover_atomic_.LoadUint32
