package store

import (
	"encoding/binary"
	"errors"
)

// ResponseWireVersion is the current wire format version for encoded
// HTTP responses stored in [KV] backends by cache and idempotency
// middleware.
const ResponseWireVersion byte = 1

// ErrInvalidWireFormat is returned by [DecodeResponse] when the buffer
// is truncated or carries an unknown version byte.
var ErrInvalidWireFormat = errors.New("store: invalid response wire format")

// EncodedResponse is a byte-efficient snapshot of an HTTP response used
// by cache and idempotency middleware to persist captured responses.
//
// Wire format (version 1):
//
//	[1 byte version=1]
//	[2 bytes status (big-endian)]
//	[2 bytes header_count (big-endian)]
//	for each header:
//	  [2 bytes key_len (big-endian)] [key_len bytes key]
//	  [2 bytes val_len (big-endian)] [val_len bytes value]
//	[remaining bytes: body]
type EncodedResponse struct {
	Status  int
	Headers [][2]string
	Body    []byte
}

// Encode serializes r into a new byte slice using the current wire
// format version.
func (r EncodedResponse) Encode() []byte {
	return r.AppendTo(nil)
}

// AppendTo serializes r into dst and returns the extended slice. Lets
// callers reuse a pooled buffer across requests (cache / idempotency
// MISS paths transfer the encoded bytes into a Store.Set that copies
// internally, so the buffer is free to recycle immediately after).
func (r EncodedResponse) AppendTo(dst []byte) []byte {
	size := 1 + 2 + 2
	for _, h := range r.Headers {
		size += 2 + len(h[0]) + 2 + len(h[1])
	}
	size += len(r.Body)

	// Grow once up-front so the append chain below never reallocates.
	if cap(dst)-len(dst) < size {
		grown := make([]byte, len(dst), len(dst)+size)
		copy(grown, dst)
		dst = grown
	}
	dst = append(dst, ResponseWireVersion)
	var tmp [2]byte
	binary.BigEndian.PutUint16(tmp[:], uint16(r.Status))
	dst = append(dst, tmp[:]...)
	binary.BigEndian.PutUint16(tmp[:], uint16(len(r.Headers)))
	dst = append(dst, tmp[:]...)
	for _, h := range r.Headers {
		binary.BigEndian.PutUint16(tmp[:], uint16(len(h[0])))
		dst = append(dst, tmp[:]...)
		dst = append(dst, h[0]...)
		binary.BigEndian.PutUint16(tmp[:], uint16(len(h[1])))
		dst = append(dst, tmp[:]...)
		dst = append(dst, h[1]...)
	}
	dst = append(dst, r.Body...)
	return dst
}

// DecodeResponse parses buf into an [EncodedResponse] following the
// versioned wire format. Returns [ErrInvalidWireFormat] if the version
// is unknown or the buffer is truncated.
func DecodeResponse(buf []byte) (EncodedResponse, error) {
	var r EncodedResponse
	if len(buf) < 5 {
		return r, ErrInvalidWireFormat
	}
	if buf[0] != ResponseWireVersion {
		return r, ErrInvalidWireFormat
	}
	r.Status = int(binary.BigEndian.Uint16(buf[1:3]))
	hdrCount := int(binary.BigEndian.Uint16(buf[3:5]))
	off := 5
	r.Headers = make([][2]string, 0, hdrCount)
	for i := 0; i < hdrCount; i++ {
		if len(buf)-off < 2 {
			return r, ErrInvalidWireFormat
		}
		kLen := int(binary.BigEndian.Uint16(buf[off : off+2]))
		off += 2
		if len(buf)-off < kLen+2 {
			return r, ErrInvalidWireFormat
		}
		key := string(buf[off : off+kLen])
		off += kLen
		vLen := int(binary.BigEndian.Uint16(buf[off : off+2]))
		off += 2
		if len(buf)-off < vLen {
			return r, ErrInvalidWireFormat
		}
		val := string(buf[off : off+vLen])
		off += vLen
		r.Headers = append(r.Headers, [2]string{key, val})
	}
	if off < len(buf) {
		// Share the caller-owned buf slice for the body. Callers (cache,
		// idempotency) pass buf sourced from a Store.Get, which returns
		// a caller-owned defensive copy — sharing lets us skip the extra
		// copy without exposing mutable store state. If a consumer
		// retains r.Body past buf's scope, Go's GC pins the whole
		// backing array via the slice header.
		r.Body = buf[off:]
	}
	return r, nil
}
