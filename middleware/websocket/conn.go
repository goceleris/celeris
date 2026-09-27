//line middleware/websocket/conn.go:1:1
package websocket; import _cover_atomic_ "sync/atomic"

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	defaultReadBufSize  = 4096
	defaultWriteBufSize = 4096
	defaultReadLimit    = 64 * 1024 * 1024 // 64MB
	closeTimeout        = 5 * time.Second
)

// MessageType is the type of a WebSocket message.
type MessageType = Opcode

const (
	// TextMessage denotes a UTF-8 text message.
	TextMessage = OpText
	// BinaryMessage denotes a binary message.
	BinaryMessage = OpBinary
)

// Conn represents a WebSocket connection. It is safe for one goroutine to
// read and another to write concurrently, but not for multiple readers or
// multiple writers.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer

	ctx      context.Context
	cancel   context.CancelFunc
	localsMu sync.RWMutex
	locals   map[string]any

	// Read state.
	readHdr        frameHeader // reusable frame header (avoids heap alloc)
	readBuf        []byte      // reusable read buffer for frame headers
	readPayload    []byte      // reusable payload buffer (grows as needed)
	readLimit      int64
	readFrag       Opcode        // opcode of first frame in fragmented message
	readFragBuf    []byte        // accumulated fragmented payload
	readCompressed bool          // true if current message had RSV1 (compressed)
	readUTF8       utf8Stream    // incremental UTF-8 validator (fragmented text messages)
	idleTimeout    time.Duration // auto read deadline; 0 = disabled

	// Compression state.
	compressEnabled   bool // permessage-deflate negotiated
	compressDisabled  bool // write compression toggled off at runtime
	compressLevel     int  // flate compression level
	compressThreshold int  // minimum payload size for compression

	// Write state.
	writeHdr    [maxHeaderSize]byte // reusable frame header buffer
	writeSem    chan struct{}       // channel-based mutex (buffered 1, gorilla pattern)
	fragWriting atomic.Bool         // true while NextWriter is active
	writePool   BufferPool          // optional: pool *bufio.Writer between connections (hijack path only)
	bwDst       io.Writer           // underlying writer for bw (net.Conn or engineWriter)
	bwPooled    bool                // true when bw was borrowed from writePool and must be Put back

	// Close state.
	closeMu   sync.Mutex
	closeSent atomic.Bool
	closeRecv bool
	closed    atomic.Bool

	// Engine-integrated state (native engines only — std uses raw net.Conn).
	engine        bool         // true when using engine-integrated I/O
	engineReader  *chanReader  // non-nil for engine path; receives chunks from event loop
	engineWriteFn func([]byte) // non-nil for engine path; routes writes through guarded writeFn
	writeErr      atomic.Value // engine-reported I/O error (sticky); read by Write

	// Sticky idle deadline (engine path). Updated after each successful read,
	// honored by the engine's checkTimeouts sweep via SetWSIdleDeadline.
	idleDeadlineFn func(int64) // installed in setupConn for engine path; nil otherwise

	// Callbacks.
	pingHandler  func(data []byte) error
	pongHandler  func(data []byte) error
	closeHandler func(code int, text string) error

	// Captured from celeris.Context at upgrade time.
	params  [][2]string
	query   [][2]string
	headers [][2]string

	subprotocol string

	// cachedIP holds the parsed peer IP (host part of RemoteAddr).
	// Computed lazily on first IP() call so per-message logging loops
	// don't re-parse RemoteAddr.String() on every iteration.
	cachedIP string
}

// newConn creates a Conn from a raw net.Conn (hijack path — used by std engine).
func newConn(ctx context.Context, cancel context.CancelFunc, c net.Conn, readBufSize, writeBufSize int) *Conn {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[0], 1);
	if readBufSize <= 0 {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[4], 1);
		readBufSize = defaultReadBufSize
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[1], 1);if writeBufSize <= 0 {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[5], 1);
		writeBufSize = defaultWriteBufSize
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[2], 1);ws := &Conn{
		conn:      c,
		br:        bufio.NewReaderSize(c, readBufSize),
		bwDst:     c,
		bw:        bufio.NewWriterSize(c, writeBufSize),
		ctx:       ctx,
		cancel:    cancel,
		readBuf:   make([]byte, maxHeaderSize),
		readLimit: defaultReadLimit,
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[3], 1);ws.writeSem = make(chan struct{}, 1)
	ws.writeSem <- struct{}{}
	ws.pingHandler = ws.defaultPingHandler
	return ws
}

// newEngineConn creates a Conn using engine-integrated I/O: reads come from
// a chanReader fed by the event loop, writes go through the engine's
// write buffer via writeFn. The connection is detached from the engine's
// HTTP parser at this point.
func newEngineConn(ctx context.Context, cancel context.CancelFunc, reader *chanReader, writeFn func([]byte),
	readBufSize int) *Conn {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[6], 1);
	if readBufSize <= 0 {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[9], 1);
		readBufSize = defaultReadBufSize
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[7], 1);ws := &Conn{
		ctx:           ctx,
		cancel:        cancel,
		readBuf:       make([]byte, maxHeaderSize),
		readLimit:     defaultReadLimit,
		engine:        true,
		engineReader:  reader,
		engineWriteFn: writeFn,
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[8], 1);ws.br = bufio.NewReaderSize(reader, readBufSize)
	ew := &engineWriter{conn: ws}
	ws.bwDst = ew
	ws.bw = bufio.NewWriterSize(ew, defaultWriteBufSize)
	ws.writeSem = make(chan struct{}, 1)
	ws.writeSem <- struct{}{}
	ws.pingHandler = ws.defaultPingHandler
	return ws
}

// engineWriter wraps the engine's writeFn as an io.Writer. It checks the
// Conn's sticky writeErr (populated by the engine via H1State.OnError) so
// that subsequent Writes after an engine-side I/O failure return the real
// cause instead of a generic ErrWriteClosed.
type engineWriter struct {
	conn *Conn
}

// storedWriteErr boxes the engine-reported error so Conn.writeErr (an
// atomic.Value) always holds ONE concrete type. The engine surfaces errors of
// varying concrete types via OnError (errPeerClosed, syscall errors,
// ErrWriteClosed, …); storing them directly panics atomic.Value with "store of
// inconsistently typed value" on the second differing type. Boxing keeps the
// stored dynamic type constant.
type storedWriteErr struct{ err error }

func (w *engineWriter) Write(p []byte) (int, error) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[10], 1);
	if v := w.conn.writeErr.Load(); v != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[14], 1);
		return 0, v.(storedWriteErr).err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[11], 1);if w.conn.closed.Load() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[15], 1);
		return 0, ErrWriteClosed
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[12], 1);if w.conn.engineWriteFn == nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[16], 1);
		// Conn was constructed pre-Detach (race-window safety in
		// tryEngineUpgrade); the WS handler must not write before
		// setRawWrite has been called.
		return 0, ErrWriteClosed
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[13], 1);w.conn.engineWriteFn(p)
	return len(p), nil
}

// setRawWrite finishes wiring the engine raw-write function into the
// Conn. Used by tryEngineUpgrade to install the engine-provided rawWrite
// AFTER OnError is registered, so a pre-Detach race cannot lose errors.
func (c *Conn) setRawWrite(fn func([]byte)) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[17], 1);
	c.engineWriteFn = fn
}

