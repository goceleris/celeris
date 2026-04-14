package websocket

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
}

// newConn creates a Conn from a raw net.Conn (hijack path — used by std engine).
func newConn(ctx context.Context, cancel context.CancelFunc, c net.Conn, readBufSize, writeBufSize int) *Conn {
	if readBufSize <= 0 {
		readBufSize = defaultReadBufSize
	}
	if writeBufSize <= 0 {
		writeBufSize = defaultWriteBufSize
	}
	ws := &Conn{
		conn:      c,
		br:        bufio.NewReaderSize(c, readBufSize),
		bwDst:     c,
		bw:        bufio.NewWriterSize(c, writeBufSize),
		ctx:       ctx,
		cancel:    cancel,
		readBuf:   make([]byte, maxHeaderSize),
		readLimit: defaultReadLimit,
	}
	ws.writeSem = make(chan struct{}, 1)
	ws.writeSem <- struct{}{}
	ws.pingHandler = ws.defaultPingHandler
	return ws
}

// newEngineConn creates a Conn using engine-integrated I/O: reads come from
// a chanReader fed by the event loop, writes go through the engine's
// write buffer via writeFn. The connection is detached from the engine's
// HTTP parser at this point.
func newEngineConn(ctx context.Context, cancel context.CancelFunc, reader *chanReader, writeFn func([]byte),
	readBufSize int) *Conn {
	if readBufSize <= 0 {
		readBufSize = defaultReadBufSize
	}
	ws := &Conn{
		ctx:           ctx,
		cancel:        cancel,
		readBuf:       make([]byte, maxHeaderSize),
		readLimit:     defaultReadLimit,
		engine:        true,
		engineReader:  reader,
		engineWriteFn: writeFn,
	}
	ws.br = bufio.NewReaderSize(reader, readBufSize)
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

func (w *engineWriter) Write(p []byte) (int, error) {
	if e := w.conn.writeErr.Load(); e != nil {
		return 0, e.(error)
	}
	if w.conn.closed.Load() {
		return 0, ErrWriteClosed
	}
	if w.conn.engineWriteFn == nil {
		// Conn was constructed pre-Detach (race-window safety in
		// tryEngineUpgrade); the WS handler must not write before
		// setRawWrite has been called.
		return 0, ErrWriteClosed
	}
	w.conn.engineWriteFn(p)
	return len(p), nil
}

// setRawWrite finishes wiring the engine raw-write function into the
// Conn. Used by tryEngineUpgrade to install the engine-provided rawWrite
// AFTER OnError is registered, so a pre-Detach race cannot lose errors.
func (c *Conn) setRawWrite(fn func([]byte)) {
	c.engineWriteFn = fn
}

func (c *Conn) lockWrite()   { <-c.writeSem }
func (c *Conn) unlockWrite() { c.writeSem <- struct{}{} }

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
func (c *Conn) getWriter() {
	if c.writePool == nil || c.engine {
		return
	}
	if c.bwPooled {
		return
	}
	c.bw = c.writePool.Get(c.bwDst)
	c.bwPooled = true
}

// putWriter returns a borrowed pool writer. No-op when no pool is
// configured, on the engine path, or when the current bw is not pooled.
// Must be called under the write lock, after Flush. The per-conn bw is
// restored to nil so the next getWriter borrows fresh.
func (c *Conn) putWriter() {
	if c.writePool == nil || c.engine || !c.bwPooled {
		return
	}
	c.writePool.Put(c.bw)
	c.bw = nil
	c.bwPooled = false
}

// readFrameFast attempts to read an entire frame from the bufio buffer with
// minimal function calls. Falls back to the multi-call path for large frames
// or when the bufio buffer doesn't have enough data.
func (c *Conn) readFrameFast() (payload []byte, err error) {
	h := &c.readHdr

	// Peek 2 bytes to determine frame header size.
	hdr, err := c.br.Peek(2)
	if err != nil {
		// Fall back to io.ReadFull for partial data.
		return c.readFrameSlow()
	}

	b0, b1 := hdr[0], hdr[1]
	h.Fin = b0&0x80 != 0
	h.RSV1 = b0&0x40 != 0
	h.RSV2 = b0&0x20 != 0
	h.RSV3 = b0&0x10 != 0
	h.Opcode = Opcode(b0 & 0x0F)
	h.Masked = b1&0x80 != 0
	payLen := int64(b1 & 0x7F)

	// Calculate total header size needed.
	headerSize := 2
	switch payLen {
	case 126:
		headerSize += 2
	case 127:
		headerSize += 8
	}
	if h.Masked {
		headerSize += 4
	}

	// For small frames: try to peek header + payload in one call.
	totalSize := headerSize + int(payLen)
	if payLen < 126 && totalSize <= 4096 {
		// Try to get the entire frame in one peek.
		buf, err := c.br.Peek(totalSize)
		if err == nil {
			// Parse everything inline from the peeked buffer.
			pos := 2
			if h.Masked {
				copy(h.Mask[:], buf[pos:pos+4])
				pos += 4
			}
			h.Length = payLen

			// Validate.
			if err := validateFrameHeader(h, c.compressEnabled); err != nil {
				_, _ = c.br.Discard(totalSize)
				c.writeCloseProtocol(CloseProtocolError, err.Error())
				return nil, err
			}
			if !h.Masked {
				_, _ = c.br.Discard(totalSize)
				c.writeCloseProtocol(CloseProtocolError, "unmasked client frame")
				return nil, ErrProtocol
			}
			if h.Length > c.readLimit {
				_, _ = c.br.Discard(totalSize)
				c.writeCloseProtocol(CloseMessageTooBig, "message too large")
				return nil, ErrReadLimit
			}

			// Copy payload into reusable buffer and unmask.
			n := int(payLen)
			if cap(c.readPayload) < n {
				c.readPayload = make([]byte, n)
			}
			payload = c.readPayload[:n]
			copy(payload, buf[pos:pos+n])
			maskBytes(h.Mask, payload)

			_, _ = c.br.Discard(totalSize)
			return payload, nil
		}
		// Peek failed — not enough data buffered. Fall through to slow path.
	}

	return c.readFrameSlow()
}

// readFrameSlow reads a frame using multiple io.ReadFull calls (for large
// frames or when the fast peek path fails).
func (c *Conn) readFrameSlow() ([]byte, error) {
	h := &c.readHdr
	if err := readFrameHeader(c.br, c.readBuf, h); err != nil {
		return nil, err
	}
	if err := validateFrameHeader(h, c.compressEnabled); err != nil {
		c.writeCloseProtocol(CloseProtocolError, err.Error())
		return nil, err
	}
	if !h.Masked {
		c.writeCloseProtocol(CloseProtocolError, "unmasked client frame")
		return nil, ErrProtocol
	}
	if h.Length > c.readLimit {
		c.writeCloseProtocol(CloseMessageTooBig, "message too large")
		return nil, ErrReadLimit
	}
	n := int(h.Length)
	if cap(c.readPayload) < n {
		c.readPayload = make([]byte, n)
	}
	payload := c.readPayload[:n]
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return nil, err
	}
	maskBytes(h.Mask, payload)
	return payload, nil
}

