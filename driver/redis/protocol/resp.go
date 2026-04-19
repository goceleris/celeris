// Package protocol implements the RESP2 and RESP3 wire protocol framing used
// by the celeris native Redis driver.
//
// The parser is allocation-frugal: simple strings, errors, and bulk strings
// are returned as slices that alias the Reader's internal input buffer.
// Callers that need to retain bytes past the next [Reader.Feed] or
// [Reader.Next] call must copy them. The aggregate types (Array, Set, Push,
// Map, Attr) draw their backing slices from small sync.Pools — call
// [Reader.Release] on a top-level Value to return those slices to the pool.
//
// RESP2 types: +, -, :, $, *.
// RESP3 types add: _ (null), # (bool), , (double), ( (bignum), ! (blob
// error), = (verbatim), ~ (set), % (map), | (attribute), > (push).
//
// # Incremental parsing
//
// [Reader.Next] returns [ErrIncomplete] when the buffered input does not yet
// hold a full frame. The read cursor is not advanced in this case, so the
// caller should feed more bytes and retry.
//
// # Large bulk strings
//
// Bulk strings longer than [maxInlineBulk] are returned in a freshly
// allocated heap buffer rather than kept in the Reader's own buffer. This
// prevents pathological growth of the Reader buffer on occasional large
// values.
package protocol

import (
	"errors"
	"math"
	"strconv"
	"sync"
)

// ErrIncomplete indicates the buffered input does not yet contain a full RESP
// frame. The Reader's cursor is not advanced.
var ErrIncomplete = errors.New("celeris-redis-protocol: incomplete frame")

// ErrProtocol indicates malformed RESP input.
var ErrProtocol = errors.New("celeris-redis-protocol: protocol error")

// ErrProtocolOversizedBulk indicates a bulk string length header exceeds
// [MaxBulkLen]. Hostile or corrupt servers can otherwise force the Reader
// buffer to grow without bound while waiting for payload bytes.
var ErrProtocolOversizedBulk = errors.New("celeris-redis-protocol: bulk length exceeds maximum")

// ErrProtocolOversizedArray indicates an aggregate header (array / set / push
// / map / attr) exceeds [MaxArrayLen].
var ErrProtocolOversizedArray = errors.New("celeris-redis-protocol: aggregate length exceeds maximum")

const (
	// MaxBulkLen caps the advertised length of a single bulk/verbatim/blob
	// error frame. 512 MiB matches Redis's own proto-max-bulk-len ceiling.
	MaxBulkLen = 512 * 1024 * 1024
	// MaxArrayLen caps the advertised element count of an aggregate
	// (array/set/push/map/attr). Chosen generously — 128M elements would
	// already represent gigabytes of pointers.
	MaxArrayLen = 128 * 1024 * 1024
)

// Type tags a RESP value.
type Type uint8

const (
	// TySimple is a RESP2/RESP3 simple string prefixed by '+'.
	TySimple Type = iota
	// TyError is a RESP2/RESP3 simple error prefixed by '-'.
	TyError
	// TyInt is a RESP2/RESP3 integer prefixed by ':'.
	TyInt
	// TyBulk is a RESP2/RESP3 bulk string prefixed by '$'.
	TyBulk
	// TyArray is a RESP2/RESP3 array prefixed by '*'.
	TyArray
	// TyNull is a RESP3 null ('_') or a RESP2 null-bulk/null-array ($-1/*-1).
	TyNull
	// TyBool is a RESP3 boolean prefixed by '#'.
	TyBool
	// TyDouble is a RESP3 double prefixed by ','.
	TyDouble
	// TyBigInt is a RESP3 big number prefixed by '('.
	TyBigInt
	// TyBlobErr is a RESP3 blob error prefixed by '!'.
	TyBlobErr
	// TyVerbatim is a RESP3 verbatim string prefixed by '='.
	TyVerbatim
	// TySet is a RESP3 set prefixed by '~'.
	TySet
	// TyMap is a RESP3 map prefixed by '%'.
	TyMap
	// TyAttr is a RESP3 attribute map prefixed by '|'.
	TyAttr
	// TyPush is a RESP3 push frame prefixed by '>'.
	TyPush
)

