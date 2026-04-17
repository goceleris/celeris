// Package protocol implements the text and binary wire protocols used by the
// celeris native Memcached driver.
//
// # Text protocol
//
// The text protocol is line-oriented ASCII. Storage commands carry a data
// block on the following line; retrieval commands return zero or more VALUE
// lines (each with an optional CAS), then the END sentinel. Responses are
// streamed via [TextReader]; the reader parses one logical reply at a time
// and returns [ErrIncomplete] when more bytes are needed.
//
// # Binary protocol
//
// See [binary.go]. Length-prefixed 24-byte header + optional extras/key/value
// body. Drivers choose text or binary at dial time.
//
// # Allocation discipline
//
// Both [TextWriter] and [BinaryWriter] expose a caller-supplied byte buffer
// via Reset — no per-call heap allocation in the steady state. The reader
// side feeds into an append-only internal buffer; VALUE payloads are sliced
// into the buffer and valid until the next Feed/Compact cycle.
package protocol

import (
	"bytes"
	"errors"
	"strconv"
)

// ErrIncomplete indicates the buffered input does not yet contain a complete
// reply. The reader's cursor is not advanced and the caller should feed more
// bytes and retry.
var ErrIncomplete = errors.New("celeris/memcached/protocol: incomplete frame")

// ErrProtocol is returned when server input cannot be parsed against either
// dialect (corrupted stream, unknown reply line, bad length header, ...).
var ErrProtocol = errors.New("celeris/memcached/protocol: protocol error")

// MaxValueLen caps the advertised length of a single VALUE data block. 128 MiB
// is above memcached's default 1 MiB per-item limit with plenty of headroom
// for servers raised via -I.
const MaxValueLen = 128 * 1024 * 1024

// Kind tags the flavor of a parsed text-protocol reply.
type Kind uint8

const (
	// KindStored corresponds to "STORED\r\n".
	KindStored Kind = iota
	// KindNotStored corresponds to "NOT_STORED\r\n".
	KindNotStored
	// KindExists corresponds to "EXISTS\r\n" (CAS mismatch).
	KindExists
	// KindNotFound corresponds to "NOT_FOUND\r\n".
	KindNotFound
	// KindDeleted corresponds to "DELETED\r\n".
	KindDeleted
	// KindTouched corresponds to "TOUCHED\r\n".
	KindTouched
	// KindOK corresponds to "OK\r\n".
	KindOK
	// KindEnd corresponds to "END\r\n" and terminates a get/gets/stats reply.
	KindEnd
	// KindValue is a "VALUE <key> <flags> <bytes> [<cas>]\r\n<data>\r\n" block.
	KindValue
	// KindStat is a "STAT <name> <value>\r\n" line within a stats reply.
	KindStat
	// KindNumber is a decimal integer reply from incr/decr.
	KindNumber
	// KindVersion is a "VERSION <version>\r\n" reply.
	KindVersion
	// KindError is a generic "ERROR\r\n" reply (unknown command).
	KindError
	// KindClientError is a "CLIENT_ERROR <msg>\r\n" reply (malformed input).
	KindClientError
	// KindServerError is a "SERVER_ERROR <msg>\r\n" reply (server-side fault).
	KindServerError
)

// String returns a short name for diagnostics.
func (k Kind) String() string {
	switch k {
	case KindStored:
		return "STORED"
	case KindNotStored:
		return "NOT_STORED"
	case KindExists:
		return "EXISTS"
	case KindNotFound:
		return "NOT_FOUND"
	case KindDeleted:
		return "DELETED"
	case KindTouched:
		return "TOUCHED"
	case KindOK:
		return "OK"
	case KindEnd:
		return "END"
	case KindValue:
		return "VALUE"
	case KindStat:
		return "STAT"
	case KindNumber:
		return "NUMBER"
	case KindVersion:
		return "VERSION"
	case KindError:
		return "ERROR"
	case KindClientError:
		return "CLIENT_ERROR"
	case KindServerError:
		return "SERVER_ERROR"
	}
	return "unknown"
}

// TextReply is one decoded text-protocol reply line (or VALUE block).
//
// Depending on Kind, different fields are populated:
//
//   - KindValue:    Key, Flags, CAS, Data.
//   - KindStat:     Key (name), Data (value).
//   - KindNumber:   Int.
//   - KindVersion:  Data (version string).
//   - KindError / KindClientError / KindServerError: Data (message, may be empty).
//   - Scalars (Stored, Deleted, Touched, OK, End, ...): no extra fields.
type TextReply struct {
	Kind  Kind
	Key   []byte
	Flags uint32
	// CAS is the compare-and-swap token for VALUE blocks returned by gets/gats
	// replies. Zero when the server did not advertise one.
	CAS  uint64
	Int  uint64
	Data []byte
}