func (c *Conn) lockWrite()   {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[18], 1); <-c.writeSem }
func (c *Conn) unlockWrite() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[19], 1); c.writeSem <- struct{}{} }

// getWriter returns the bufio.Writer for writing frames. When a
// WriteBufferPool is configured (hijack path only), the writer is
// borrowed from the pool with Reset(c.bwDst) so no allocation happens
// on the steady-state path. The engine path always uses the per-conn
// bufio.Writer because the engine itself pools its write buffer
// (cs.writeBuf) — there is no benefit to a second pooling layer.
//
// The borrow is sticky: the borrowed writer is held across successive
// getWriter calls within the same write-lock epoch, so the streaming
// messageWriter (which calls getWriter on every frame and putWriter
// only on Close) does not leak pool entries. Must be called under the
// write lock.
func (c *Conn) getWriter() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[20], 1);
	if c.writePool == nil || c.engine {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[23], 1);
		return
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[21], 1);if c.bwPooled {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[24], 1);
		return
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[22], 1);c.bw = c.writePool.Get(c.bwDst)
	c.bwPooled = true
}

// putWriter returns a borrowed pool writer. No-op when no pool is
// configured, on the engine path, or when the current bw is not pooled.
// Must be called under the write lock, after Flush. The per-conn bw is
// restored to nil so the next getWriter borrows fresh.
func (c *Conn) putWriter() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[25], 1);
	if c.writePool == nil || c.engine || !c.bwPooled {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[27], 1);
		return
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[26], 1);c.writePool.Put(c.bw)
	c.bw = nil
	c.bwPooled = false
}

// readFrameFast attempts to read an entire frame from the bufio buffer with
// minimal function calls. Falls back to the multi-call path for large frames
// or when the bufio buffer doesn't have enough data.
func (c *Conn) readFrameFast() (payload []byte, err error) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[28], 1);
	h := &c.readHdr

	// Peek 2 bytes to determine frame header size.
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[29], 1);hdr, err := c.br.Peek(2)
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[36], 1);
		// Fall back to io.ReadFull for partial data.
		return c.readFrameSlow()
	}

	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[30], 1);b0, b1 := hdr[0], hdr[1]
	h.Fin = b0&0x80 != 0
	h.RSV1 = b0&0x40 != 0
	h.RSV2 = b0&0x20 != 0
	h.RSV3 = b0&0x10 != 0
	h.Opcode = Opcode(b0 & 0x0F)
	h.Masked = b1&0x80 != 0
	payLen := int64(b1 & 0x7F)

	// Calculate total header size needed.
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[31], 1);headerSize := 2
	switch payLen {
	case 126:_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[37], 1);
		headerSize += 2
	case 127:_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[38], 1);
		headerSize += 8
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[32], 1);if h.Masked {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[39], 1);
		headerSize += 4
	}

	// For small frames: try to peek header + payload in one call.
	//
	// Uncompressed text (first frame or continuation of a text fragment)
	// bypasses the all-at-once peek path so readFrameSlow can validate
	// UTF-8 incrementally as bufio returns each TCP chunk. Without this,
	// a frame split across multiple TCP writes would only be validated
	// after the final chunk arrived (Autobahn 6.4.3/4 NON-STRICT).
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[33], 1);opcodeText := h.Opcode == OpText || (h.Opcode == OpContinuation && c.readFrag == OpText)
	if opcodeText && !h.RSV1 && !c.readCompressed {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[40], 1);
		return c.readFrameSlow()
	}

	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[34], 1);totalSize := headerSize + int(payLen)
	if payLen < 126 && totalSize <= 4096 {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[41], 1);
		// Try to get the entire frame in one peek.
		buf, err := c.br.Peek(totalSize)
		if err == nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[42], 1);
			// Parse everything inline from the peeked buffer.
			pos := 2
			if h.Masked {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[50], 1);
				copy(h.Mask[:], buf[pos:pos+4])
				pos += 4
			}
			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[43], 1);h.Length = payLen

			// Validate.
			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[44], 1);if err := validateFrameHeader(h, c.compressEnabled); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[51], 1);
				_, _ = c.br.Discard(totalSize)
				c.writeCloseProtocol(CloseProtocolError, err.Error())
				return nil, err
			}
			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[45], 1);if !h.Masked {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[52], 1);
				_, _ = c.br.Discard(totalSize)
				c.writeCloseProtocol(CloseProtocolError, "unmasked client frame")
				return nil, ErrProtocol
			}
			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[46], 1);if h.Length > c.readLimit {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[53], 1);
				_, _ = c.br.Discard(totalSize)
				c.writeCloseProtocol(CloseMessageTooBig, "message too large")
				return nil, ErrReadLimit
			}

			// Copy payload into reusable buffer and unmask.
			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[47], 1);n := int(payLen)
			if cap(c.readPayload) < n {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[54], 1);
				c.readPayload = make([]byte, n)
			}
			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[48], 1);payload = c.readPayload[:n]
			copy(payload, buf[pos:pos+n])
			maskBytes(h.Mask, payload)

			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[49], 1);_, _ = c.br.Discard(totalSize)
			return payload, nil
		}
		// Peek failed — not enough data buffered. Fall through to slow path.
	}

	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[35], 1);return c.readFrameSlow()
}

// readFrameSlow reads a frame using multiple io.ReadFull calls (for large
// frames or when the fast peek path fails).
//
// For uncompressed text data — first text frame or a continuation of a
// text fragmented message — the payload is consumed in bufio-sized
// chunks and validated incrementally via readUTF8. This lets us fail
// fast on a TCP-chopped frame (Autobahn 6.4.3/6.4.4 strict behavior):
// the invalid byte sequence is rejected the moment it arrives rather
// than after the entire frame has been buffered.
func (c *Conn) readFrameSlow() ([]byte, error) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[55], 1);
	h := &c.readHdr
	if err := readFrameHeader(c.br, c.readBuf, h); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[64], 1);
		return nil, err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[56], 1);if err := validateFrameHeader(h, c.compressEnabled); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[65], 1);
		c.writeCloseProtocol(CloseProtocolError, err.Error())
		return nil, err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[57], 1);if !h.Masked {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[66], 1);
		c.writeCloseProtocol(CloseProtocolError, "unmasked client frame")
		return nil, ErrProtocol
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[58], 1);if h.Length > c.readLimit {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[67], 1);
		c.writeCloseProtocol(CloseMessageTooBig, "message too large")
		return nil, ErrReadLimit
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[59], 1);n := int(h.Length)
	if cap(c.readPayload) < n {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[68], 1);
		c.readPayload = make([]byte, n)
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[60], 1);payload := c.readPayload[:n]

	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[61], 1);streamValidate := !h.RSV1 && !c.readCompressed &&
		(h.Opcode == OpText || (h.Opcode == OpContinuation && c.readFrag == OpText))
	if streamValidate {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[69], 1);
		if h.Opcode == OpText {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[72], 1);
			c.readUTF8.reset()
		}
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[70], 1);pos := 0
		for pos < n {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[73], 1);
			got, err := c.br.Read(payload[pos:n])
			if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[77], 1);
				return nil, err
			}
			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[74], 1);if got == 0 {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[78], 1);
				continue
			}
			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[75], 1);maskBytesOffset(h.Mask, payload[pos:pos+got], pos)
			final := (pos+got == n) && h.Fin
			if !c.readUTF8.feed(payload[pos:pos+got], final) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[79], 1);
				c.writeCloseProtocol(CloseInvalidPayload, "invalid UTF-8")
				return nil, ErrInvalidUTF8
			}
			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[76], 1);pos += got
		}
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[71], 1);return payload, nil
	}

	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[62], 1);if _, err := io.ReadFull(c.br, payload); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[80], 1);
		return nil, err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[63], 1);maskBytes(h.Mask, payload)
	return payload, nil
}

