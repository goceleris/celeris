//line middleware/websocket/writer.go:1:1
package websocket; import _cover_atomic_ "sync/atomic"

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
func (c *Conn) NextWriter(messageType MessageType) (io.WriteCloser, error) {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[0], 1);
	if c.closed.Load() {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[4], 1);
		return nil, ErrWriteClosed
	}
	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[1], 1);c.fragWriting.Store(true)

	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[2], 1);if c.compressEnabled && !c.compressDisabled && messageType.IsData() {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[5], 1);
		return newCompressedMessageWriter(c, messageType), nil
	}

	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[3], 1);return &messageWriter{
		c:      c,
		opcode: messageType,
	}, nil
}

// Write sends a frame containing p. The first call sends a frame with the
// message opcode; subsequent calls send continuation frames. All frames
// except the last have FIN=0.
func (w *messageWriter) Write(p []byte) (int, error) {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[6], 1);
	if len(p) == 0 {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[13], 1);
		return 0, nil
	}

	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[7], 1);w.c.lockWrite()
	defer w.c.unlockWrite()
	w.c.getWriter() // acquire pooled buffer (held until Close)

	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[8], 1);op := w.opcode
	if w.started {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[14], 1);
		op = OpContinuation
	}
	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[9], 1);w.started = true

	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[10], 1);err := writeFrame(w.c.bw, false, op, p, w.c.writeHdr[:])
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[15], 1);
		return 0, err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[11], 1);if err := w.c.bw.Flush(); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[16], 1);
		return 0, err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[12], 1);return len(p), nil
}

// Close sends the final (empty) frame with FIN=1 and clears the fragmented
// write flag.
func (w *messageWriter) Close() error {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[17], 1);
	w.c.lockWrite()
	defer w.c.unlockWrite()
	defer w.c.putWriter() // return pooled buffer
	defer w.c.fragWriting.Store(false)
	w.c.getWriter()

	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[18], 1);op := w.opcode
	if w.started {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[21], 1);
		op = OpContinuation
	}

	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[19], 1);err := writeFrame(w.c.bw, true, op, nil, w.c.writeHdr[:])
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[22], 1);
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[20], 1);return w.c.bw.Flush()
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

func newCompressedMessageWriter(c *Conn, opcode Opcode) *compressedMessageWriter {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[23], 1);
	w := &compressedMessageWriter{c: c, opcode: opcode, fwLevel: c.compressLevel}
	w.tw.dst = &w.outBuf
	w.fw = getFlateWriter(&w.tw, c.compressLevel)
	return w
}

func (w *compressedMessageWriter) Write(p []byte) (int, error) {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[24], 1);
	n, err := w.fw.Write(p)
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[28], 1);
		return n, err
	}
	// Push compressed data through to outBuf.
	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[25], 1);if err := w.fw.Flush(); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[29], 1);
		return n, err
	}
	// Emit frames when buffer is large enough.
	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[26], 1);for w.outBuf.Len() >= defaultWriteBufSize {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[30], 1);
		if err := w.emitFrame(false); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[31], 1);
			return n, err
		}
	}
	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[27], 1);return n, nil
}

func (w *compressedMessageWriter) emitFrame(fin bool) error {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[32], 1);
	// Borrow the compressed bytes directly — no copy needed because the
	// write lock serializes all frame writes and we Reset after writing.
	data := w.outBuf.Bytes()

	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[33], 1);w.c.lockWrite()
	defer w.c.unlockWrite()
	w.c.getWriter()

	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[34], 1);if !w.started {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[39], 1);
		// First frame: opcode + RSV1.
		b0 := byte(w.opcode & 0x0F)
		if fin {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[42], 1);
			b0 |= 0x80
		}
		_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[40], 1);b0 |= 0x40 // RSV1 = compressed
		err := writeFrameRaw(w.c.bw, b0, data, w.c.writeHdr[:])
		if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[43], 1);
			w.outBuf.Reset()
			return err
		}
		_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[41], 1);w.started = true
	} else{ _cover_atomic_.AddUint32(&GoCover_c633_writer.Count[44], 1);{
		err := writeFrame(w.c.bw, fin, OpContinuation, data, w.c.writeHdr[:])
		if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[45], 1);
			w.outBuf.Reset()
			return err
		}
	}}

	// Reset AFTER writing — safe because the write lock prevents concurrent
	// frame writes, and bufio.Writer has already copied data to its internal
	// buffer (or flushed to the socket).
	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[35], 1);w.outBuf.Reset()

	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[36], 1);if err := w.c.bw.Flush(); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[46], 1);
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[37], 1);if fin {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[47], 1);
		w.c.putWriter()
	}
	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[38], 1);return nil
}