// TextReader streams text-protocol replies. The zero value is not usable;
// call [NewTextReader].
type TextReader struct {
	buf []byte
	r   int
	w   int
}

// NewTextReader returns a ready-to-use TextReader.
func NewTextReader() *TextReader {
	return &TextReader{}
}

// Feed appends data to the internal buffer.
func (r *TextReader) Feed(data []byte) {
	if len(data) == 0 {
		return
	}
	r.buf = append(r.buf, data...)
	r.w = len(r.buf)
}

// Reset clears internal state while retaining the buffer for reuse.
func (r *TextReader) Reset() {
	r.buf = r.buf[:0]
	r.r = 0
	r.w = 0
}

// Compact discards already-parsed bytes. Aliased slices from previously
// returned replies become invalid after Compact.
func (r *TextReader) Compact() {
	if r.r == 0 {
		return
	}
	if r.r >= r.w {
		r.buf = r.buf[:0]
		r.r = 0
		r.w = 0
		return
	}
	n := copy(r.buf, r.buf[r.r:r.w])
	r.buf = r.buf[:n]
	r.r = 0
	r.w = n
}

// Next parses one complete text-protocol reply. Returns [ErrIncomplete] if
// more bytes are needed; the read cursor is restored in that case.
func (r *TextReader) Next() (TextReply, error) {
	start := r.r
	reply, err := r.parse()
	if err != nil {
		if errors.Is(err, ErrIncomplete) {
			r.r = start
		}
		return TextReply{}, err
	}
	return reply, nil
}

func (r *TextReader) parse() (TextReply, error) {
	line, ok := r.readLine()
	if !ok {
		return TextReply{}, ErrIncomplete
	}
	if len(line) == 0 {
		return TextReply{}, ErrProtocol
	}

	// VALUE <key> <flags> <bytes> [<cas>]\r\n<data>\r\n
	if bytes.HasPrefix(line, []byte("VALUE ")) {
		return r.parseValue(line[len("VALUE "):])
	}
	// STAT <name> <value>\r\n
	if bytes.HasPrefix(line, []byte("STAT ")) {
		rest := line[len("STAT "):]
		sp := bytes.IndexByte(rest, ' ')
		if sp < 0 {
			return TextReply{Kind: KindStat, Key: rest}, nil
		}
		return TextReply{Kind: KindStat, Key: rest[:sp], Data: rest[sp+1:]}, nil
	}
	// VERSION <version>\r\n
	if bytes.HasPrefix(line, []byte("VERSION ")) {
		return TextReply{Kind: KindVersion, Data: line[len("VERSION "):]}, nil
	}
	// SERVER_ERROR / CLIENT_ERROR carry a message; ERROR does not.
	if bytes.HasPrefix(line, []byte("SERVER_ERROR")) {
		return TextReply{Kind: KindServerError, Data: trimSpacePrefix(line[len("SERVER_ERROR"):])}, nil
	}
	if bytes.HasPrefix(line, []byte("CLIENT_ERROR")) {
		return TextReply{Kind: KindClientError, Data: trimSpacePrefix(line[len("CLIENT_ERROR"):])}, nil
	}

	// Single-token replies.
	switch string(line) {
	case "STORED":
		return TextReply{Kind: KindStored}, nil
	case "NOT_STORED":
		return TextReply{Kind: KindNotStored}, nil
	case "EXISTS":
		return TextReply{Kind: KindExists}, nil
	case "NOT_FOUND":
		return TextReply{Kind: KindNotFound}, nil
	case "DELETED":
		return TextReply{Kind: KindDeleted}, nil
	case "TOUCHED":
		return TextReply{Kind: KindTouched}, nil
	case "OK":
		return TextReply{Kind: KindOK}, nil
	case "END":
		return TextReply{Kind: KindEnd}, nil
	case "ERROR":
		return TextReply{Kind: KindError}, nil
	}

	// Incr/decr reply: bare decimal number.
	if n, err := parseUint(line); err == nil {
		return TextReply{Kind: KindNumber, Int: n}, nil
	}

	return TextReply{}, ErrProtocol
}