// String returns a short name for diagnostics.
func (t Type) String() string {
	switch t {
	case TySimple:
		return "simple"
	case TyError:
		return "error"
	case TyInt:
		return "int"
	case TyBulk:
		return "bulk"
	case TyArray:
		return "array"
	case TyNull:
		return "null"
	case TyBool:
		return "bool"
	case TyDouble:
		return "double"
	case TyBigInt:
		return "bigint"
	case TyBlobErr:
		return "bloberr"
	case TyVerbatim:
		return "verbatim"
	case TySet:
		return "set"
	case TyMap:
		return "map"
	case TyAttr:
		return "attr"
	case TyPush:
		return "push"
	}
	return "unknown"
}

// KV is a key-value pair inside a Map or Attribute value.
type KV struct {
	K, V Value
}

// Value is one decoded RESP value. Depending on Type, different fields are
// populated.
//
//   - Simple / Error / Bulk / Verbatim / BlobErr → Str (may alias the
//     Reader's buffer; copy before next Feed/Next).
//   - Int → Int.
//   - Double → Float.
//   - Bool → Bool.
//   - BigInt → BigN (raw ASCII, alias of buffer).
//   - Array / Set / Push → Array.
//   - Map / Attr → Map.
//   - Null → no field set.
type Value struct {
	Type  Type
	Str   []byte
	Int   int64
	Float float64
	Bool  bool
	BigN  []byte
	Array []Value
	Map   []KV

	// pooled flags: when true the Array/Map slice came from a sync.Pool and
	// Release should return it.
	pooledArr bool
	pooledMap bool
}

const (
	// maxInlineBulk is the largest bulk string kept inline in the Reader
	// buffer. Longer bulks are copied to a fresh heap slice.
	maxInlineBulk = 1 << 20 // 1 MiB
)

// Reader is a streaming RESP decoder. It owns an append-only buffer that
// callers extend via [Reader.Feed]. The zero value is ready for use.
type Reader struct {
	buf []byte
	r   int // parsed cursor
	w   int // append cursor (== len(buf))

	arrPool sync.Pool // []Value
	mapPool sync.Pool // []KV
}

// NewReader returns a ready-to-use Reader.
func NewReader() *Reader {
	r := &Reader{}
	r.arrPool.New = func() any { s := make([]Value, 0, 8); return &s }
	r.mapPool.New = func() any { s := make([]KV, 0, 8); return &s }
	return r
}

// Feed appends data to the internal buffer.
func (r *Reader) Feed(data []byte) {
	if len(data) == 0 {
		return
	}
	r.buf = append(r.buf, data...)
	r.w = len(r.buf)
}

// Reset clears internal state while keeping buffers for reuse.
func (r *Reader) Reset() {
	r.buf = r.buf[:0]
	r.r = 0
	r.w = 0
}

