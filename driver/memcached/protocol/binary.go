package protocol

import (
	"encoding/binary"
)

// Binary-protocol constants. See
// https://github.com/memcached/memcached/wiki/BinaryProtocolRevamped.
const (
	// BinHeaderLen is the fixed 24-byte binary-protocol header size.
	BinHeaderLen = 24

	// MagicRequest marks a request packet (client → server).
	MagicRequest byte = 0x80
	// MagicResponse marks a response packet (server → client).
	MagicResponse byte = 0x81
)

// Binary opcodes. Only the subset actually used by the driver is enumerated;
// the parser/encoder is still generic over the full 1-byte opcode range.
const (
	// OpGet retrieves a value (no extras on request).
	OpGet byte = 0x00
	// OpSet stores a value (4-byte flags + 4-byte exptime extras on request).
	OpSet byte = 0x01
	// OpAdd stores a value only if the key does not exist.
	OpAdd byte = 0x02
	// OpReplace stores a value only if the key already exists.
	OpReplace byte = 0x03
	// OpDelete removes a key.
	OpDelete byte = 0x04
	// OpIncrement atomically adds to a counter.
	OpIncrement byte = 0x05
	// OpDecrement atomically subtracts from a counter.
	OpDecrement byte = 0x06
	// OpQuit closes the connection.
	OpQuit byte = 0x07
	// OpFlush wipes all data (optional 4-byte expiration extras).
	OpFlush byte = 0x08
	// OpGetQ is the quiet variant of GET (no reply on miss).
	OpGetQ byte = 0x09
	// OpNoop is a no-op that forces a reply (used to terminate multi-get
	// pipelines made of OpGetQ packets).
	OpNoop byte = 0x0a
	// OpVersion returns the server version string as the value body.
	OpVersion byte = 0x0b
	// OpGetK is like OpGet but includes the key in the response body.
	OpGetK byte = 0x0c
	// OpGetKQ is the quiet variant of OpGetK.
	OpGetKQ byte = 0x0d
	// OpAppend appends to an existing value.
	OpAppend byte = 0x0e
	// OpPrepend prepends to an existing value.
	OpPrepend byte = 0x0f
	// OpStat requests server statistics (multi-reply terminated by a zero-
	// length key response).
	OpStat byte = 0x10
	// OpTouch updates the expiration of a key without fetching the value.
	OpTouch byte = 0x1c
	// OpGAT gets a value and updates its expiration.
	OpGAT byte = 0x1d
	// OpGATQ is the quiet variant of OpGAT.
	OpGATQ byte = 0x1e
)

// Binary status codes.
const (
	// StatusOK indicates a successful reply.
	StatusOK uint16 = 0x0000
	// StatusKeyNotFound indicates the key does not exist.
	StatusKeyNotFound uint16 = 0x0001
	// StatusKeyExists indicates the key already exists or CAS mismatch.
	StatusKeyExists uint16 = 0x0002
	// StatusValueTooLarge indicates the value exceeded the server's limit.
	StatusValueTooLarge uint16 = 0x0003
	// StatusInvalidArgs indicates malformed request.
	StatusInvalidArgs uint16 = 0x0004
	// StatusItemNotStored indicates add/replace preconditions failed.
	StatusItemNotStored uint16 = 0x0005
	// StatusNonNumeric indicates incr/decr on a non-numeric value.
	StatusNonNumeric uint16 = 0x0006
	// StatusUnknownCommand indicates an unknown opcode.
	StatusUnknownCommand uint16 = 0x0081
	// StatusOutOfMemory indicates server-side OOM.
	StatusOutOfMemory uint16 = 0x0082
)

// BinaryHeader is the fixed 24-byte packet header. Magic + opcode + lengths +
// status/vbucket + opaque + CAS.
type BinaryHeader struct {
	Magic       byte
	Opcode      byte
	KeyLen      uint16
	ExtrasLen   uint8
	DataType    uint8
	VBucketOrSt uint16 // vbucket (request) or status (response)
	BodyLen     uint32
	Opaque      uint32
	CAS         uint64
}

// BinaryPacket is one decoded binary-protocol packet. Extras/Key/Value slices
// alias the reader's internal buffer; copy before retaining past the next
// Feed/Compact cycle.
type BinaryPacket struct {
	Header BinaryHeader
	Extras []byte
	Key    []byte
	Value  []byte
}