// parseValue decodes "VALUE <key> <flags> <bytes> [<cas>]" plus the data
// block + trailing CRLF. On short input returns ErrIncomplete and the caller
// restores r.r.
func (r *TextReader) parseValue(rest []byte) (TextReply, error) {
	// Parse header fields.
	key, rest, ok := splitOnce(rest, ' ')
	if !ok {
		return TextReply{}, ErrProtocol
	}
	flagsTok, rest, ok := splitOnce(rest, ' ')
	if !ok {
		return TextReply{}, ErrProtocol
	}
	flags, err := parseUint(flagsTok)
	if err != nil || flags > 0xFFFFFFFF {
		return TextReply{}, ErrProtocol
	}
	var bytesTok, casTok []byte
	if sp := bytes.IndexByte(rest, ' '); sp >= 0 {
		bytesTok, casTok = rest[:sp], rest[sp+1:]
	} else {
		bytesTok = rest
	}
	n, err := parseUint(bytesTok)
	if err != nil {
		return TextReply{}, ErrProtocol
	}
	if n > MaxValueLen {
		return TextReply{}, ErrProtocol
	}
	var cas uint64
	if len(casTok) > 0 {
		cas, err = parseUint(casTok)
		if err != nil {
			return TextReply{}, ErrProtocol
		}
	}

	// Need n bytes + CRLF.
	if r.w-r.r < int(n)+2 {
		return TextReply{}, ErrIncomplete
	}
	end := r.r + int(n)
	if r.buf[end] != '\r' || r.buf[end+1] != '\n' {
		return TextReply{}, ErrProtocol
	}
	data := r.buf[r.r:end]
	r.r = end + 2
	return TextReply{
		Kind:  KindValue,
		Key:   key,
		Flags: uint32(flags),
		CAS:   cas,
		Data:  data,
	}, nil
}

// readLine returns the bytes up to (but not including) the next CRLF and
// advances past the CRLF. Returns (nil, false) on short input and leaves
// r.r unchanged.
func (r *TextReader) readLine() ([]byte, bool) {
	start := r.r
	for i := start; i+1 < r.w; i++ {
		if r.buf[i] == '\r' && r.buf[i+1] == '\n' {
			s := r.buf[start:i]
			r.r = i + 2
			return s, true
		}
	}
	return nil, false
}

// splitOnce splits s on the first occurrence of sep. Returns (head, tail,
// true) on a successful split; if sep is not found returns (s, nil, false).
func splitOnce(s []byte, sep byte) (head, tail []byte, ok bool) {
	i := bytes.IndexByte(s, sep)
	if i < 0 {
		return s, nil, false
	}
	return s[:i], s[i+1:], true
}

// trimSpacePrefix drops a single leading ASCII space if present.
func trimSpacePrefix(s []byte) []byte {
	if len(s) > 0 && s[0] == ' ' {
		return s[1:]
	}
	return s
}

// parseUint parses an unsigned ASCII decimal integer. Empty / non-digit input
// yields ErrProtocol.
func parseUint(b []byte) (uint64, error) {
	if len(b) == 0 {
		return 0, ErrProtocol
	}
	var n uint64
	for _, d := range b {
		if d < '0' || d > '9' {
			return 0, ErrProtocol
		}
		n10 := n * 10
		if n10/10 != n {
			return 0, ErrProtocol
		}
		n = n10 + uint64(d-'0')
		if n < uint64(d-'0') {
			return 0, ErrProtocol
		}
	}
	return n, nil
}

// TextWriter builds text-protocol requests into a reusable byte buffer. The
// internal buffer is reset on every [TextWriter.Reset] call; callers that
// batch multiple commands should call Reset at the start of a batch and then
// use the Append* methods.
type TextWriter struct {
	buf []byte
}

// NewTextWriter returns a TextWriter with a small pre-allocated buffer.
func NewTextWriter() *TextWriter {
	return &TextWriter{buf: make([]byte, 0, 128)}
}

// Reset empties the internal buffer without freeing it.
func (w *TextWriter) Reset() { w.buf = w.buf[:0] }

// Bytes returns the serialized buffer.
func (w *TextWriter) Bytes() []byte { return w.buf }

