// Package protocol implements the PostgreSQL v3 wire protocol framing and
// per-type encode/decode tables used by the celeris native PostgreSQL driver.
//
// Message framing follows the PostgreSQL documentation's "Message Formats"
// section: a one byte message type (frontend and backend share the same
// framing, though a handful of type bytes collide across directions) followed
// by a four byte big-endian length that *includes* the length field itself
// but *not* the type byte, followed by the payload.
//
// The startup, SSL request, and cancel request messages are exceptions: they
// have no type byte — just a four byte length and payload. Only the client
// sends these, and only as the very first message on a connection.
package protocol

import (
	"encoding/binary"
	"errors"
	"io"
)

// Frontend (client -> server) message type bytes.
const (
	MsgQuery           byte = 'Q'
	MsgParse           byte = 'P'
	MsgBind            byte = 'B'
	MsgDescribe        byte = 'D'
	MsgExecute         byte = 'E'
	MsgSync            byte = 'S'
	MsgClose           byte = 'C'
	MsgCopyData        byte = 'd'
	MsgCopyDone        byte = 'c'
	MsgCopyFail        byte = 'f'
	MsgTerminate       byte = 'X'
	MsgPasswordMessage byte = 'p' // also carries SASL responses
	MsgFlush           byte = 'H'
	MsgFunctionCall    byte = 'F'
)

// Backend (server -> client) message type bytes. Some of these collide with
// frontend type bytes (for instance both directions use 'D', 'C', 'd', 'c')
// so callers must always interpret the type byte in the context of the
// direction of the connection.
const (
	BackendAuthentication           byte = 'R'
	BackendBackendKeyData           byte = 'K'
	BackendParameterStatus          byte = 'S'
	BackendReadyForQuery            byte = 'Z'
	BackendRowDescription           byte = 'T'
	BackendDataRow                  byte = 'D'
	BackendCommandComplete          byte = 'C'
	BackendErrorResponse            byte = 'E'
	BackendNoticeResponse           byte = 'N'
	BackendParseComplete            byte = '1'
	BackendBindComplete             byte = '2'
	BackendCloseComplete            byte = '3'
	BackendNoData                   byte = 'n'
	BackendParameterDesc            byte = 't'
	BackendCopyData                 byte = 'd'
	BackendCopyDone                 byte = 'c'
	BackendCopyInResponse           byte = 'G'
	BackendCopyOutResponse          byte = 'H'
	BackendCopyBothResponse         byte = 'W'
	BackendNotification             byte = 'A'
	BackendEmptyQuery               byte = 'I'
	BackendPortalSuspended          byte = 's'
	BackendNegotiateProtocolVersion byte = 'v'
)

// headerLen is the number of length-field bytes (type byte is separate).
const headerLen = 4

// maxMessageSize is a soft cap to protect against wildly out-of-spec length
// prefixes read off an untrusted stream. The PostgreSQL protocol itself is
// bounded by int32 (since the length is a signed 32-bit value), but in
// practice well-formed messages are much smaller.
const maxMessageSize = 1 << 30 // 1 GiB

// ErrIncomplete is returned by Reader.Next when the internal buffer does not
// yet hold a full message. Callers should Feed more bytes and retry.
var ErrIncomplete = errors.New("postgres/protocol: incomplete message")

// ErrInvalidLength is returned when a message advertises a length smaller
// than the mandatory 4-byte length prefix or larger than maxMessageSize.
var ErrInvalidLength = errors.New("postgres/protocol: invalid message length")

// Reader decodes a stream of PostgreSQL protocol messages out of a byte
// buffer that the caller feeds incrementally. Reader is not safe for
// concurrent use.
type Reader struct {
	buf []byte
	pos int
}

// NewReader returns an empty Reader.
func NewReader() *Reader { return &Reader{} }

// Feed appends data to the internal buffer. The slice is not retained: the
// bytes are copied into the Reader's buffer.
func (r *Reader) Feed(data []byte) {
	r.buf = append(r.buf, data...)
}

// Next returns the type byte and payload of the next complete message in the
// buffer. The returned payload slice aliases the Reader's internal buffer
// and is only valid until the next call to Feed, Next, Compact, or Reset.
// When the buffer does not contain a full message, Next returns
// ErrIncomplete and the cursor is left unchanged.
func (r *Reader) Next() (byte, []byte, error) {
	if len(r.buf)-r.pos < 1+headerLen {
		return 0, nil, ErrIncomplete
	}
	msgType := r.buf[r.pos]
	length := int(binary.BigEndian.Uint32(r.buf[r.pos+1 : r.pos+5]))
	if length < headerLen {
		return 0, nil, ErrInvalidLength
	}
	if length > maxMessageSize {
		return 0, nil, ErrInvalidLength
	}
	total := 1 + length // type byte + declared length (length includes itself)
	if len(r.buf)-r.pos < total {
		return 0, nil, ErrIncomplete
	}
	payload := r.buf[r.pos+1+headerLen : r.pos+total]
	r.pos += total
	return msgType, payload, nil
}

// Compact discards bytes that have already been consumed. Call periodically
// (typically after processing a batch of messages) to reclaim space.
func (r *Reader) Compact() {
	if r.pos == 0 {
		return
	}
	n := copy(r.buf, r.buf[r.pos:])
	r.buf = r.buf[:n]
	r.pos = 0
}

// Reset clears the buffer. The underlying array is kept so that subsequent
// Feed calls can reuse its capacity.
func (r *Reader) Reset() {
	r.buf = r.buf[:0]
	r.pos = 0
}