// Status returns the response status (response packets only).
func (p *BinaryPacket) Status() uint16 { return p.Header.VBucketOrSt }

// BinaryReader streams binary-protocol packets.
type BinaryReader struct {
	buf []byte
	r   int
	w   int
}

// NewBinaryReader returns a ready-to-use BinaryReader.
func NewBinaryReader() *BinaryReader { return &BinaryReader{} }

// Feed appends data to the internal buffer.
func (r *BinaryReader) Feed(data []byte) {
	if len(data) == 0 {
		return
	}
	r.buf = append(r.buf, data...)
	r.w = len(r.buf)
}

// Reset clears internal state while retaining the buffer for reuse.
func (r *BinaryReader) Reset() {
	r.buf = r.buf[:0]
	r.r = 0
	r.w = 0
}

// Compact discards already-parsed bytes.
func (r *BinaryReader) Compact() {
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

// Next parses one complete packet. Returns [ErrIncomplete] if more bytes are
// needed; the read cursor is restored in that case.
func (r *BinaryReader) Next() (BinaryPacket, error) {
	start := r.r
	if r.w-r.r < BinHeaderLen {
		return BinaryPacket{}, ErrIncomplete
	}
	h := r.buf[r.r : r.r+BinHeaderLen]
	hdr := BinaryHeader{
		Magic:       h[0],
		Opcode:      h[1],
		KeyLen:      binary.BigEndian.Uint16(h[2:4]),
		ExtrasLen:   h[4],
		DataType:    h[5],
		VBucketOrSt: binary.BigEndian.Uint16(h[6:8]),
		BodyLen:     binary.BigEndian.Uint32(h[8:12]),
		Opaque:      binary.BigEndian.Uint32(h[12:16]),
		CAS:         binary.BigEndian.Uint64(h[16:24]),
	}
	if hdr.Magic != MagicRequest && hdr.Magic != MagicResponse {
		return BinaryPacket{}, ErrProtocol
	}
	if hdr.BodyLen > MaxValueLen {
		return BinaryPacket{}, ErrProtocol
	}
	total := BinHeaderLen + int(hdr.BodyLen)
	if r.w-r.r < total {
		r.r = start
		return BinaryPacket{}, ErrIncomplete
	}
	body := r.buf[r.r+BinHeaderLen : r.r+total]
	r.r += total

	extrasEnd := int(hdr.ExtrasLen)
	keyEnd := extrasEnd + int(hdr.KeyLen)
	if keyEnd > len(body) {
		return BinaryPacket{}, ErrProtocol
	}
	pkt := BinaryPacket{Header: hdr}
	if extrasEnd > 0 {
		pkt.Extras = body[:extrasEnd]
	}
	if keyEnd > extrasEnd {
		pkt.Key = body[extrasEnd:keyEnd]
	}
	if len(body) > keyEnd {
		pkt.Value = body[keyEnd:]
	}
	return pkt, nil
}

// BinaryWriter builds binary-protocol requests into a reusable byte buffer.
type BinaryWriter struct {
	buf []byte
}

// NewBinaryWriter returns a BinaryWriter with a small pre-allocated buffer.
func NewBinaryWriter() *BinaryWriter {
	return &BinaryWriter{buf: make([]byte, 0, 128)}
}

// Reset empties the internal buffer without freeing it.
func (w *BinaryWriter) Reset() { w.buf = w.buf[:0] }

// Bytes returns the serialized buffer.
func (w *BinaryWriter) Bytes() []byte { return w.buf }

// AppendRequest encodes a request packet with the given opcode, extras, key,
// value, cas, and opaque. The buffer is appended to — call [BinaryWriter.Reset]
// to start a fresh packet.
func (w *BinaryWriter) AppendRequest(opcode byte, extras, key, value []byte, cas uint64, opaque uint32) []byte {
	var hdr [BinHeaderLen]byte
	hdr[0] = MagicRequest
	hdr[1] = opcode
	binary.BigEndian.PutUint16(hdr[2:4], uint16(len(key)))
	hdr[4] = byte(len(extras))
	hdr[5] = 0 // DataType: always 0 in revamped spec.
	binary.BigEndian.PutUint16(hdr[6:8], 0)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(extras)+len(key)+len(value)))
	binary.BigEndian.PutUint32(hdr[12:16], opaque)
	binary.BigEndian.PutUint64(hdr[16:24], cas)
	w.buf = append(w.buf, hdr[:]...)
	w.buf = append(w.buf, extras...)
	w.buf = append(w.buf, key...)
	w.buf = append(w.buf, value...)
	return w.buf
}

