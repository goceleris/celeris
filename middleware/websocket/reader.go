package websocket

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

func (r *messageReader) Read(p []byte) (int, error) {
	for len(r.frameData) == 0 {
		if r.final {
			return 0, io.EOF
		}
		// Read next frame.
		if err := r.nextFrame(); err != nil {
			return 0, err
		}
	}

	n := copy(p, r.frameData)
	r.frameData = r.frameData[n:]
	return n, nil
}

func (r *messageReader) nextFrame() error {
	for {
		payload, err := r.c.readFrameFast()
		if err != nil {
			return err
		}
		h := &r.c.readHdr

		// Handle control frames inline (ping/pong/close).
		if h.Opcode.IsControl() {
			if err := r.c.handleControl(h.Opcode, payload); err != nil {
				return err
			}
			continue
		}

		// Data frame or continuation.
		if h.Opcode != OpContinuation && r.opcode != 0 {
			// Interleaved data frame → protocol error.
			r.c.writeCloseProtocol(CloseProtocolError, "interleaved data frame")
			return ErrProtocol
		}

		r.frameData = payload
		r.final = h.Fin

		// Decompress if this message is compressed (RSV1 on first frame).
		// For streaming, we decompress per-frame which is incorrect for
		// permessage-deflate (context spans entire message). For now,
		// streaming + compression is not supported together.

		return nil
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
func (c *Conn) NextReader() (MessageType, io.Reader, error) {
	for {
		payload, err := c.readFrameFast()
		if err != nil {
			return 0, nil, err
		}
		h := &c.readHdr

		if h.Opcode.IsControl() {
			if err := c.handleControl(h.Opcode, payload); err != nil {
				return 0, nil, err
			}
			continue
		}

		// Track compression.
		compressed := h.RSV1

		if h.Fin && !compressed {
			// Single unfragmented, uncompressed message.
			// Return a simple bytes reader (no streaming needed).
			if h.Opcode == OpText && !utf8.Valid(payload) {
				c.writeCloseProtocol(CloseInvalidPayload, "invalid UTF-8")
				return 0, nil, ErrInvalidUTF8
			}
			return h.Opcode, bytes.NewReader(payload), nil
		}

		if compressed {
			// Compressed messages must be fully assembled before decompression.
			// Fall back to buffered ReadMessage behavior.
			if !h.Fin {
				c.readFrag = h.Opcode
				c.readCompressed = true
				c.readFragBuf = append(c.readFragBuf[:0], payload...)
				// Read remaining fragments.
				mt, data, err := c.ReadMessage()
				if err != nil {
					return 0, nil, err
				}
				return mt, bytes.NewReader(data), nil
			}
			// Single compressed frame — decompress, bounded by readLimit.
			data, derr := decompressMessage(payload, c.readLimit)
			if derr != nil {
				if derr == ErrReadLimit {
					c.writeCloseProtocol(CloseMessageTooBig, "decompressed message too large")
				} else {
					c.writeCloseProtocol(CloseProtocolError, "decompression error")
				}
				return 0, nil, derr
			}
			if h.Opcode == OpText && !utf8.Valid(data) {
				c.writeCloseProtocol(CloseInvalidPayload, "invalid UTF-8")
				return 0, nil, ErrInvalidUTF8
			}
			return h.Opcode, bytes.NewReader(data), nil
		}

		// Multi-frame uncompressed message — return streaming reader.
		mr := &messageReader{
			c:         c,
			frameData: payload,
			final:     false,
			opcode:    h.Opcode,
		}
		return h.Opcode, mr, nil
	}
}
