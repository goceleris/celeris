//line middleware/websocket/prepared.go:1:1
package websocket; import _cover_atomic_ "sync/atomic"

import (
	"encoding/binary"
	"errors"
	"sync"
)

// PreparedMessage caches the wire-format encoding of a message for efficient
// broadcast to multiple connections. Create one with [NewPreparedMessage] and
// send it via [Conn.WritePreparedMessage].
type PreparedMessage struct {
	messageType MessageType
	data        []byte
	mu          sync.Mutex
	frames      map[prepareKey][]byte
}

type prepareKey struct {
	compress bool
	level    int
}

// ErrInvalidPreparedOpcode is returned by [NewPreparedMessage] when the
// caller passes an opcode that cannot be safely cached for fan-out.
//
// Permitted opcodes are [OpText], [OpBinary], and [OpContinuation] (data
// frames). Control opcodes ([OpClose], [OpPing], [OpPong]) are rejected
// because RFC 6455 §5.5 requires control frames to be ≤125 bytes and
// non-fragmented; PreparedMessage's whole purpose is to share the frame
// across many [Conn.WritePreparedMessage] calls, but a >125-byte
// control frame would be a per-connection protocol violation. Callers
// who genuinely want to broadcast a control frame should use
// [Conn.WriteControl] per-connection instead, which validates length.
var ErrInvalidPreparedOpcode = errors.New("websocket: PreparedMessage rejects control opcodes (RFC 6455 §5.5)")

// NewPreparedMessage creates a PreparedMessage from the given payload.
// The data is copied; the caller retains ownership of the original slice.
//
// messageType MUST be a data opcode ([OpText] or [OpBinary]). Passing a
// control opcode returns [ErrInvalidPreparedOpcode] — see RFC 6455 §5.5.
func NewPreparedMessage(messageType MessageType, data []byte) (*PreparedMessage, error) {_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[0], 1);
	if messageType.IsControl() {_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[3], 1);
		return nil, ErrInvalidPreparedOpcode
	}
	_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[1], 1);pm := &PreparedMessage{
		messageType: messageType,
		data:        append([]byte(nil), data...),
		frames:      make(map[prepareKey][]byte, 2),
	}
	// Eagerly build the uncompressed frame.
	_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[2], 1);pm.frames[prepareKey{}] = buildFrameBytes(0x80|byte(messageType&0x0F), data)
	return pm, nil
}

// frame returns the cached wire-format bytes for the given compression config.
// Lazily builds compressed variants on first use.
func (pm *PreparedMessage) frame(compress bool, level int) []byte {_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[4], 1);
	key := prepareKey{compress: compress, level: level}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if f, ok := pm.frames[key]; ok {_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[8], 1);
		return f
	}
	_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[5], 1);if !compress {_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[9], 1);
		// Uncompressed frame (should already be cached, but build if missing).
		f := buildFrameBytes(0x80|byte(pm.messageType&0x0F), pm.data)
		pm.frames[key] = f
		return f
	}
	// Compressed frame: compress data, build frame with RSV1.
	_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[6], 1);buf := acquireCompressBuf()
	defer releaseCompressBuf(buf)
	if err := compressMessage(buf, pm.data, level); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[10], 1);
		// Compression failed; fall back to uncompressed.
		return pm.frame(false, 0)
	}
	_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[7], 1);b0 := byte(0x80|0x40) | byte(pm.messageType&0x0F) // FIN + RSV1 + opcode
	f := buildFrameBytes(b0, buf.data)
	pm.frames[key] = f
	return f
}

// buildFrameBytes builds a complete frame (header + payload) as a []byte.
func buildFrameBytes(b0 byte, payload []byte) []byte {_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[11], 1);
	length := len(payload)
	var frame []byte
	switch {
	case length <= 125:_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[13], 1);
		frame = make([]byte, 2+length)
		frame[0] = b0
		frame[1] = byte(length)
		copy(frame[2:], payload)
	case length <= 65535:_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[14], 1);
		frame = make([]byte, 4+length)
		frame[0] = b0
		frame[1] = 126
		binary.BigEndian.PutUint16(frame[2:], uint16(length))
		copy(frame[4:], payload)
	default:_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[15], 1);
		frame = make([]byte, 10+length)
		frame[0] = b0
		frame[1] = 127
		binary.BigEndian.PutUint64(frame[2:], uint64(length))
		copy(frame[10:], payload)
	}
	_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[12], 1);return frame
}

// WritePreparedMessage sends a pre-encoded message. This is efficient for
// broadcasting the same message to many connections — the frame is encoded
// once and reused.
func (c *Conn) WritePreparedMessage(pm *PreparedMessage) error {_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[16], 1);
	c.lockWrite()
	defer c.unlockWrite()
	c.getWriter()
	defer c.putWriter()

	_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[17], 1);if c.closed.Load() {_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[22], 1);
		return ErrWriteClosed
	}
	_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[18], 1);if c.fragWriting.Load() {_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[23], 1);
		return errors.New("websocket: cannot call WritePreparedMessage while NextWriter is active")
	}

	// Determine if this connection wants compression.
	_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[19], 1);compress := c.compressEnabled && !c.compressDisabled && len(pm.data) >= c.compressThreshold
	f := pm.frame(compress, c.compressLevel)

	_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[20], 1);if _, err := c.bw.Write(f); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[24], 1);
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_prepared.Count[21], 1);return c.bw.Flush()
}

var GoCover_c633_prepared = struct {
	Count     [25]uint32
	Pos       [3 * 25]uint32
	NumStmt   [25]uint16
} {
	Pos: [3 * 25]uint32{
		43, 43, 0x1d0002, // [0]
		46, 50, 0x10002, // [1]
		52, 53, 0x100002, // [2]
		44, 45, 0x10003, // [3]
		59, 62, 0x210002, // [4]
		65, 65, 0xf0002, // [5]
		72, 74, 0x3d0002, // [6]
		78, 81, 0xa0002, // [7]
		63, 64, 0x10003, // [8]
		67, 70, 0x10003, // [9]
		76, 77, 0x10003, // [10]
		86, 88, 0x90002, // [11]
		107, 107, 0xe0002, // [12]
		90, 93, 0x1b0003, // [13]
		95, 99, 0x1b0003, // [14]
		101, 105, 0x1c0003, // [15]
		114, 118, 0x10002, // [16]
		119, 119, 0x150002, // [17]
		122, 122, 0x1a0002, // [18]
		127, 129, 0x10002, // [19]
		130, 130, 0x290002, // [20]
		133, 133, 0x150002, // [21]
		120, 121, 0x10003, // [22]
		123, 124, 0x10003, // [23]
		131, 132, 0x10003, // [24]
	},
	NumStmt: [25]uint16{
		1, // 0
		3, // 1
		3, // 2
		1, // 3
		4, // 4
		1, // 5
		3, // 6
		4, // 7
		1, // 8
		3, // 9
		1, // 10
		3, // 11
		1, // 12
		4, // 13
		5, // 14
		5, // 15
		5, // 16
		5, // 17
		1, // 18
		3, // 19
		3, // 20
		1, // 21
		1, // 22
		1, // 23
		1, // 24
	},
}

var _ = _cover_atomic_.LoadUint32