// ReadMessage reads the next complete message from the connection.
// Returns the message type and an owned copy of the payload. The returned
// slice is safe to retain, pass to other goroutines, or store.
//
// For zero-allocation reads (advanced usage), use [Conn.ReadMessageReuse].
func (c *Conn) ReadMessage() (MessageType, []byte, error) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[81], 1);
	mt, data, err := c.ReadMessageReuse()
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[83], 1);
		return 0, nil, err
	}
	// Return an owned copy so the caller can safely retain it.
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[82], 1);owned := make([]byte, len(data))
	copy(owned, data)
	return mt, owned, nil
}

// ReadMessageReuse reads the next complete message from the connection.
// The returned byte slice is reused across calls and is only valid until
// the next call to ReadMessageReuse or ReadMessage.
//
// Use this for zero-allocation reads when you process each message
// immediately without retaining the slice.
func (c *Conn) ReadMessageReuse() (MessageType, []byte, error) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[84], 1);
	for {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[85], 1);
		// Apply idle timeout before blocking on read.
		// Std (hijack) path: net.Conn.SetReadDeadline.
		// Engine path: extend the engine's idle deadline via SetWSIdleDeadline.
		if c.idleTimeout > 0 {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[98], 1);
			deadline := time.Now().Add(c.idleTimeout)
			if c.conn != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[100], 1);
				_ = c.conn.SetReadDeadline(deadline)
			}
			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[99], 1);if c.idleDeadlineFn != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[101], 1);
				c.idleDeadlineFn(deadline.UnixNano())
			}
		}

		// Fast path: try to read the entire frame from the bufio buffer
		// with a single Peek, avoiding multiple io.ReadFull calls.
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[86], 1);payload, err := c.readFrameFast()
		if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[102], 1);
			return 0, nil, err
		}
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[87], 1);h := &c.readHdr

		// Handle control frames.
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[88], 1);if h.Opcode.IsControl() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[103], 1);
			if err := c.handleControl(h.Opcode, payload); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[105], 1);
				return 0, nil, err
			}
			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[104], 1);continue
		}

		// Handle data frames with fragmentation (loop-based, no recursion).
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[89], 1);if h.Opcode != OpContinuation {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[106], 1);
			// New data frame while fragmentation is in progress → protocol error.
			if c.readFrag != 0 {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[110], 1);
				c.writeCloseProtocol(CloseProtocolError, "interleaved data frame")
				return 0, nil, ErrProtocol
			}
			// Track whether this message is compressed (RSV1 on first frame).
			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[107], 1);c.readCompressed = h.RSV1

			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[108], 1);if h.Fin {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[111], 1);
				// Single unfragmented message.
				if c.readCompressed {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[113], 1);
					var err error
					payload, err = decompressMessage(payload, c.readLimit)
					if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[115], 1);
						if err == ErrReadLimit {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[117], 1);
							c.writeCloseProtocol(CloseMessageTooBig, "decompressed message too large")
						} else{ _cover_atomic_.AddUint32(&GoCover_c633_conn.Count[118], 1);{
							c.writeCloseProtocol(CloseProtocolError, "decompression error")
						}}
						_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[116], 1);return 0, nil, err
					}
					// Compressed text is validated post-decompression; the
					// streaming UTF-8 pass in readFrameSlow only sees
					// raw DEFLATE bytes and is therefore skipped for
					// RSV1 frames.
					_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[114], 1);if h.Opcode == OpText && !utf8.Valid(payload) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[119], 1);
						c.writeCloseProtocol(CloseInvalidPayload, "invalid UTF-8")
						return 0, nil, ErrInvalidUTF8
					}
				}
				// Uncompressed text was already streaming-validated in
				// readFrameSlow; no redundant scan here.
				_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[112], 1);return h.Opcode, payload, nil
			}
			// Start of fragmented message. Uncompressed text is validated
			// incrementally by readFrameSlow as each TCP chunk arrives
			// (strict fail-fast behavior for Autobahn 6.4.x); compressed
			// text defers validation to post-decompression.
			_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[109], 1);c.readFrag = h.Opcode
			c.readFragBuf = append(c.readFragBuf[:0], payload...)
			continue // read next frame
		}

		// Continuation frame.
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[90], 1);if c.readFrag == 0 {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[120], 1);
			c.writeCloseProtocol(CloseProtocolError, "unexpected continuation")
			return 0, nil, ErrProtocol
		}

		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[91], 1);if int64(len(c.readFragBuf))+int64(len(payload)) > c.readLimit {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[121], 1);
			c.writeCloseProtocol(CloseMessageTooBig, "message too large")
			return 0, nil, ErrReadLimit
		}
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[92], 1);c.readFragBuf = append(c.readFragBuf, payload...)

		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[93], 1);if !h.Fin {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[122], 1);
			continue // more fragments coming
		}

		// Final fragment — assemble complete message.
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[94], 1);op := c.readFrag
		c.readFrag = 0
		msg := append([]byte(nil), c.readFragBuf...)
		c.readFragBuf = c.readFragBuf[:0]

		// Decompress fragmented message if RSV1 was set on first frame.
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[95], 1);if c.readCompressed {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[123], 1);
			var derr error
			msg, derr = decompressMessage(msg, c.readLimit)
			if derr != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[124], 1);
				if derr == ErrReadLimit {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[126], 1);
					c.writeCloseProtocol(CloseMessageTooBig, "decompressed message too large")
				} else{ _cover_atomic_.AddUint32(&GoCover_c633_conn.Count[127], 1);{
					c.writeCloseProtocol(CloseProtocolError, "decompression error")
				}}
				_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[125], 1);return 0, nil, derr
			}
		}

		// Uncompressed text was validated incrementally via readUTF8 above;
		// the final feed(...,Fin=true) already enforced no trailing
		// truncation. Compressed text needs a post-decompression pass.
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[96], 1);if op == OpText && c.readCompressed && !utf8.Valid(msg) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[128], 1);
			c.writeCloseProtocol(CloseInvalidPayload, "invalid UTF-8")
			return 0, nil, ErrInvalidUTF8
		}

		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[97], 1);return op, msg, nil
	}
}

func (c *Conn) handleControl(op Opcode, payload []byte) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[129], 1);
	switch op {
	case OpPing:_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[131], 1);
		if c.pingHandler != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[136], 1);
			return c.pingHandler(payload)
		}
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[132], 1);return c.writePong(payload)
	case OpPong:_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[133], 1);
		if c.pongHandler != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[137], 1);
			return c.pongHandler(payload)
		}
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[134], 1);return nil
	case OpClose:_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[135], 1);
		return c.handleCloseFrame(payload)
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[130], 1);return nil
}