// ReadMessage reads the next complete message from the connection.
// Returns the message type and an owned copy of the payload. The returned
// slice is safe to retain, pass to other goroutines, or store.
//
// For zero-allocation reads (advanced usage), use [Conn.ReadMessageReuse].
func (c *Conn) ReadMessage() (MessageType, []byte, error) {
	mt, data, err := c.ReadMessageReuse()
	if err != nil {
		return 0, nil, err
	}
	// Return an owned copy so the caller can safely retain it.
	owned := make([]byte, len(data))
	copy(owned, data)
	return mt, owned, nil
}

// ReadMessageReuse reads the next complete message from the connection.
// The returned byte slice is reused across calls and is only valid until
// the next call to ReadMessageReuse or ReadMessage.
//
// Use this for zero-allocation reads when you process each message
// immediately without retaining the slice.
func (c *Conn) ReadMessageReuse() (MessageType, []byte, error) {
	for {
		// Apply idle timeout before blocking on read.
		// Std (hijack) path: net.Conn.SetReadDeadline.
		// Engine path: extend the engine's idle deadline via SetWSIdleDeadline.
		if c.idleTimeout > 0 {
			deadline := time.Now().Add(c.idleTimeout)
			if c.conn != nil {
				_ = c.conn.SetReadDeadline(deadline)
			}
			if c.idleDeadlineFn != nil {
				c.idleDeadlineFn(deadline.UnixNano())
			}
		}

		// Fast path: try to read the entire frame from the bufio buffer
		// with a single Peek, avoiding multiple io.ReadFull calls.
		payload, err := c.readFrameFast()
		if err != nil {
			return 0, nil, err
		}
		h := &c.readHdr

		// Handle control frames.
		if h.Opcode.IsControl() {
			if err := c.handleControl(h.Opcode, payload); err != nil {
				return 0, nil, err
			}
			continue
		}

		// Handle data frames with fragmentation (loop-based, no recursion).
		if h.Opcode != OpContinuation {
			// New data frame while fragmentation is in progress → protocol error.
			if c.readFrag != 0 {
				c.writeCloseProtocol(CloseProtocolError, "interleaved data frame")
				return 0, nil, ErrProtocol
			}
			// Track whether this message is compressed (RSV1 on first frame).
			c.readCompressed = h.RSV1

			if h.Fin {
				// Single unfragmented message — decompress if needed.
				if c.readCompressed {
					var err error
					payload, err = decompressMessage(payload, c.readLimit)
					if err != nil {
						if err == ErrReadLimit {
							c.writeCloseProtocol(CloseMessageTooBig, "decompressed message too large")
						} else {
							c.writeCloseProtocol(CloseProtocolError, "decompression error")
						}
						return 0, nil, err
					}
				}
				if h.Opcode == OpText && !utf8.Valid(payload) {
					c.writeCloseProtocol(CloseInvalidPayload, "invalid UTF-8")
					return 0, nil, ErrInvalidUTF8
				}
				return h.Opcode, payload, nil
			}
			// Start of fragmented message.
			c.readFrag = h.Opcode
			c.readFragBuf = append(c.readFragBuf[:0], payload...)
			continue // read next frame
		}

		// Continuation frame.
		if c.readFrag == 0 {
			c.writeCloseProtocol(CloseProtocolError, "unexpected continuation")
			return 0, nil, ErrProtocol
		}

		if int64(len(c.readFragBuf))+int64(len(payload)) > c.readLimit {
			c.writeCloseProtocol(CloseMessageTooBig, "message too large")
			return 0, nil, ErrReadLimit
		}
		c.readFragBuf = append(c.readFragBuf, payload...)

		if !h.Fin {
			continue // more fragments coming
		}

		// Final fragment — assemble complete message.
		op := c.readFrag
		c.readFrag = 0
		msg := append([]byte(nil), c.readFragBuf...)
		c.readFragBuf = c.readFragBuf[:0]

		// Decompress fragmented message if RSV1 was set on first frame.
		if c.readCompressed {
			var derr error
			msg, derr = decompressMessage(msg, c.readLimit)
			if derr != nil {
				if derr == ErrReadLimit {
					c.writeCloseProtocol(CloseMessageTooBig, "decompressed message too large")
				} else {
					c.writeCloseProtocol(CloseProtocolError, "decompression error")
				}
				return 0, nil, derr
			}
		}

		if op == OpText && !utf8.Valid(msg) {
			c.writeCloseProtocol(CloseInvalidPayload, "invalid UTF-8")
			return 0, nil, ErrInvalidUTF8
		}

		return op, msg, nil
	}
}