func (w *compressedMessageWriter) Close() error {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[48], 1);
	defer w.c.fragWriting.Store(false)

	// Flush remaining compressed data.
	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[49], 1);if err := w.fw.Flush(); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[52], 1);
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[50], 1);if fw, ok := w.fw.(*flate.Writer); ok {_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[53], 1);
		putFlateWriter(fw, w.fwLevel)
	}

	// The truncWriter holds back the last 4 bytes (deflate sync marker
	// 0x00 0x00 0xff 0xff). By not flushing them, we strip the trailer
	// per RFC 7692.

	// Emit final frame with whatever is in outBuf.
	_cover_atomic_.AddUint32(&GoCover_c633_writer.Count[51], 1);return w.emitFrame(true)
}

var GoCover_c633_writer = struct {
	Count     [54]uint32
	Pos       [3 * 54]uint32
	NumStmt   [54]uint16
} {
	Pos: [3 * 54]uint32{
		35, 35, 0x150002, // [0]
		38, 39, 0x10002, // [1]
		40, 40, 0x460002, // [2]
		44, 47, 0x80002, // [3]
		36, 37, 0x10003, // [4]
		41, 42, 0x10003, // [5]
		54, 54, 0x110002, // [6]
		58, 61, 0x10002, // [7]
		62, 63, 0xf0002, // [8]
		66, 67, 0x10002, // [9]
		68, 69, 0x100002, // [10]
		72, 72, 0x270002, // [11]
		75, 75, 0x140002, // [12]
		55, 56, 0x10003, // [13]
		64, 65, 0x10003, // [14]
		70, 71, 0x10003, // [15]
		73, 74, 0x10003, // [16]
		81, 86, 0x10002, // [17]
		87, 88, 0xf0002, // [18]
		92, 93, 0x100002, // [19]
		96, 96, 0x170002, // [20]
		89, 90, 0x10003, // [21]
		94, 95, 0x10003, // [22]
		116, 120, 0x10002, // [23]
		123, 124, 0x100002, // [24]
		128, 128, 0x250002, // [25]
		132, 132, 0x2c0002, // [26]
		137, 137, 0xf0002, // [27]
		125, 126, 0x10003, // [28]
		129, 130, 0x10003, // [29]
		133, 133, 0x2c0003, // [30]
		134, 135, 0x10004, // [31]
		143, 144, 0x10002, // [32]
		145, 148, 0x10002, // [33]
		149, 149, 0x100002, // [34]
		173, 174, 0x10002, // [35]
		175, 175, 0x270002, // [36]
		178, 178, 0x90002, // [37]
		181, 181, 0xc0002, // [38]
		151, 152, 0xa0003, // [39]
		155, 157, 0x110003, // [40]
		161, 161, 0x130003, // [41]
		153, 154, 0x10004, // [42]
		158, 160, 0x10004, // [43]
		163, 164, 0x110003, // [44]
		165, 167, 0x10004, // [45]
		176, 177, 0x10003, // [46]
		179, 180, 0x10003, // [47]
		185, 186, 0x10002, // [48]
		188, 188, 0x250002, // [49]
		191, 191, 0x280002, // [50]
		200, 200, 0x1a0002, // [51]
		189, 190, 0x10003, // [52]
		192, 193, 0x10003, // [53]
	},
	NumStmt: [54]uint16{
		1, // 0
		2, // 1
		2, // 2
		1, // 3
		1, // 4
		1, // 5
		1, // 6
		5, // 7
		5, // 8
		3, // 9
		3, // 10
		1, // 11
		1, // 12
		1, // 13
		1, // 14
		1, // 15
		1, // 16
		7, // 17
		7, // 18
		2, // 19
		1, // 20
		1, // 21
		1, // 22
		4, // 23
		2, // 24
		1, // 25
		1, // 26
		1, // 27
		1, // 28
		1, // 29
		1, // 30
		1, // 31
		5, // 32
		5, // 33
		5, // 34
		2, // 35
		2, // 36
		1, // 37
		1, // 38
		2, // 39
		3, // 40
		1, // 41
		1, // 42
		2, // 43
		2, // 44
		2, // 45
		1, // 46
		1, // 47
		2, // 48
		2, // 49
		1, // 50
		1, // 51
		1, // 52
		1, // 53
	},
}

var _ = _cover_atomic_.LoadUint32
