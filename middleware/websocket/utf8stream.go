package websocket

import "unicode/utf8"

// utf8Stream is an incremental UTF-8 validator that tolerates multi-byte
// sequences split across fragment boundaries. It implements the "fail
// fast" behavior Autobahn 6.4.x tests for: as soon as an invalid byte
// sequence is seen, the stream returns an error on the current fragment
// instead of deferring the check to the final message assembly.
//
// Up to 3 trailing bytes may remain pending between Feed calls (the
// maximum number of bytes that can start a valid 4-byte UTF-8 rune
// without completing it). Final=true on the last call enforces that no
// truncated sequence remains.
type utf8Stream struct {
	pending [4]byte
	n       int // 0..3 pending bytes
}

// feed validates data. If final is false, trailing truncated-but-possibly-
// valid bytes are carried over to the next call; if final is true, any
// remaining pending bytes are validated as a complete sequence. Returns
// false on the first byte position that definitely cannot be part of a
// valid UTF-8 rune given its context.
func (s *utf8Stream) feed(data []byte, final bool) bool {
	if s.n > 0 {
		// Concatenate pending + data into a small scratch buffer; we only
		// need this path for the rare fragment boundary that splits a
		// multi-byte rune, so the alloc is acceptable.
		buf := make([]byte, s.n+len(data))
		copy(buf, s.pending[:s.n])
		copy(buf[s.n:], data)
		s.n = 0
		data = buf
	}

	for len(data) > 0 {
		// ASCII fast path — most text is ASCII-heavy.
		if data[0] < 0x80 {
			data = data[1:]
			continue
		}
		if !utf8.FullRune(data) {
			// Truncated. Lawful only if more bytes are still coming.
			if final {
				return false
			}
			if len(data) > len(s.pending) {
				// Impossible — FullRune returns true for >4 bytes of any
				// content — but guard anyway.
				return false
			}
			s.n = copy(s.pending[:], data)
			return true
		}
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 {
			return false
		}
		data = data[size:]
	}
	return true
}

// reset clears pending state for reuse across messages.
func (s *utf8Stream) reset() { s.n = 0 }