// AppendStorage encodes "<cmd> <key> <flags> <exptime> <bytes>[ <cas>][ noreply]\r\n<data>\r\n".
// cmd is one of: set, add, replace, append, prepend, cas.
// For cas commands, casID must be non-zero.
func (w *TextWriter) AppendStorage(cmd, key string, flags uint32, exptime int64, data []byte, casID uint64, noreply bool) []byte {
	w.buf = append(w.buf, cmd...)
	w.buf = append(w.buf, ' ')
	w.buf = append(w.buf, key...)
	w.buf = append(w.buf, ' ')
	w.buf = strconv.AppendUint(w.buf, uint64(flags), 10)
	w.buf = append(w.buf, ' ')
	w.buf = strconv.AppendInt(w.buf, exptime, 10)
	w.buf = append(w.buf, ' ')
	w.buf = strconv.AppendInt(w.buf, int64(len(data)), 10)
	if casID != 0 {
		w.buf = append(w.buf, ' ')
		w.buf = strconv.AppendUint(w.buf, casID, 10)
	}
	if noreply {
		w.buf = append(w.buf, ' ', 'n', 'o', 'r', 'e', 'p', 'l', 'y')
	}
	w.buf = append(w.buf, '\r', '\n')
	w.buf = append(w.buf, data...)
	w.buf = append(w.buf, '\r', '\n')
	return w.buf
}

// AppendRetrieval encodes "<cmd> <key1> <key2> ...\r\n". cmd is one of
// get / gets. gat/gats carry a leading exptime — see [AppendRetrievalTouch].
func (w *TextWriter) AppendRetrieval(cmd string, keys ...string) []byte {
	w.buf = append(w.buf, cmd...)
	for _, k := range keys {
		w.buf = append(w.buf, ' ')
		w.buf = append(w.buf, k...)
	}
	w.buf = append(w.buf, '\r', '\n')
	return w.buf
}

// AppendRetrievalTouch encodes "<cmd> <exptime> <key1> <key2> ...\r\n". cmd
// is one of gat / gats.
func (w *TextWriter) AppendRetrievalTouch(cmd string, exptime int64, keys ...string) []byte {
	w.buf = append(w.buf, cmd...)
	w.buf = append(w.buf, ' ')
	w.buf = strconv.AppendInt(w.buf, exptime, 10)
	for _, k := range keys {
		w.buf = append(w.buf, ' ')
		w.buf = append(w.buf, k...)
	}
	w.buf = append(w.buf, '\r', '\n')
	return w.buf
}

// AppendArith encodes "incr <key> <delta>\r\n" or "decr <key> <delta>\r\n".
func (w *TextWriter) AppendArith(cmd, key string, delta uint64) []byte {
	w.buf = append(w.buf, cmd...)
	w.buf = append(w.buf, ' ')
	w.buf = append(w.buf, key...)
	w.buf = append(w.buf, ' ')
	w.buf = strconv.AppendUint(w.buf, delta, 10)
	w.buf = append(w.buf, '\r', '\n')
	return w.buf
}

// AppendDelete encodes "delete <key>\r\n".
func (w *TextWriter) AppendDelete(key string) []byte {
	w.buf = append(w.buf, 'd', 'e', 'l', 'e', 't', 'e', ' ')
	w.buf = append(w.buf, key...)
	w.buf = append(w.buf, '\r', '\n')
	return w.buf
}

// AppendTouch encodes "touch <key> <exptime>\r\n".
func (w *TextWriter) AppendTouch(key string, exptime int64) []byte {
	w.buf = append(w.buf, 't', 'o', 'u', 'c', 'h', ' ')
	w.buf = append(w.buf, key...)
	w.buf = append(w.buf, ' ')
	w.buf = strconv.AppendInt(w.buf, exptime, 10)
	w.buf = append(w.buf, '\r', '\n')
	return w.buf
}

// AppendFlushAll encodes "flush_all[ <delay>]\r\n". Pass a negative delay to
// omit the argument.
func (w *TextWriter) AppendFlushAll(delay int64) []byte {
	w.buf = append(w.buf, 'f', 'l', 'u', 's', 'h', '_', 'a', 'l', 'l')
	if delay >= 0 {
		w.buf = append(w.buf, ' ')
		w.buf = strconv.AppendInt(w.buf, delay, 10)
	}
	w.buf = append(w.buf, '\r', '\n')
	return w.buf
}

// AppendSimple encodes a single-word command like "version\r\n", "stats\r\n",
// or "quit\r\n".
func (w *TextWriter) AppendSimple(cmd string) []byte {
	w.buf = append(w.buf, cmd...)
	w.buf = append(w.buf, '\r', '\n')
	return w.buf
}

// AppendStats encodes "stats[ <arg>]\r\n".
func (w *TextWriter) AppendStats(arg string) []byte {
	w.buf = append(w.buf, 's', 't', 'a', 't', 's')
	if arg != "" {
		w.buf = append(w.buf, ' ')
		w.buf = append(w.buf, arg...)
	}
	w.buf = append(w.buf, '\r', '\n')
	return w.buf
}