func (c *Conn) handleCloseFrame(payload []byte) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[138], 1);
	c.closeMu.Lock()
	c.closeRecv = true
	c.closeMu.Unlock()

	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[139], 1);code, text, err := parseClosePayload(payload)
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[143], 1);
		code = CloseProtocolError
		text = err.Error()
	}

	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[140], 1);if c.closeHandler != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[144], 1);
		return c.closeHandler(code, text)
	}

	// Default: echo close frame back. RFC 6455 §7.4.1 reserves 1005/1006
	// as status-code sentinels that MUST NOT appear on the wire, so when
	// the peer sent an empty-payload close we echo an empty-payload close
	// (writeCloseFrame treats code==0 as "no payload").
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[141], 1);if !c.closeSent.Load() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[145], 1);
		if len(payload) == 0 {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[146], 1);
			_ = c.writeCloseFrame(0, "")
		} else{ _cover_atomic_.AddUint32(&GoCover_c633_conn.Count[147], 1);{
			_ = c.writeCloseFrame(code, text)
		}}
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[142], 1);return &CloseError{Code: code, Text: text}
}

func (c *Conn) defaultPingHandler(data []byte) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[148], 1);
	return c.writePong(data)
}

// WriteMessage writes a complete message to the connection.
// If compression is negotiated and the payload exceeds the compression
// threshold, the message is compressed transparently. Compression runs
// before the write lock is taken so that ping/pong control frames can
// interleave with large compressed data writes.
func (c *Conn) WriteMessage(messageType MessageType, data []byte) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[149], 1);
	if c.closed.Load() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[158], 1);
		return ErrWriteClosed
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[150], 1);if c.fragWriting.Load() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[159], 1);
		return errors.New("websocket: cannot call WriteMessage while NextWriter is active")
	}

	// Compress data frames if compression is negotiated and payload is
	// large enough. This intentionally runs OUTSIDE the write lock so
	// concurrent control frames can interleave during expensive deflates.
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[151], 1);compressed := false
	var pooled *compressBuf
	if c.compressEnabled && !c.compressDisabled && messageType.IsData() && len(data) >= c.compressThreshold {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[160], 1);
		pooled = acquireCompressBuf()
		if err := compressMessage(pooled, data, c.compressLevel); err == nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[162], 1);
			// Only use compressed version if it's actually smaller.
			if len(pooled.data) < len(data) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[163], 1);
				data = pooled.data
				compressed = true
			}
		}
		// Release pool buffer after we're done with it (deferred so the
		// data slice remains valid for the duration of the write).
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[161], 1);defer releaseCompressBuf(pooled)
	}

	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[152], 1);c.lockWrite()
	defer c.unlockWrite()
	c.getWriter()
	defer c.putWriter()

	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[153], 1);if c.closed.Load() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[164], 1);
		return ErrWriteClosed
	}

	// Build frame header byte 0.
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[154], 1);b0 := byte(0x80) | byte(messageType&0x0F) // FIN + opcode
	if compressed {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[165], 1);
		b0 |= 0x40 // RSV1 = compressed
	}

	// Fast path: small uncompressed frames.
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[155], 1);length := len(data)
	if length <= 125 {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[166], 1);
		pos := 0
		c.writeHdr[pos] = b0
		pos++
		c.writeHdr[pos] = byte(length)
		pos++
		if _, err := c.bw.Write(c.writeHdr[:pos]); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[169], 1);
			return err
		}
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[167], 1);if length > 0 {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[170], 1);
			if _, err := c.bw.Write(data); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[171], 1);
				return err
			}
		}
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[168], 1);return c.bw.Flush()
	}

	// General path with custom b0 for RSV1.
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[156], 1);err := writeFrameRaw(c.bw, b0, data, c.writeHdr[:])
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[172], 1);
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[157], 1);return c.bw.Flush()
}

// WriteText writes a text message.
func (c *Conn) WriteText(data []byte) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[173], 1);
	return c.WriteMessage(TextMessage, data)
}

// WriteBinary writes a binary message.
func (c *Conn) WriteBinary(data []byte) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[174], 1);
	return c.WriteMessage(BinaryMessage, data)
}

// WriteJSON marshals v as JSON and writes it as a text message.
func (c *Conn) WriteJSON(v any) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[175], 1);
	data, err := json.Marshal(v)
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[177], 1);
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[176], 1);return c.WriteMessage(TextMessage, data)
}

// ReadJSON reads the next message and unmarshals it from JSON.
func (c *Conn) ReadJSON(v any) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[178], 1);
	_, data, err := c.ReadMessageReuse()
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[180], 1);
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[179], 1);return json.Unmarshal(data, v)
}

// WritePing sends a ping control frame. The payload must be <= 125 bytes.
// Use this with [Conn.SetPongHandler] to implement keepalive.
func (c *Conn) WritePing(data []byte) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[181], 1);
	c.lockWrite()
	c.getWriter()
	defer c.putWriter()
	defer c.unlockWrite()
	if c.closed.Load() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[184], 1);
		return ErrWriteClosed
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[182], 1);err := writeFrame(c.bw, true, OpPing, data, c.writeHdr[:])
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[185], 1);
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[183], 1);return c.bw.Flush()
}

// WriteControl sends a control frame with a per-frame deadline. It can be
// called concurrently with [Conn.NextWriter] — the channel-based write
// semaphore is acquired with the deadline, returning [ErrWriteTimeout] if
// it cannot be obtained in time.
//
// On the std (hijack) path the deadline is also applied to the underlying
// net.Conn via SetWriteDeadline so that a slow peer cannot indefinitely
// block the actual flush. On the engine path the write goes into the
// engine's per-connection write buffer (cs.writeBuf) and never blocks at
// the syscall level, so only the lock-acquisition deadline applies.
//
// messageType must be a control opcode ([OpClose], [OpPing], [OpPong]).
// data must be <= 125 bytes.
func (c *Conn) WriteControl(messageType int, data []byte, deadline time.Time) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[186], 1);
	op := Opcode(messageType)
	if !op.IsControl() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[195], 1);
		return errors.New("websocket: WriteControl requires a control opcode")
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[187], 1);if len(data) > maxControlPayload {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[196], 1);
		return ErrControlTooLarge
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[188], 1);if c.closed.Load() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[197], 1);
		return ErrWriteClosed
	}

	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[189], 1);timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	// Acquire write lock with timeout (channel-based, no spin-wait).
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[190], 1);select {
	case <-c.writeSem:_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[198], 1);
		defer c.unlockWrite()
		c.getWriter()
		defer c.putWriter()
	case <-timer.C:_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[199], 1);
		return ErrWriteTimeout
	}

	// Std (hijack) path: pin the deadline to the underlying socket so a
	// blocked flush actually fails fast. Restore the previous (zero)
	// deadline on return so subsequent writes are unaffected.
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[191], 1);if c.conn != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[200], 1);
		_ = c.conn.SetWriteDeadline(deadline)
		defer func() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[201], 1); _ = c.conn.SetWriteDeadline(time.Time{}) }()
	}

	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[192], 1);if op == OpClose {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[202], 1);
		c.closeSent.Store(true)
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[193], 1);err := writeFrame(c.bw, true, op, data, c.writeHdr[:])
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[203], 1);
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[194], 1);return c.bw.Flush()
}

func (c *Conn) writePong(data []byte) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[204], 1);
	c.lockWrite()
	c.getWriter()
	defer c.putWriter()
	defer c.unlockWrite()
	if c.closed.Load() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[207], 1);
		return ErrWriteClosed
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[205], 1);err := writeFrame(c.bw, true, OpPong, data, c.writeHdr[:])
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[208], 1);
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[206], 1);return c.bw.Flush()
}