func (c *Conn) handleControl(op Opcode, payload []byte) error {
	switch op {
	case OpPing:
		if c.pingHandler != nil {
			return c.pingHandler(payload)
		}
		return c.writePong(payload)
	case OpPong:
		if c.pongHandler != nil {
			return c.pongHandler(payload)
		}
		return nil
	case OpClose:
		return c.handleCloseFrame(payload)
	}
	return nil
}

func (c *Conn) handleCloseFrame(payload []byte) error {
	c.closeMu.Lock()
	c.closeRecv = true
	c.closeMu.Unlock()

	code, text, err := parseClosePayload(payload)
	if err != nil {
		code = CloseProtocolError
		text = err.Error()
	}

	if c.closeHandler != nil {
		return c.closeHandler(code, text)
	}

	// Default: echo close frame back.
	if !c.closeSent.Load() {
		_ = c.writeCloseFrame(code, text)
	}
	return &CloseError{Code: code, Text: text}
}

func (c *Conn) defaultPingHandler(data []byte) error {
	return c.writePong(data)
}

// WriteMessage writes a complete message to the connection.
// If compression is negotiated and the payload exceeds the compression
// threshold, the message is compressed transparently. Compression runs
// before the write lock is taken so that ping/pong control frames can
// interleave with large compressed data writes.
func (c *Conn) WriteMessage(messageType MessageType, data []byte) error {
	if c.closed.Load() {
		return ErrWriteClosed
	}
	if c.fragWriting.Load() {
		return errors.New("websocket: cannot call WriteMessage while NextWriter is active")
	}

	// Compress data frames if compression is negotiated and payload is
	// large enough. This intentionally runs OUTSIDE the write lock so
	// concurrent control frames can interleave during expensive deflates.
	compressed := false
	var pooled *compressBuf
	if c.compressEnabled && !c.compressDisabled && messageType.IsData() && len(data) >= c.compressThreshold {
		pooled = acquireCompressBuf()
		if err := compressMessage(pooled, data, c.compressLevel); err == nil {
			// Only use compressed version if it's actually smaller.
			if len(pooled.data) < len(data) {
				data = pooled.data
				compressed = true
			}
		}
		// Release pool buffer after we're done with it (deferred so the
		// data slice remains valid for the duration of the write).
		defer releaseCompressBuf(pooled)
	}

	c.lockWrite()
	defer c.unlockWrite()
	c.getWriter()
	defer c.putWriter()

	if c.closed.Load() {
		return ErrWriteClosed
	}

	// Build frame header byte 0.
	b0 := byte(0x80) | byte(messageType&0x0F) // FIN + opcode
	if compressed {
		b0 |= 0x40 // RSV1 = compressed
	}

	// Fast path: small uncompressed frames.
	length := len(data)
	if length <= 125 {
		pos := 0
		c.writeHdr[pos] = b0
		pos++
		c.writeHdr[pos] = byte(length)
		pos++
		if _, err := c.bw.Write(c.writeHdr[:pos]); err != nil {
			return err
		}
		if length > 0 {
			if _, err := c.bw.Write(data); err != nil {
				return err
			}
		}
		return c.bw.Flush()
	}

	// General path with custom b0 for RSV1.
	err := writeFrameRaw(c.bw, b0, data, c.writeHdr[:])
	if err != nil {
		return err
	}
	return c.bw.Flush()
}