// Compact discards already-parsed bytes. Aliased slices from previously
// returned Values become invalid after Compact.
func (r *Reader) Compact() {
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

// Release returns any pooled slices attached to v back to the Reader's
// internal pools. Safe to call on values whose aggregate slices were not
// pool-sourced (fields are simply cleared).
func (r *Reader) Release(v Value) {
	r.release(&v)
}

// ClearPooledFlags marks v (and its nested Array/Map entries) as non-pooled.
// Callers that deep-copy a Value into heap-allocated slices must invoke this
// so a later [Reader.Release] cannot return heap slices into the Reader's
// sync.Pools (which would corrupt later parses).
func ClearPooledFlags(v *Value) {
	v.pooledArr = false
	v.pooledMap = false
	for i := range v.Array {
		ClearPooledFlags(&v.Array[i])
	}
	for i := range v.Map {
		ClearPooledFlags(&v.Map[i].K)
		ClearPooledFlags(&v.Map[i].V)
	}
}

func (r *Reader) release(v *Value) {
	if v.pooledArr && v.Array != nil {
		for i := range v.Array {
			r.release(&v.Array[i])
		}
		s := v.Array[:0]
		v.Array = nil
		r.arrPool.Put(&s)
	} else if len(v.Array) > 0 {
		for i := range v.Array {
			r.release(&v.Array[i])
		}
	}
	if v.pooledMap && v.Map != nil {
		for i := range v.Map {
			r.release(&v.Map[i].K)
			r.release(&v.Map[i].V)
		}
		s := v.Map[:0]
		v.Map = nil
		r.mapPool.Put(&s)
	} else if len(v.Map) > 0 {
		for i := range v.Map {
			r.release(&v.Map[i].K)
			r.release(&v.Map[i].V)
		}
	}
}

// Next parses one complete RESP value. On [ErrIncomplete], the read cursor is
// left unchanged; the caller should Feed more bytes and retry.
func (r *Reader) Next() (Value, error) {
	start := r.r
	v, err := r.parse()
	if err != nil {
		if errors.Is(err, ErrIncomplete) {
			r.r = start
		}
		return Value{}, err
	}
	return v, nil
}

// parse consumes one value; advances r.r on success.
func (r *Reader) parse() (Value, error) {
	if r.r >= r.w {
		return Value{}, ErrIncomplete
	}
	tag := r.buf[r.r]
	r.r++
	switch tag {
	case '+':
		s, err := r.readLine()
		if err != nil {
			return Value{}, err
		}
		return Value{Type: TySimple, Str: s}, nil
	case '-':
		s, err := r.readLine()
		if err != nil {
			return Value{}, err
		}
		return Value{Type: TyError, Str: s}, nil
	case ':':
		n, err := r.readInt()
		if err != nil {
			return Value{}, err
		}
		return Value{Type: TyInt, Int: n}, nil
	case '$':
		return r.readBulk(TyBulk)
	case '=':
		return r.readBulk(TyVerbatim)
	case '!':
		return r.readBulk(TyBlobErr)
	case '*':
		return r.readArray(TyArray)
	case '~':
		return r.readArray(TySet)
	case '>':
		return r.readArray(TyPush)
	case '%':
		return r.readMap(TyMap)
	case '|':
		return r.readMap(TyAttr)
	case '_':
		// RESP3 null: _<CRLF>
		if _, err := r.readLine(); err != nil {
			return Value{}, err
		}
		return Value{Type: TyNull}, nil
	case '#':
		s, err := r.readLine()
		if err != nil {
			return Value{}, err
		}
		if len(s) != 1 || (s[0] != 't' && s[0] != 'f') {
			return Value{}, ErrProtocol
		}
		return Value{Type: TyBool, Bool: s[0] == 't'}, nil
	case ',':
		s, err := r.readLine()
		if err != nil {
			return Value{}, err
		}
		f, perr := parseFloat(s)
		if perr != nil {
			return Value{}, ErrProtocol
		}
		return Value{Type: TyDouble, Float: f}, nil
	case '(':
		s, err := r.readLine()
		if err != nil {
			return Value{}, err
		}
		return Value{Type: TyBigInt, BigN: s}, nil
	default:
		return Value{}, ErrProtocol
	}
}

// readLine returns the bytes up to (but not including) the next CRLF and
// advances past the CRLF. On short input returns ErrIncomplete and leaves
// r.r unchanged.
func (r *Reader) readLine() ([]byte, error) {
	start := r.r
	for i := start; i+1 < r.w; i++ {
		if r.buf[i] == '\r' && r.buf[i+1] == '\n' {
			s := r.buf[start:i]
			r.r = i + 2
			return s, nil
		}
	}
	r.r = start
	return nil, ErrIncomplete
}

// readInt parses <int><CRLF>.
func (r *Reader) readInt() (int64, error) {
	s, err := r.readLine()
	if err != nil {
		return 0, err
	}
	return parseInt(s)
}

func (r *Reader) readBulk(ty Type) (Value, error) {
	n, err := r.readInt()
	if err != nil {
		return Value{}, err
	}
	if n == -1 {
		return Value{Type: TyNull}, nil
	}
	if n < 0 {
		return Value{}, ErrProtocol
	}
	if n > MaxBulkLen {
		return Value{}, ErrProtocolOversizedBulk
	}
	// Need n bytes + CRLF. On ErrIncomplete the caller (Next)
	// rewinds r.r to before the tag byte; no local rewind needed.
	if r.w-r.r < int(n)+2 {
		return Value{}, ErrIncomplete
	}
	end := r.r + int(n)
	if r.buf[end] != '\r' || r.buf[end+1] != '\n' {
		return Value{}, ErrProtocol
	}
	var data []byte
	if n > maxInlineBulk {
		data = make([]byte, n)
		copy(data, r.buf[r.r:end])
	} else {
		data = r.buf[r.r:end]
	}
	r.r = end + 2
	return Value{Type: ty, Str: data}, nil
}

func (r *Reader) readArray(ty Type) (Value, error) {
	n, err := r.readInt()
	if err != nil {
		return Value{}, err
	}
	if n == -1 {
		return Value{Type: TyNull}, nil
	}
	if n < 0 {
		return Value{}, ErrProtocol
	}
	if n > MaxArrayLen {
		return Value{}, ErrProtocolOversizedArray
	}
	arrPtr := r.arrPool.Get().(*[]Value)
	arr := (*arrPtr)[:0]
	if cap(arr) < int(n) {
		arr = make([]Value, 0, n)
	}
	for i := int64(0); i < n; i++ {
		v, err := r.parse()
		if err != nil {
			// caller's Next will rewind on ErrIncomplete — but we've
			// already consumed parts. We must release the pool item.
			s := arr[:0]
			r.arrPool.Put(&s)
			return Value{}, err
		}
		arr = append(arr, v)
	}
	*arrPtr = arr
	return Value{Type: ty, Array: arr, pooledArr: true}, nil
}

func (r *Reader) readMap(ty Type) (Value, error) {
	n, err := r.readInt()
	if err != nil {
		return Value{}, err
	}
	if n == -1 {
		return Value{Type: TyNull}, nil
	}
	if n < 0 {
		return Value{}, ErrProtocol
	}
	if n > MaxArrayLen {
		return Value{}, ErrProtocolOversizedArray
	}
	mPtr := r.mapPool.Get().(*[]KV)
	m := (*mPtr)[:0]
	if cap(m) < int(n) {
		m = make([]KV, 0, n)
	}
	for i := int64(0); i < n; i++ {
		k, err := r.parse()
		if err != nil {
			s := m[:0]
			r.mapPool.Put(&s)
			return Value{}, err
		}
		v, err := r.parse()
		if err != nil {
			s := m[:0]
			r.mapPool.Put(&s)
			return Value{}, err
		}
		m = append(m, KV{K: k, V: v})
	}
	*mPtr = m
	return Value{Type: ty, Map: m, pooledMap: true}, nil
}

// parseInt parses an ASCII decimal signed integer.
// parseUint is the unsigned fast path for parseInt. It handles the common case
// (positive integer, no sign prefix) and is small enough for the compiler to
// inline, avoiding the function-call overhead on the hot RESP integer parsing
// path.
func parseUint(b []byte) (int64, error) {
	if len(b) == 0 {
		return 0, ErrProtocol
	}
	const cutoff = math.MaxInt64 / 10
	var n int64
	for _, d := range b {
		if d < '0' || d > '9' {
			return 0, ErrProtocol
		}
		if n > cutoff {
			return 0, ErrProtocol
		}
		n = n*10 + int64(d-'0')
		if n < 0 { // overflow
			return 0, ErrProtocol
		}
	}
	return n, nil
}

func parseInt(b []byte) (int64, error) {
	if len(b) == 0 {
		return 0, ErrProtocol
	}
	// Fast path: no sign prefix (vast majority of Redis integer replies).
	if b[0] >= '0' && b[0] <= '9' {
		return parseUint(b)
	}
	neg := b[0] == '-'
	if !neg && b[0] != '+' {
		return 0, ErrProtocol
	}
	if len(b) == 1 {
		return 0, ErrProtocol
	}
	n, err := parseUint(b[1:])
	if err != nil {
		// Special case: math.MinInt64 has magnitude 9223372036854775808 which
		// overflows int64. parseUint rejects it, but for a negative integer
		// this is the valid representation of MinInt64.
		if neg && string(b[1:]) == "9223372036854775808" {
			return math.MinInt64, nil
		}
		return 0, err
	}
	if neg {
		return -n, nil
	}
	return n, nil
}

func parseFloat(b []byte) (float64, error) {
	// RESP3 allows "inf", "-inf", "nan" and standard decimal.
	return strconv.ParseFloat(string(b), 64)
}

// Writer serializes RESP2 command arrays suitable for any Redis server. Only
// the RESP2 array-of-bulk-strings "inline command" form is emitted — servers
// accept this even when negotiated to RESP3 for responses.
type Writer struct {
	buf []byte
}

// NewWriter returns a Writer with a small pre-allocated buffer.
func NewWriter() *Writer {
	return &Writer{buf: make([]byte, 0, 64)}
}

// NewWriterSize returns a Writer with a pre-allocated buffer of at least size
// bytes. Use this for pipeline workloads where the total encoded size is known
// or estimable, to avoid repeated grow-copies.
func NewWriterSize(size int) *Writer {
	return &Writer{buf: make([]byte, 0, size)}
}

// Grow ensures the Writer's internal buffer has at least n bytes of unused
// capacity. If the buffer already has sufficient capacity, this is a no-op.
func (w *Writer) Grow(n int) {
	if cap(w.buf)-len(w.buf) >= n {
		return
	}
	buf := make([]byte, len(w.buf), len(w.buf)+n)
	copy(buf, w.buf)
	w.buf = buf
}

// Reset empties the internal buffer without freeing it.
func (w *Writer) Reset() { w.buf = w.buf[:0] }

// Bytes returns the serialized bytes.
func (w *Writer) Bytes() []byte { return w.buf }

// WriteCommand appends *N\r\n$len\r\nARG\r\n... for the given string args and
// returns the full internal buffer slice. Callers that issue several commands
// before flushing should call [Writer.Reset] between commands — or use
// [Writer.AppendCommand] to append a second command after the first.
func (w *Writer) WriteCommand(args ...string) []byte {
	w.buf = w.buf[:0]
	return w.AppendCommand(args...)
}

// AppendCommand appends one command to the buffer without resetting.
func (w *Writer) AppendCommand(args ...string) []byte {
	w.buf = append(w.buf, '*')
	w.buf = strconv.AppendInt(w.buf, int64(len(args)), 10)
	w.buf = append(w.buf, '\r', '\n')
	for _, a := range args {
		w.buf = append(w.buf, '$')
		w.buf = strconv.AppendInt(w.buf, int64(len(a)), 10)
		w.buf = append(w.buf, '\r', '\n')
		w.buf = append(w.buf, a...)
		w.buf = append(w.buf, '\r', '\n')
	}
	return w.buf
}

// appendBulk writes one RESP bulk string element ($len\r\ndata\r\n).
func (w *Writer) appendBulk(s string) {
	w.buf = append(w.buf, '$')
	w.buf = strconv.AppendInt(w.buf, int64(len(s)), 10)
	w.buf = append(w.buf, '\r', '\n')
	w.buf = append(w.buf, s...)
	w.buf = append(w.buf, '\r', '\n')
}

// AppendCommand1 appends a 1-arg command without allocating a []string slice.
func (w *Writer) AppendCommand1(a0 string) []byte {
	w.buf = append(w.buf, '*', '1', '\r', '\n')
	w.appendBulk(a0)
	return w.buf
}

// AppendCommand2 appends a 2-arg command without allocating a []string slice.
func (w *Writer) AppendCommand2(a0, a1 string) []byte {
	w.buf = append(w.buf, '*', '2', '\r', '\n')
	w.appendBulk(a0)
	w.appendBulk(a1)
	return w.buf
}

// AppendCommand3 appends a 3-arg command without allocating a []string slice.
func (w *Writer) AppendCommand3(a0, a1, a2 string) []byte {
	w.buf = append(w.buf, '*', '3', '\r', '\n')
	w.appendBulk(a0)
	w.appendBulk(a1)
	w.appendBulk(a2)
	return w.buf
}

// AppendCommand4 appends a 4-arg command without allocating a []string slice.
func (w *Writer) AppendCommand4(a0, a1, a2, a3 string) []byte {
	w.buf = append(w.buf, '*', '4', '\r', '\n')
	w.appendBulk(a0)
	w.appendBulk(a1)
	w.appendBulk(a2)
	w.appendBulk(a3)
	return w.buf
}

// AppendCommand5 appends a 5-arg command without allocating a []string slice.
func (w *Writer) AppendCommand5(a0, a1, a2, a3, a4 string) []byte {
	w.buf = append(w.buf, '*', '5', '\r', '\n')
	w.appendBulk(a0)
	w.appendBulk(a1)
	w.appendBulk(a2)
	w.appendBulk(a3)
	w.appendBulk(a4)
	return w.buf
}

// WriteCommandBytes is the []byte variant, avoiding the string conversion.
func (w *Writer) WriteCommandBytes(args [][]byte) []byte {
	w.buf = w.buf[:0]
	return w.AppendCommandBytes(args)
}

// AppendCommandBytes appends one []byte command.
func (w *Writer) AppendCommandBytes(args [][]byte) []byte {
	w.buf = append(w.buf, '*')
	w.buf = strconv.AppendInt(w.buf, int64(len(args)), 10)
	w.buf = append(w.buf, '\r', '\n')
	for _, a := range args {
		w.buf = append(w.buf, '$')
		w.buf = strconv.AppendInt(w.buf, int64(len(a)), 10)
		w.buf = append(w.buf, '\r', '\n')
		w.buf = append(w.buf, a...)
		w.buf = append(w.buf, '\r', '\n')
	}
	return w.buf
}