func (c *Conn) writeCloseFrame(code int, text string) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[209], 1);
	c.lockWrite()
	c.getWriter()
	defer c.putWriter()
	defer c.unlockWrite()
	c.closeSent.Store(true)
	err := writeCloseFrame(c.bw, code, text, c.writeHdr[:])
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[211], 1);
		return err
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[210], 1);return c.bw.Flush()
}

func (c *Conn) writeCloseProtocol(code int, text string) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[212], 1);
	_ = c.writeCloseFrame(code, text)
}

// GracefulClose sends a close frame and waits for the peer's response.
//
// On the hijack path the deadline is enforced via net.Conn.SetReadDeadline.
// On the engine path (chanReader-backed reads) net.Conn is nil, so a
// time.AfterFunc closes the engineReader to unblock ReadMessageReuse if
// the peer never sends its close frame.
func (c *Conn) GracefulClose(code int, text string) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[213], 1);
	err := c.writeCloseFrame(code, text)
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[219], 1);
		return c.Close()
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[214], 1);deadline := time.Now().Add(closeTimeout)
	if c.conn != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[220], 1);
		_ = c.conn.SetReadDeadline(deadline)
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[215], 1);var watchdog *time.Timer
	if c.engineReader != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[221], 1);
		watchdog = time.AfterFunc(closeTimeout, func() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[223], 1);
			c.engineReader.closeWith(io.EOF)
		})
		// Engine path also asks the engine's idle sweep to close the
		// connection on schedule; redundant with the watchdog but cheap.
		_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[222], 1);if c.idleDeadlineFn != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[224], 1);
			c.idleDeadlineFn(deadline.UnixNano())
		}
	}
	// Read until we get the close response or deadline.
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[216], 1);for {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[225], 1);
		_, _, rerr := c.ReadMessageReuse()
		if rerr != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[226], 1);
			break
		}
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[217], 1);if watchdog != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[227], 1);
		watchdog.Stop()
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[218], 1);return c.Close()
}

// Close closes the underlying connection.
//
// On the engine path (net.Conn unavailable), Close asks the engine to
// drop the FD on its next idle sweep by setting the WS idle deadline to
// the past. This is the server-side TCP close that RFC 6455 §7.1.1 and
// Autobahn 7.7.x (requireClean=True) require after the close handshake —
// without it the peer waits indefinitely for the server FIN. The
// underlying drain order (drainDetachQueue → flush writes → checkTimeouts
// → closeConn) guarantees any buffered close-frame echo reaches the wire
// before the FD is shut down.
func (c *Conn) Close() error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[228], 1);
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed.Load() {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[233], 1);
		return nil
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[229], 1);c.closed.Store(true)
	c.cancel()
	if c.engineReader != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[234], 1);
		c.engineReader.closeWith(io.EOF)
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[230], 1);if c.conn != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[235], 1);
		return c.conn.Close()
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[231], 1);if c.idleDeadlineFn != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[236], 1);
		c.idleDeadlineFn(1)
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[232], 1);return nil
}

// NetConn returns the underlying net.Conn.
func (c *Conn) NetConn() net.Conn {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[237], 1); return c.conn }

// Context returns the connection's context, cancelled when closed.
func (c *Conn) Context() context.Context {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[238], 1); return c.ctx }

// Subprotocol returns the negotiated subprotocol, or "" if none.
func (c *Conn) Subprotocol() string {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[239], 1); return c.subprotocol }

// RemoteAddr returns the peer's network address.
func (c *Conn) RemoteAddr() net.Addr {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[240], 1);
	if c.conn == nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[242], 1);
		return nil
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[241], 1);return c.conn.RemoteAddr()
}

// LocalAddr returns the local network address, if known.
func (c *Conn) LocalAddr() net.Addr {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[243], 1);
	if c.conn == nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[245], 1);
		return nil
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[244], 1);return c.conn.LocalAddr()
}

// BackpressureDropped returns the number of inbound chunks dropped because
// the engine-path read buffer overflowed despite TCP-level backpressure.
// Should be 0 in healthy operation. Returns 0 on the std (hijack) path,
// where backpressure is handled directly by the kernel TCP stack.
func (c *Conn) BackpressureDropped() uint64 {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[246], 1);
	if c.engineReader == nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[248], 1);
		return 0
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[247], 1);return c.engineReader.Dropped()
}

// IP returns the remote IP address (without port). The result is cached
// after the first call so per-message log loops don't re-parse
// RemoteAddr().String() on every iteration.
func (c *Conn) IP() string {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[249], 1);
	if c.cachedIP != "" {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[254], 1);
		return c.cachedIP
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[250], 1);if c.conn == nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[255], 1);
		return ""
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[251], 1);addr := c.conn.RemoteAddr()
	if addr == nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[256], 1);
		return ""
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[252], 1);host, _, err := net.SplitHostPort(addr.String())
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[257], 1);
		c.cachedIP = addr.String()
	} else{ _cover_atomic_.AddUint32(&GoCover_c633_conn.Count[258], 1);{
		c.cachedIP = host
	}}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[253], 1);return c.cachedIP
}

// Locals returns a per-connection value. Safe for concurrent use.
func (c *Conn) Locals(key string) any {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[259], 1);
	c.localsMu.RLock()
	defer c.localsMu.RUnlock()
	if c.locals == nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[261], 1);
		return nil
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[260], 1);return c.locals[key]
}

// SetLocals stores a per-connection value. Safe for concurrent use.
func (c *Conn) SetLocals(key string, val any) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[262], 1);
	c.localsMu.Lock()
	defer c.localsMu.Unlock()
	if c.locals == nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[264], 1);
		c.locals = make(map[string]any, 4)
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[263], 1);c.locals[key] = val
}

// Param returns a URL route parameter captured at upgrade time.
func (c *Conn) Param(key string) string {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[265], 1);
	for _, p := range c.params {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[267], 1);
		if p[0] == key {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[268], 1);
			return p[1]
		}
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[266], 1);return ""
}

// Query returns a query parameter captured at upgrade time.
func (c *Conn) Query(key string) string {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[269], 1);
	for _, q := range c.query {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[271], 1);
		if q[0] == key {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[272], 1);
			return q[1]
		}
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[270], 1);return ""
}

// Header returns a request header captured at upgrade time.
func (c *Conn) Header(key string) string {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[273], 1);
	for _, h := range c.headers {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[275], 1);
		if h[0] == key {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[276], 1);
			return h[1]
		}
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[274], 1);return ""
}

// SetReadLimit sets the maximum message size in bytes. The default is 64MB.
func (c *Conn) SetReadLimit(limit int64) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[277], 1); c.readLimit = limit }

// SetReadDeadline sets the deadline for future reads.
// Returns nil on engine-integrated connections where deadlines are not supported.
func (c *Conn) SetReadDeadline(t time.Time) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[278], 1);
	if c.conn == nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[280], 1);
		return nil
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[279], 1);return c.conn.SetReadDeadline(t)
}

// SetWriteDeadline sets the deadline for future writes.
// Returns nil on engine-integrated connections where deadlines are not supported.
func (c *Conn) SetWriteDeadline(t time.Time) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[281], 1);
	if c.conn == nil {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[283], 1);
		return nil
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[282], 1);return c.conn.SetWriteDeadline(t)
}

// SetPingHandler sets the handler for ping frames. The default handler
// replies with a pong containing the same payload.
func (c *Conn) SetPingHandler(h func(data []byte) error) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[284], 1); c.pingHandler = h }