// AppendStorage encodes a storage-class request (set/add/replace). The
// 8-byte extras carry flags (4) + exptime (4), big-endian.
func (w *BinaryWriter) AppendStorage(opcode byte, key string, value []byte, flags uint32, exptime uint32, cas uint64, opaque uint32) []byte {
	var extras [8]byte
	binary.BigEndian.PutUint32(extras[0:4], flags)
	binary.BigEndian.PutUint32(extras[4:8], exptime)
	return w.AppendRequest(opcode, extras[:], []byte(key), value, cas, opaque)
}

// AppendConcat encodes append/prepend which have no extras.
func (w *BinaryWriter) AppendConcat(opcode byte, key string, value []byte, cas uint64, opaque uint32) []byte {
	return w.AppendRequest(opcode, nil, []byte(key), value, cas, opaque)
}

// AppendGet encodes a simple GET (or GetK) with no extras.
func (w *BinaryWriter) AppendGet(opcode byte, key string, opaque uint32) []byte {
	return w.AppendRequest(opcode, nil, []byte(key), nil, 0, opaque)
}

// AppendDelete encodes a DELETE with no extras.
func (w *BinaryWriter) AppendDelete(key string, cas uint64, opaque uint32) []byte {
	return w.AppendRequest(OpDelete, nil, []byte(key), nil, cas, opaque)
}

// AppendArith encodes an INCREMENT / DECREMENT packet. Extras are
// delta(8) + initial(8) + exptime(4) = 20 bytes, big-endian.
func (w *BinaryWriter) AppendArith(opcode byte, key string, delta, initial uint64, exptime uint32, opaque uint32) []byte {
	var extras [20]byte
	binary.BigEndian.PutUint64(extras[0:8], delta)
	binary.BigEndian.PutUint64(extras[8:16], initial)
	binary.BigEndian.PutUint32(extras[16:20], exptime)
	return w.AppendRequest(opcode, extras[:], []byte(key), nil, 0, opaque)
}

// AppendTouch encodes a TOUCH packet. Extras carry exptime(4).
func (w *BinaryWriter) AppendTouch(key string, exptime uint32, opaque uint32) []byte {
	var extras [4]byte
	binary.BigEndian.PutUint32(extras[0:4], exptime)
	return w.AppendRequest(OpTouch, extras[:], []byte(key), nil, 0, opaque)
}

// AppendGAT encodes a Get-And-Touch (or GATQ) packet. Extras carry exptime(4).
func (w *BinaryWriter) AppendGAT(opcode byte, key string, exptime uint32, opaque uint32) []byte {
	var extras [4]byte
	binary.BigEndian.PutUint32(extras[0:4], exptime)
	return w.AppendRequest(opcode, extras[:], []byte(key), nil, 0, opaque)
}

// AppendFlush encodes a FLUSH packet with an optional exptime extra.
func (w *BinaryWriter) AppendFlush(exptime uint32, opaque uint32) []byte {
	if exptime == 0 {
		return w.AppendRequest(OpFlush, nil, nil, nil, 0, opaque)
	}
	var extras [4]byte
	binary.BigEndian.PutUint32(extras[0:4], exptime)
	return w.AppendRequest(OpFlush, extras[:], nil, nil, 0, opaque)
}

// AppendSimple encodes a packet with no extras/key/value (e.g. version, noop,
// quit, stats-without-arg).
func (w *BinaryWriter) AppendSimple(opcode byte, opaque uint32) []byte {
	return w.AppendRequest(opcode, nil, nil, nil, 0, opaque)
}

// AppendStats encodes STATS with an optional sub-statistic key.
func (w *BinaryWriter) AppendStats(arg string, opaque uint32) []byte {
	return w.AppendRequest(OpStat, nil, []byte(arg), nil, 0, opaque)
}