// Buffered returns the number of bytes currently in the buffer that have not
// yet been consumed by Next.
func (r *Reader) Buffered() int { return len(r.buf) - r.pos }

// Writer encodes PostgreSQL protocol messages into an internal buffer. The
// buffer is reused across messages, so callers that need to retain message
// bytes across Reset calls must copy them out. Writer is not safe for
// concurrent use.
type Writer struct {
	buf        []byte
	msgStart   int // index of the type byte (or length prefix for startup msgs)
	inMessage  bool
	startupMsg bool
}

// NewWriter returns an empty Writer.
func NewWriter() *Writer { return &Writer{} }

// StartMessage begins a new frontend message with the given type byte. The
// caller must follow with writes for the message body and then call
// FinishMessage.
func (w *Writer) StartMessage(msgType byte) {
	if w.inMessage {
		panic("postgres/protocol: StartMessage called while another message is in progress")
	}
	w.msgStart = len(w.buf)
	w.inMessage = true
	w.startupMsg = false
	// type byte + placeholder 4-byte length
	w.buf = append(w.buf, msgType, 0, 0, 0, 0)
}

// StartStartupMessage begins a startup/cancel/SSL message. These messages
// have no type byte — just a 4-byte length prefix.
func (w *Writer) StartStartupMessage() {
	if w.inMessage {
		panic("postgres/protocol: StartStartupMessage called while another message is in progress")
	}
	w.msgStart = len(w.buf)
	w.inMessage = true
	w.startupMsg = true
	// placeholder 4-byte length
	w.buf = append(w.buf, 0, 0, 0, 0)
}

// FinishMessage completes the current message by patching the length field.
// The length includes the length prefix itself but never the frontend type
// byte.
func (w *Writer) FinishMessage() {
	if !w.inMessage {
		panic("postgres/protocol: FinishMessage called with no message in progress")
	}
	var lenOffset int
	if w.startupMsg {
		lenOffset = w.msgStart
	} else {
		lenOffset = w.msgStart + 1
	}
	length := uint32(len(w.buf) - lenOffset)
	binary.BigEndian.PutUint32(w.buf[lenOffset:lenOffset+4], length)
	w.inMessage = false
}

// Bytes returns the accumulated buffer. The returned slice aliases the
// Writer's internal storage until Reset is called.
func (w *Writer) Bytes() []byte { return w.buf }

// Reset clears the buffer for reuse while keeping the underlying array.
func (w *Writer) Reset() {
	w.buf = w.buf[:0]
	w.msgStart = 0
	w.inMessage = false
	w.startupMsg = false
}

// Len returns the number of bytes currently in the buffer.
func (w *Writer) Len() int { return len(w.buf) }

// WriteByte appends a single byte to the current message body. It returns
// nil unconditionally to satisfy io.ByteWriter.
func (w *Writer) WriteByte(b byte) error {
	w.buf = append(w.buf, b)
	return nil
}

// WriteInt16 appends a big-endian int16.
func (w *Writer) WriteInt16(v int16) {
	w.buf = append(w.buf, byte(uint16(v)>>8), byte(v))
}

// WriteInt32 appends a big-endian int32.
func (w *Writer) WriteInt32(v int32) {
	u := uint32(v)
	w.buf = append(w.buf, byte(u>>24), byte(u>>16), byte(u>>8), byte(u))
}

// WriteString appends s followed by a null terminator (PostgreSQL CString).
func (w *Writer) WriteString(s string) {
	w.buf = append(w.buf, s...)
	w.buf = append(w.buf, 0)
}

// WriteBytes appends b verbatim without any length prefix.
func (w *Writer) WriteBytes(b []byte) {
	w.buf = append(w.buf, b...)
}

// ReadByte reads a single byte from payload at *pos and advances *pos.
func ReadByte(payload []byte, pos *int) (byte, error) {
	if *pos+1 > len(payload) {
		return 0, io.ErrShortBuffer
	}
	b := payload[*pos]
	*pos++
	return b, nil
}

// ReadInt16 reads a big-endian int16 and advances *pos by 2.
func ReadInt16(payload []byte, pos *int) (int16, error) {
	if *pos+2 > len(payload) {
		return 0, io.ErrShortBuffer
	}
	v := int16(binary.BigEndian.Uint16(payload[*pos:]))
	*pos += 2
	return v, nil
}

// ReadInt32 reads a big-endian int32 and advances *pos by 4.
func ReadInt32(payload []byte, pos *int) (int32, error) {
	if *pos+4 > len(payload) {
		return 0, io.ErrShortBuffer
	}
	v := int32(binary.BigEndian.Uint32(payload[*pos:]))
	*pos += 4
	return v, nil
}

// ReadCString reads a null-terminated string. The returned string is a copy
// of the bytes — it does not alias payload. Returns io.ErrShortBuffer if no
// null terminator is found before the end of payload.
func ReadCString(payload []byte, pos *int) (string, error) {
	for i := *pos; i < len(payload); i++ {
		if payload[i] == 0 {
			s := string(payload[*pos:i])
			*pos = i + 1
			return s, nil
		}
	}
	return "", io.ErrShortBuffer
}

// ReadBytes reads exactly n bytes and advances *pos. The returned slice
// ALIASES payload — callers that need to retain the bytes beyond the next
// Reader call must copy.
func ReadBytes(payload []byte, pos *int, n int) ([]byte, error) {
	if n < 0 {
		return nil, io.ErrShortBuffer
	}
	if *pos+n > len(payload) {
		return nil, io.ErrShortBuffer
	}
	b := payload[*pos : *pos+n]
	*pos += n
	return b, nil
}