// SetPongHandler sets the handler for pong frames. The default is a no-op.
func (c *Conn) SetPongHandler(h func(data []byte) error) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[285], 1); c.pongHandler = h }

// SetCloseHandler sets the handler for close frames. The default handler
// echoes the close frame back and returns a [CloseError].
func (c *Conn) SetCloseHandler(h func(code int, text string) error) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[286], 1); c.closeHandler = h }

// PingHandler returns the current ping handler.
func (c *Conn) PingHandler() func(data []byte) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[287], 1); return c.pingHandler }

// PongHandler returns the current pong handler.
func (c *Conn) PongHandler() func(data []byte) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[288], 1); return c.pongHandler }

// CloseHandler returns the current close handler.
func (c *Conn) CloseHandler() func(code int, text string) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[289], 1); return c.closeHandler }

// EnableWriteCompression enables or disables write compression for this
// connection. Compression must have been negotiated during the upgrade
// handshake; this only controls whether subsequent writes actually compress.
func (c *Conn) EnableWriteCompression(enable bool) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[290], 1);
	c.compressDisabled = !enable
}

// SetCompressionLevel sets the flate compression level for subsequent writes.
// Valid range: -2 (HuffmanOnly) to 9 (BestCompression), or -1 (DefaultCompression).
func (c *Conn) SetCompressionLevel(level int) error {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[291], 1);
	if level < minCompressionLevel || level > maxCompressionLevel {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[293], 1);
		return errors.New("websocket: invalid compression level")
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[292], 1);c.compressLevel = level
	return nil
}

// CloseError is returned when a close frame is received.
type CloseError struct {
	Code int
	Text string
}

func (e *CloseError) Error() string {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[294], 1);
	s := "websocket: close " + strconv.Itoa(e.Code)
	if e.Text != "" {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[296], 1);
		s += ": " + e.Text
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[295], 1);return s
}

// IsCloseError returns true if err is a CloseError with one of the given codes.
func IsCloseError(err error, codes ...int) bool {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[297], 1);
	var ce *CloseError
	if !errors.As(err, &ce) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[300], 1);
		return false
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[298], 1);for _, c := range codes {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[301], 1);
		if ce.Code == c {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[302], 1);
			return true
		}
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[299], 1);return false
}

// IsUnexpectedCloseError returns true if err is a CloseError whose code
// is NOT in the given list.
func IsUnexpectedCloseError(err error, expectedCodes ...int) bool {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[303], 1);
	var ce *CloseError
	if !errors.As(err, &ce) {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[306], 1);
		return false
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[304], 1);for _, c := range expectedCodes {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[307], 1);
		if ce.Code == c {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[308], 1);
			return false
		}
	}
	_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[305], 1);return true
}

// FormatCloseMessage creates a close frame payload with the given code and text.
func FormatCloseMessage(code int, text string) []byte {_cover_atomic_.AddUint32(&GoCover_c633_conn.Count[309], 1);
	buf := make([]byte, 2+len(text))
	binary.BigEndian.PutUint16(buf, uint16(code))
	copy(buf[2:], text)
	return buf
}

