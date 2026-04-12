package websocket

import (
	"bytes"
	"compress/flate"
	"io"
)

// messageWriter implements io.WriteCloser for sending a fragmented
// WebSocket message. Each Write call sends a continuation frame; Close
// sends the final frame.
//
// Control frames (ping, pong, close) can still be sent during a streaming
// write — only data frames are blocked.
type messageWriter struct {
	c       *Conn
	opcode  Opcode
	started bool
}

// NextWriter returns an io.WriteCloser for sending a message of the given
// type. The writer sends the message as one or more WebSocket frames.
// The caller must call Close to complete the message.
//
// Only one writer may be active at a time. Starting a new writer while
// the previous one is open is not supported.
//
// Control frames (ping/pong/close) can still be sent concurrently while
// a NextWriter is active — they are not blocked.
//
// If compression is negotiated and enabled, the writer buffers all data
// and compresses on Close (since permessage-deflate context spans the
// entire message).
func (c *Conn) NextWriter(messageType MessageType) (io.WriteCloser, error) {
	if c.closed.Load() {
		return nil, ErrWriteClosed
	}
	c.fragWriting.Store(true)

	if c.compressEnabled && !c.compressDisabled && messageType.IsData() {
		return newCompressedMessageWriter(c, messageType), nil
	}

	return &messageWriter{
		c:      c,
		opcode: messageType,
	}, nil
}

// Write sends a frame containing p. The first call sends a frame with the
// message opcode; subsequent calls send continuation frames. All frames
// except the last have FIN=0.
func (w *messageWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	w.c.lockWrite()
	defer w.c.unlockWrite()
	w.c.getWriter() // acquire pooled buffer (held until Close)

	op := w.opcode
	if w.started {
		op = OpContinuation
	}
	w.started = true

	err := writeFrame(w.c.bw, false, op, p, w.c.writeHdr[:])
	if err != nil {
		return 0, err
	}
	if err := w.c.bw.Flush(); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close sends the final (empty) frame with FIN=1 and clears the fragmented
// write flag.
func (w *messageWriter) Close() error {
	w.c.lockWrite()
	defer w.c.unlockWrite()
	defer w.c.putWriter() // return pooled buffer
	defer w.c.fragWriting.Store(false)
	w.c.getWriter()

	op := w.opcode
	if w.started {
		op = OpContinuation
	}

	err := writeFrame(w.c.bw, true, op, nil, w.c.writeHdr[:])
	if err != nil {
		return err
	}
	return w.c.bw.Flush()
}

// compressedMessageWriter streams data through a flate.Writer and emits
// WebSocket frames incrementally. The flate.Writer maintains compression
// context across Write() calls — this IS the cross-fragment state.
type compressedMessageWriter struct {
	c       *Conn
	opcode  Opcode
	started bool
	tw      truncWriter
	outBuf  bytes.Buffer
	fw      interface {
		Write([]byte) (int, error)
		Flush() error
	}
	fwLevel int
}

func newCompressedMessageWriter(c *Conn, opcode Opcode) *compressedMessageWriter {
	w := &compressedMessageWriter{c: c, opcode: opcode, fwLevel: c.compressLevel}
	w.tw.dst = &w.outBuf
	w.fw = getFlateWriter(&w.tw, c.compressLevel)
	return w
}

func (w *compressedMessageWriter) Write(p []byte) (int, error) {
	n, err := w.fw.Write(p)
	if err != nil {
		return n, err
	}
	// Push compressed data through to outBuf.
	if err := w.fw.Flush(); err != nil {
		return n, err
	}
	// Emit frames when buffer is large enough.
	for w.outBuf.Len() >= defaultWriteBufSize {
		if err := w.emitFrame(false); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (w *compressedMessageWriter) emitFrame(fin bool) error {
	// Borrow the compressed bytes directly — no copy needed because the
	// write lock serializes all frame writes and we Reset after writing.
	data := w.outBuf.Bytes()

	w.c.lockWrite()
	defer w.c.unlockWrite()
	w.c.getWriter()

	if !w.started {
		// First frame: opcode + RSV1.
		b0 := byte(w.opcode & 0x0F)
		if fin {
			b0 |= 0x80
		}
		b0 |= 0x40 // RSV1 = compressed
		err := writeFrameRaw(w.c.bw, b0, data, w.c.writeHdr[:])
		if err != nil {
			w.outBuf.Reset()
			return err
		}
		w.started = true
	} else {
		err := writeFrame(w.c.bw, fin, OpContinuation, data, w.c.writeHdr[:])
		if err != nil {
			w.outBuf.Reset()
			return err
		}
	}

	// Reset AFTER writing — safe because the write lock prevents concurrent
	// frame writes, and bufio.Writer has already copied data to its internal
	// buffer (or flushed to the socket).
	w.outBuf.Reset()

	if err := w.c.bw.Flush(); err != nil {
		return err
	}
	if fin {
		w.c.putWriter()
	}
	return nil
}

func (w *compressedMessageWriter) Close() error {
	defer w.c.fragWriting.Store(false)

	// Flush remaining compressed data.
	if err := w.fw.Flush(); err != nil {
		return err
	}
	if fw, ok := w.fw.(*flate.Writer); ok {
		putFlateWriter(fw, w.fwLevel)
	}

	// The truncWriter holds back the last 4 bytes (deflate sync marker
	// 0x00 0x00 0xff 0xff). By not flushing them, we strip the trailer
	// per RFC 7692.

	// Emit final frame with whatever is in outBuf.
	return w.emitFrame(true)
}