// WriteText writes a text message.
func (c *Conn) WriteText(data []byte) error {
	return c.WriteMessage(TextMessage, data)
}

// WriteBinary writes a binary message.
func (c *Conn) WriteBinary(data []byte) error {
	return c.WriteMessage(BinaryMessage, data)
}

// WriteJSON marshals v as JSON and writes it as a text message.
func (c *Conn) WriteJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.WriteMessage(TextMessage, data)
}

// ReadJSON reads the next message and unmarshals it from JSON.
func (c *Conn) ReadJSON(v any) error {
	_, data, err := c.ReadMessageReuse()
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// WritePing sends a ping control frame. The payload must be <= 125 bytes.
// Use this with [Conn.SetPongHandler] to implement keepalive.
func (c *Conn) WritePing(data []byte) error {
	c.lockWrite()
	c.getWriter()
	defer c.putWriter()
	defer c.unlockWrite()
	if c.closed.Load() {
		return ErrWriteClosed
	}
	err := writeFrame(c.bw, true, OpPing, data, c.writeHdr[:])
	if err != nil {
		return err
	}
	return c.bw.Flush()
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
func (c *Conn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	op := Opcode(messageType)
	if !op.IsControl() {
		return errors.New("websocket: WriteControl requires a control opcode")
	}
	if len(data) > maxControlPayload {
		return ErrControlTooLarge
	}
	if c.closed.Load() {
		return ErrWriteClosed
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	// Acquire write lock with timeout (channel-based, no spin-wait).
	select {
	case <-c.writeSem:
		defer c.unlockWrite()
		c.getWriter()
		defer c.putWriter()
	case <-timer.C:
		return ErrWriteTimeout
	}

	// Std (hijack) path: pin the deadline to the underlying socket so a
	// blocked flush actually fails fast. Restore the previous (zero)
	// deadline on return so subsequent writes are unaffected.
	if c.conn != nil {
		_ = c.conn.SetWriteDeadline(deadline)
		defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()
	}

	if op == OpClose {
		c.closeSent.Store(true)
	}
	err := writeFrame(c.bw, true, op, data, c.writeHdr[:])
	if err != nil {
		return err
	}
	return c.bw.Flush()
}

func (c *Conn) writePong(data []byte) error {
	c.lockWrite()
	c.getWriter()
	defer c.putWriter()
	defer c.unlockWrite()
	if c.closed.Load() {
		return ErrWriteClosed
	}
	err := writeFrame(c.bw, true, OpPong, data, c.writeHdr[:])
	if err != nil {
		return err
	}
	return c.bw.Flush()
}

func (c *Conn) writeCloseFrame(code int, text string) error {
	c.lockWrite()
	c.getWriter()
	defer c.putWriter()
	defer c.unlockWrite()
	c.closeSent.Store(true)
	err := writeCloseFrame(c.bw, code, text, c.writeHdr[:])
	if err != nil {
		return err
	}
	return c.bw.Flush()
}

func (c *Conn) writeCloseProtocol(code int, text string) {
	_ = c.writeCloseFrame(code, text)
}

// GracefulClose sends a close frame and waits for the peer's response.
//
// On the hijack path the deadline is enforced via net.Conn.SetReadDeadline.
// On the engine path (chanReader-backed reads) net.Conn is nil, so a
// time.AfterFunc closes the engineReader to unblock ReadMessageReuse if
// the peer never sends its close frame.
func (c *Conn) GracefulClose(code int, text string) error {
	err := c.writeCloseFrame(code, text)
	if err != nil {
		return c.Close()
	}
	deadline := time.Now().Add(closeTimeout)
	if c.conn != nil {
		_ = c.conn.SetReadDeadline(deadline)
	}
	var watchdog *time.Timer
	if c.engineReader != nil {
		watchdog = time.AfterFunc(closeTimeout, func() {
			c.engineReader.closeWith(io.EOF)
		})
		// Engine path also asks the engine's idle sweep to close the
		// connection on schedule; redundant with the watchdog but cheap.
		if c.idleDeadlineFn != nil {
			c.idleDeadlineFn(deadline.UnixNano())
		}
	}
	// Read until we get the close response or deadline.
	for {
		_, _, rerr := c.ReadMessageReuse()
		if rerr != nil {
			break
		}
	}
	if watchdog != nil {
		watchdog.Stop()
	}
	return c.Close()
}

// Close closes the underlying connection.
func (c *Conn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed.Load() {
		return nil
	}
	c.closed.Store(true)
	c.cancel()
	if c.engineReader != nil {
		c.engineReader.closeWith(io.EOF)
	}
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// NetConn returns the underlying net.Conn.
func (c *Conn) NetConn() net.Conn { return c.conn }

// Context returns the connection's context, cancelled when closed.
func (c *Conn) Context() context.Context { return c.ctx }

// Subprotocol returns the negotiated subprotocol, or "" if none.
func (c *Conn) Subprotocol() string { return c.subprotocol }

// RemoteAddr returns the peer's network address.
func (c *Conn) RemoteAddr() net.Addr {
	if c.conn == nil {
		return nil
	}
	return c.conn.RemoteAddr()
}

// LocalAddr returns the local network address, if known.
func (c *Conn) LocalAddr() net.Addr {
	if c.conn == nil {
		return nil
	}
	return c.conn.LocalAddr()
}

// BackpressureDropped returns the number of inbound chunks dropped because
// the engine-path read buffer overflowed despite TCP-level backpressure.
// Should be 0 in healthy operation. Returns 0 on the std (hijack) path,
// where backpressure is handled directly by the kernel TCP stack.
func (c *Conn) BackpressureDropped() uint64 {
	if c.engineReader == nil {
		return 0
	}
	return c.engineReader.Dropped()
}

// IP returns the remote IP address (without port).
func (c *Conn) IP() string {
	if c.conn == nil {
		return ""
	}
	addr := c.conn.RemoteAddr()
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

// Locals returns a per-connection value. Safe for concurrent use.
func (c *Conn) Locals(key string) any {
	c.localsMu.RLock()
	defer c.localsMu.RUnlock()
	if c.locals == nil {
		return nil
	}
	return c.locals[key]
}

// SetLocals stores a per-connection value. Safe for concurrent use.
func (c *Conn) SetLocals(key string, val any) {
	c.localsMu.Lock()
	defer c.localsMu.Unlock()
	if c.locals == nil {
		c.locals = make(map[string]any, 4)
	}
	c.locals[key] = val
}

// Param returns a URL route parameter captured at upgrade time.
func (c *Conn) Param(key string) string {
	for _, p := range c.params {
		if p[0] == key {
			return p[1]
		}
	}
	return ""
}

// Query returns a query parameter captured at upgrade time.
func (c *Conn) Query(key string) string {
	for _, q := range c.query {
		if q[0] == key {
			return q[1]
		}
	}
	return ""
}

// Header returns a request header captured at upgrade time.
func (c *Conn) Header(key string) string {
	for _, h := range c.headers {
		if h[0] == key {
			return h[1]
		}
	}
	return ""
}

// SetReadLimit sets the maximum message size in bytes. The default is 64MB.
func (c *Conn) SetReadLimit(limit int64) { c.readLimit = limit }

// SetReadDeadline sets the deadline for future reads.
// Returns nil on engine-integrated connections where deadlines are not supported.
func (c *Conn) SetReadDeadline(t time.Time) error {
	if c.conn == nil {
		return nil
	}
	return c.conn.SetReadDeadline(t)
}

// SetWriteDeadline sets the deadline for future writes.
// Returns nil on engine-integrated connections where deadlines are not supported.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	if c.conn == nil {
		return nil
	}
	return c.conn.SetWriteDeadline(t)
}

// SetPingHandler sets the handler for ping frames. The default handler
// replies with a pong containing the same payload.
func (c *Conn) SetPingHandler(h func(data []byte) error) { c.pingHandler = h }

// SetPongHandler sets the handler for pong frames. The default is a no-op.
func (c *Conn) SetPongHandler(h func(data []byte) error) { c.pongHandler = h }

// SetCloseHandler sets the handler for close frames. The default handler
// echoes the close frame back and returns a [CloseError].
func (c *Conn) SetCloseHandler(h func(code int, text string) error) { c.closeHandler = h }

// PingHandler returns the current ping handler.
func (c *Conn) PingHandler() func(data []byte) error { return c.pingHandler }

// PongHandler returns the current pong handler.
func (c *Conn) PongHandler() func(data []byte) error { return c.pongHandler }

// CloseHandler returns the current close handler.
func (c *Conn) CloseHandler() func(code int, text string) error { return c.closeHandler }

// EnableWriteCompression enables or disables write compression for this
// connection. Compression must have been negotiated during the upgrade
// handshake; this only controls whether subsequent writes actually compress.
func (c *Conn) EnableWriteCompression(enable bool) {
	c.compressDisabled = !enable
}

// SetCompressionLevel sets the flate compression level for subsequent writes.
// Valid range: -2 (HuffmanOnly) to 9 (BestCompression), or -1 (DefaultCompression).
func (c *Conn) SetCompressionLevel(level int) error {
	if level < minCompressionLevel || level > maxCompressionLevel {
		return errors.New("websocket: invalid compression level")
	}
	c.compressLevel = level
	return nil
}

// CloseError is returned when a close frame is received.
type CloseError struct {
	Code int
	Text string
}

func (e *CloseError) Error() string {
	s := "websocket: close " + strconv.Itoa(e.Code)
	if e.Text != "" {
		s += ": " + e.Text
	}
	return s
}

// IsCloseError returns true if err is a CloseError with one of the given codes.
func IsCloseError(err error, codes ...int) bool {
	var ce *CloseError
	if !errors.As(err, &ce) {
		return false
	}
	for _, c := range codes {
		if ce.Code == c {
			return true
		}
	}
	return false
}

// IsUnexpectedCloseError returns true if err is a CloseError whose code
// is NOT in the given list.
func IsUnexpectedCloseError(err error, expectedCodes ...int) bool {
	var ce *CloseError
	if !errors.As(err, &ce) {
		return false
	}
	for _, c := range expectedCodes {
		if ce.Code == c {
			return false
		}
	}
	return true
}

// FormatCloseMessage creates a close frame payload with the given code and text.
func FormatCloseMessage(code int, text string) []byte {
	buf := make([]byte, 2+len(text))
	binary.BigEndian.PutUint16(buf, uint16(code))
	copy(buf[2:], text)
	return buf
}