var GoCover_c633_conn = struct {
	Count     [310]uint32
	Pos       [3 * 310]uint32
	NumStmt   [310]uint16
} {
	Pos: [3 * 310]uint32{
		109, 109, 0x160002, // [0]
		112, 112, 0x170002, // [1]
		115, 124, 0x10002, // [2]
		125, 128, 0xb0002, // [3]
		110, 111, 0x10003, // [4]
		113, 114, 0x10003, // [5]
		137, 137, 0x160002, // [6]
		140, 148, 0x10002, // [7]
		149, 156, 0xb0002, // [8]
		138, 139, 0x10003, // [9]
		176, 176, 0x2b0002, // [10]
		179, 179, 0x1a0002, // [11]
		182, 182, 0x210002, // [12]
		188, 189, 0x140002, // [13]
		177, 178, 0x10003, // [14]
		180, 181, 0x10003, // [15]
		186, 187, 0x10003, // [16]
		196, 197, 0x10002, // [17]
		199, 199, 0x2e0020, // [18]
		200, 200, 0x3a0020, // [19]
		215, 215, 0x240002, // [20]
		218, 218, 0x100002, // [21]
		221, 222, 0x130002, // [22]
		216, 217, 0x10003, // [23]
		219, 220, 0x10003, // [24]
		230, 230, 0x330002, // [25]
		233, 235, 0x140002, // [26]
		231, 232, 0x10003, // [27]
		242, 243, 0x10002, // [28]
		245, 246, 0x100002, // [29]
		251, 259, 0x10002, // [30]
		261, 262, 0x100002, // [31]
		268, 268, 0xe0002, // [32]
		279, 280, 0x300002, // [33]
		284, 285, 0x270002, // [34]
		329, 329, 0x1a0002, // [35]
		248, 249, 0x10003, // [36]
		264, 264, 0x120003, // [37]
		266, 266, 0x120003, // [38]
		269, 270, 0x10003, // [39]
		281, 282, 0x10003, // [40]
		287, 288, 0x110003, // [41]
		290, 291, 0x100004, // [42]
		295, 296, 0x10004, // [43]
		298, 298, 0x440004, // [44]
		303, 303, 0x110004, // [45]
		308, 308, 0x1e0004, // [46]
		315, 316, 0x1e0004, // [47]
		319, 322, 0x10004, // [48]
		323, 324, 0x170004, // [49]
		292, 294, 0x10005, // [50]
		299, 302, 0x10005, // [51]
		304, 307, 0x10005, // [52]
		309, 312, 0x10005, // [53]
		317, 318, 0x10005, // [54]
		342, 343, 0x3c0002, // [55]
		346, 346, 0x420002, // [56]
		350, 350, 0xf0002, // [57]
		354, 354, 0x1c0002, // [58]
		358, 359, 0x1c0002, // [59]
		362, 363, 0x10002, // [60]
		364, 366, 0x140002, // [61]
		390, 390, 0x360002, // [62]
		393, 394, 0x150002, // [63]
		344, 345, 0x10003, // [64]
		347, 349, 0x10003, // [65]
		351, 353, 0x10003, // [66]
		355, 357, 0x10003, // [67]
		360, 361, 0x10003, // [68]
		367, 367, 0x190003, // [69]
		370, 371, 0xf0003, // [70]
		387, 387, 0x160003, // [71]
		368, 369, 0x10004, // [72]
		372, 373, 0x120004, // [73]
		376, 376, 0x100004, // [74]
		379, 381, 0x350004, // [75]
		385, 385, 0xe0004, // [76]
		374, 375, 0x10005, // [77]
		377, 377, 0xd0005, // [78]
		382, 384, 0x10005, // [79]
		391, 392, 0x10003, // [80]
		403, 404, 0x100002, // [81]
		408, 410, 0x170002, // [82]
		405, 406, 0x10003, // [83]
		420, 420, 0x60002, // [84]
		424, 424, 0x180003, // [85]
		436, 437, 0x110003, // [86]
		440, 441, 0x10003, // [87]
		443, 443, 0x1b0003, // [88]
		451, 451, 0x210003, // [89]
		496, 496, 0x160003, // [90]
		501, 501, 0x420003, // [91]
		505, 506, 0x10003, // [92]
		507, 507, 0xd0003, // [93]
		512, 516, 0x10003, // [94]
		518, 518, 0x170003, // [95]
		534, 534, 0x3b0003, // [96]
		539, 539, 0x160003, // [97]
		425, 426, 0x150004, // [98]
		429, 429, 0x1f0004, // [99]
		427, 428, 0x10005, // [100]
		430, 431, 0x10005, // [101]
		438, 439, 0x10004, // [102]
		444, 444, 0x3d0004, // [103]
		447, 447, 0xc0004, // [104]
		445, 446, 0x10005, // [105]
		453, 453, 0x170004, // [106]
		458, 459, 0x10004, // [107]
		460, 460, 0xd0004, // [108]
		490, 492, 0xc0004, // [109]
		454, 456, 0x10005, // [110]
		462, 462, 0x190005, // [111]
		484, 484, 0x220005, // [112]
		463, 465, 0x140006, // [113]
		477, 477, 0x340006, // [114]
		466, 466, 0x1e0007, // [115]
		471, 471, 0x190007, // [116]
		467, 468, 0x10008, // [117]
		469, 470, 0x10008, // [118]
		478, 480, 0x10007, // [119]
		497, 499, 0x10004, // [120]
		502, 504, 0x10004, // [121]
		508, 508, 0xc0004, // [122]
		519, 521, 0x130004, // [123]
		522, 522, 0x1d0005, // [124]
		527, 527, 0x180005, // [125]
		523, 524, 0x10006, // [126]
		525, 526, 0x10006, // [127]
		535, 537, 0x10004, // [128]
		544, 544, 0xc0002, // [129]
		558, 558, 0xc0002, // [130]
		546, 546, 0x1b0003, // [131]
		549, 549, 0x1e0003, // [132]
		551, 551, 0x1b0003, // [133]
		554, 554, 0xd0003, // [134]
		556, 556, 0x250003, // [135]
		547, 548, 0x10004, // [136]
		552, 553, 0x10004, // [137]
		562, 565, 0x10002, // [138]
		566, 567, 0x100002, // [139]
		572, 572, 0x1b0002, // [140]
		580, 580, 0x190002, // [141]
		587, 587, 0x2c0002, // [142]
		568, 570, 0x10003, // [143]
		573, 574, 0x10003, // [144]
		581, 581, 0x180003, // [145]
		582, 583, 0x10004, // [146]
		584, 585, 0x10004, // [147]
		591, 592, 0x10002, // [148]
		600, 600, 0x150002, // [149]
		603, 603, 0x1a0002, // [150]
		610, 612, 0x6a0002, // [151]
		626, 630, 0x10002, // [152]
		631, 631, 0x150002, // [153]
		636, 637, 0x100002, // [154]
		642, 643, 0x130002, // [155]
		661, 662, 0x100002, // [156]
		665, 665, 0x150002, // [157]
		601, 602, 0x10003, // [158]
		604, 605, 0x10003, // [159]
		613, 614, 0x480003, // [160]
		623, 623, 0x230003, // [161]
		616, 616, 0x240004, // [162]
		617, 619, 0x10005, // [163]
		632, 633, 0x10003, // [164]
		638, 639, 0x10003, // [165]
		644, 649, 0x390003, // [166]
		652, 652, 0x110003, // [167]
		657, 657, 0x160003, // [168]
		650, 651, 0x10004, // [169]
		653, 653, 0x2e0004, // [170]
		654, 655, 0x10005, // [171]
		663, 664, 0x10003, // [172]
		670, 671, 0x10002, // [173]
		675, 676, 0x10002, // [174]
		680, 681, 0x100002, // [175]
		684, 684, 0x2a0002, // [176]
		682, 683, 0x10003, // [177]
		689, 690, 0x100002, // [178]
		693, 693, 0x200002, // [179]
		691, 692, 0x10003, // [180]
		699, 703, 0x150002, // [181]
		706, 707, 0x100002, // [182]
		710, 710, 0x150002, // [183]
		704, 705, 0x10003, // [184]
		708, 709, 0x10003, // [185]
		727, 728, 0x150002, // [186]
		731, 731, 0x230002, // [187]
		734, 734, 0x150002, // [188]
		738, 740, 0x10002, // [189]
		742, 742, 0x90002, // [190]
		754, 754, 0x130002, // [191]
		759, 759, 0x130002, // [192]
		762, 763, 0x100002, // [193]
		766, 766, 0x150002, // [194]
		729, 730, 0x10003, // [195]
		732, 733, 0x10003, // [196]
		735, 736, 0x10003, // [197]
		744, 746, 0x160003, // [198]
		748, 748, 0x190003, // [199]
		755, 756, 0x100003, // [200]
		756, 756, 0x3c0012, // [201]
		760, 761, 0x10003, // [202]
		764, 765, 0x10003, // [203]
		770, 774, 0x150002, // [204]
		777, 778, 0x100002, // [205]
		781, 781, 0x150002, // [206]
		775, 776, 0x10003, // [207]
		779, 780, 0x10003, // [208]
		785, 791, 0x100002, // [209]
		794, 794, 0x150002, // [210]
		792, 793, 0x10003, // [211]
		798, 799, 0x10002, // [212]
		808, 809, 0x100002, // [213]
		812, 813, 0x130002, // [214]
		816, 817, 0x1b0002, // [215]
		828, 828, 0x60002, // [216]
		834, 834, 0x150002, // [217]
		837, 837, 0x120002, // [218]
		810, 811, 0x10003, // [219]
		814, 815, 0x10003, // [220]
		818, 818, 0x320003, // [221]
		823, 823, 0x1e0003, // [222]
		819, 820, 0x10004, // [223]
		824, 825, 0x10004, // [224]
		829, 830, 0x120003, // [225]
		831, 831, 0x90004, // [226]
		835, 836, 0x10003, // [227]
		851, 853, 0x150002, // [228]
		856, 858, 0x1b0002, // [229]
		861, 861, 0x130002, // [230]
		864, 864, 0x1d0002, // [231]
		867, 867, 0xc0002, // [232]
		854, 855, 0x10003, // [233]
		859, 860, 0x10003, // [234]
		862, 863, 0x10003, // [235]
		865, 866, 0x10003, // [236]
		871, 871, 0x340025, // [237]
		874, 874, 0x3a002c, // [238]
		877, 877, 0x3d0027, // [239]
		881, 881, 0x130002, // [240]
		884, 884, 0x1c0002, // [241]
		882, 883, 0x10003, // [242]
		889, 889, 0x130002, // [243]
		892, 892, 0x1b0002, // [244]
		890, 891, 0x10003, // [245]
		900, 900, 0x1b0002, // [246]
		903, 903, 0x210002, // [247]
		901, 902, 0x10003, // [248]
		910, 910, 0x160002, // [249]
		913, 913, 0x130002, // [250]
		916, 917, 0x110002, // [251]
		920, 921, 0x100002, // [252]
		926, 926, 0x130002, // [253]
		911, 912, 0x10003, // [254]
		914, 915, 0x10003, // [255]
		918, 919, 0x10003, // [256]
		922, 923, 0x10003, // [257]
		924, 925, 0x10003, // [258]
		931, 933, 0x150002, // [259]
		936, 936, 0x160002, // [260]
		934, 935, 0x10003, // [261]
		941, 943, 0x150002, // [262]
		946, 946, 0x150002, // [263]
		944, 945, 0x10003, // [264]
		951, 951, 0x1d0002, // [265]
		956, 956, 0xb0002, // [266]
		952, 952, 0x120003, // [267]
		953, 954, 0x10004, // [268]
		961, 961, 0x1c0002, // [269]
		966, 966, 0xb0002, // [270]
		962, 962, 0x120003, // [271]
		963, 964, 0x10004, // [272]
		971, 971, 0x1e0002, // [273]
		976, 976, 0xb0002, // [274]
		972, 972, 0x120003, // [275]
		973, 974, 0x10004, // [276]
		980, 980, 0x41002c, // [277]
		985, 985, 0x130002, // [278]
		988, 988, 0x220002, // [279]
		986, 987, 0x10003, // [280]
		994, 994, 0x130002, // [281]
		997, 997, 0x230002, // [282]
		995, 996, 0x10003, // [283]
		1002, 1002, 0x4f003c, // [284]
		1005, 1005, 0x4f003c, // [285]
		1009, 1009, 0x5b0047, // [286]
		1012, 1012, 0x4e0038, // [287]
		1015, 1015, 0x4e0038, // [288]
		1018, 1018, 0x5a0043, // [289]
		1024, 1025, 0x10002, // [290]
		1030, 1030, 0x400002, // [291]
		1033, 1034, 0xc0002, // [292]
		1031, 1032, 0x10003, // [293]
		1044, 1045, 0x120002, // [294]
		1048, 1048, 0xa0002, // [295]
		1046, 1047, 0x10003, // [296]
		1053, 1054, 0x1a0002, // [297]
		1057, 1057, 0x1a0002, // [298]
		1062, 1062, 0xe0002, // [299]
		1055, 1056, 0x10003, // [300]
		1058, 1058, 0x130003, // [301]
		1059, 1060, 0x10004, // [302]
		1068, 1069, 0x1a0002, // [303]
		1072, 1072, 0x220002, // [304]
		1077, 1077, 0xd0002, // [305]
		1070, 1071, 0x10003, // [306]
		1073, 1073, 0x130003, // [307]
		1074, 1075, 0x10004, // [308]
		1082, 1086, 0x10002, // [309]
	},
	NumStmt: [310]uint16{
		1, // 0
		1, // 1
		5, // 2
		5, // 3
		1, // 4
		1, // 5
		1, // 6
		9, // 7
		9, // 8
		1, // 9
		1, // 10
		1, // 11
		1, // 12
		2, // 13
		1, // 14
		1, // 15
		1, // 16
		1, // 17
		1, // 18
		1, // 19
		1, // 20
		1, // 21
		2, // 22
		1, // 23
		1, // 24
		1, // 25
		3, // 26
		1, // 27
		3, // 28
		3, // 29
		10, // 30
		10, // 31
		1, // 32
		2, // 33
		2, // 34
		1, // 35
		1, // 36
		1, // 37
		1, // 38
		1, // 39
		1, // 40
		2, // 41
		2, // 42
		2, // 43
		2, // 44
		1, // 45
		1, // 46
		2, // 47
		5, // 48
		5, // 49
		2, // 50
		3, // 51
		3, // 52
		3, // 53
		1, // 54
		2, // 55
		1, // 56
		1, // 57
		1, // 58
		2, // 59
		3, // 60
		3, // 61
		1, // 62
		2, // 63
		1, // 64
		2, // 65
		2, // 66
		2, // 67
		1, // 68
		1, // 69
		2, // 70
		1, // 71
		1, // 72
		2, // 73
		1, // 74
		3, // 75
		1, // 76
		1, // 77
		1, // 78
		2, // 79
		1, // 80
		2, // 81
		3, // 82
		1, // 83
		1, // 84
		1, // 85
		2, // 86
		2, // 87
		2, // 88
		1, // 89
		1, // 90
		1, // 91
		2, // 92
		2, // 93
		5, // 94
		5, // 95
		1, // 96
		1, // 97
		2, // 98
		1, // 99
		1, // 100
		1, // 101
		1, // 102
		1, // 103
		1, // 104
		1, // 105
		1, // 106
		2, // 107
		2, // 108
		3, // 109
		2, // 110
		1, // 111
		1, // 112
		3, // 113
		1, // 114
		1, // 115
		1, // 116
		1, // 117
		1, // 118
		2, // 119
		2, // 120
		2, // 121
		1, // 122
		3, // 123
		1, // 124
		1, // 125
		1, // 126
		1, // 127
		2, // 128
		1, // 129
		1, // 130
		1, // 131
		1, // 132
		1, // 133
		1, // 134
		1, // 135
		1, // 136
		1, // 137
		5, // 138
		5, // 139
		1, // 140
		1, // 141
		1, // 142
		2, // 143
		1, // 144
		1, // 145
		1, // 146
		1, // 147
		1, // 148
		1, // 149
		1, // 150
		3, // 151
		5, // 152
		5, // 153
		2, // 154
		2, // 155
		2, // 156
		1, // 157
		1, // 158
		1, // 159
		2, // 160
		1, // 161
		1, // 162
		2, // 163
		1, // 164
		1, // 165
		6, // 166
		1, // 167
		1, // 168
		1, // 169
		1, // 170
		1, // 171
		1, // 172
		1, // 173
		1, // 174
		2, // 175
		1, // 176
		1, // 177
		2, // 178
		1, // 179
		1, // 180
		5, // 181
		2, // 182
		1, // 183
		1, // 184
		1, // 185
		2, // 186
		1, // 187
		1, // 188
		3, // 189
		3, // 190
		1, // 191
		1, // 192
		2, // 193
		1, // 194
		1, // 195
		1, // 196
		1, // 197
		3, // 198
		1, // 199
		2, // 200
		1, // 201
		1, // 202
		1, // 203
		5, // 204
		2, // 205
		1, // 206
		1, // 207
		1, // 208
		7, // 209
		1, // 210
		1, // 211
		1, // 212
		2, // 213
		2, // 214
		2, // 215
		1, // 216
		1, // 217
		1, // 218
		1, // 219
		1, // 220
		1, // 221
		1, // 222
		1, // 223
		1, // 224
		2, // 225
		1, // 226
		1, // 227
		3, // 228
		3, // 229
		1, // 230
		1, // 231
		1, // 232
		1, // 233
		1, // 234
		1, // 235
		1, // 236
		1, // 237
		1, // 238
		1, // 239
		1, // 240
		1, // 241
		1, // 242
		1, // 243
		1, // 244
		1, // 245
		1, // 246
		1, // 247
		1, // 248
		1, // 249
		1, // 250
		2, // 251
		2, // 252
		1, // 253
		1, // 254
		1, // 255
		1, // 256
		1, // 257
		1, // 258
		3, // 259
		1, // 260
		1, // 261
		3, // 262
		1, // 263
		1, // 264
		1, // 265
		1, // 266
		1, // 267
		1, // 268
		1, // 269
		1, // 270
		1, // 271
		1, // 272
		1, // 273
		1, // 274
		1, // 275
		1, // 276
		1, // 277
		1, // 278
		1, // 279
		1, // 280
		1, // 281
		1, // 282
		1, // 283
		1, // 284
		1, // 285
		1, // 286
		1, // 287
		1, // 288
		1, // 289
		1, // 290
		1, // 291
		2, // 292
		1, // 293
		2, // 294
		1, // 295
		1, // 296
		2, // 297
		1, // 298
		1, // 299
		1, // 300
		1, // 301
		1, // 302
		2, // 303
		1, // 304
		1, // 305
		1, // 306
		1, // 307
		1, // 308
		4, // 309
	},
}

var _ = _cover_atomic_.LoadUint32
