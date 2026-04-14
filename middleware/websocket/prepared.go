package websocket

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

// NewPreparedMessage creates a PreparedMessage from the given payload.
// The data is copied; the caller retains ownership of the original slice.
func NewPreparedMessage(messageType MessageType, data []byte) (*PreparedMessage, error) {
	pm := &PreparedMessage{
		messageType: messageType,
		data:        append([]byte(nil), data...),
		frames:      make(map[prepareKey][]byte, 2),
	}
	// Eagerly build the uncompressed frame.
	pm.frames[prepareKey{}] = buildFrameBytes(0x80|byte(messageType&0x0F), data)
	return pm, nil
}

// frame returns the cached wire-format bytes for the given compression config.
// Lazily builds compressed variants on first use.
func (pm *PreparedMessage) frame(compress bool, level int) []byte {
	key := prepareKey{compress: compress, level: level}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if f, ok := pm.frames[key]; ok {
		return f
	}
	if !compress {
		// Uncompressed frame (should already be cached, but build if missing).
		f := buildFrameBytes(0x80|byte(pm.messageType&0x0F), pm.data)
		pm.frames[key] = f
		return f
	}
	// Compressed frame: compress data, build frame with RSV1.
	buf := acquireCompressBuf()
	defer releaseCompressBuf(buf)
	if err := compressMessage(buf, pm.data, level); err != nil {
		// Compression failed; fall back to uncompressed.
		return pm.frame(false, 0)
	}
	b0 := byte(0x80|0x40) | byte(pm.messageType&0x0F) // FIN + RSV1 + opcode
	f := buildFrameBytes(b0, buf.data)
	pm.frames[key] = f
	return f
}

// buildFrameBytes builds a complete frame (header + payload) as a []byte.
func buildFrameBytes(b0 byte, payload []byte) []byte {
	length := len(payload)
	var frame []byte
	switch {
	case length <= 125:
		frame = make([]byte, 2+length)
		frame[0] = b0
		frame[1] = byte(length)
		copy(frame[2:], payload)
	case length <= 65535:
		frame = make([]byte, 4+length)
		frame[0] = b0
		frame[1] = 126
		binary.BigEndian.PutUint16(frame[2:], uint16(length))
		copy(frame[4:], payload)
	default:
		frame = make([]byte, 10+length)
		frame[0] = b0
		frame[1] = 127
		binary.BigEndian.PutUint64(frame[2:], uint64(length))
		copy(frame[10:], payload)
	}
	return frame
}

// WritePreparedMessage sends a pre-encoded message. This is efficient for
// broadcasting the same message to many connections — the frame is encoded
// once and reused.
func (c *Conn) WritePreparedMessage(pm *PreparedMessage) error {
	c.lockWrite()
	defer c.unlockWrite()
	c.getWriter()
	defer c.putWriter()

	if c.closed.Load() {
		return ErrWriteClosed
	}
	if c.fragWriting.Load() {
		return errors.New("websocket: cannot call WritePreparedMessage while NextWriter is active")
	}

	// Determine if this connection wants compression.
	compress := c.compressEnabled && !c.compressDisabled && len(pm.data) >= c.compressThreshold
	f := pm.frame(compress, c.compressLevel)

	if _, err := c.bw.Write(f); err != nil {
		return err
	}
	return c.bw.Flush()
}
