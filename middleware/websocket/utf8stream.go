//line utf8stream.go:1:1
package websocket; import _cover_atomic_ "sync/atomic"

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
func (s *utf8Stream) feed(data []byte, final bool) bool {_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[0], 1);
	if s.n > 0 {_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[3], 1);
		// Concatenate pending + data into a small scratch buffer; we only
		// need this path for the rare fragment boundary that splits a
		// multi-byte rune, so the alloc is acceptable.
		buf := make([]byte, s.n+len(data))
		copy(buf, s.pending[:s.n])
		copy(buf[s.n:], data)
		s.n = 0
		data = buf
	}

	_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[1], 1);for len(data) > 0 {_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[4], 1);
		// ASCII fast path — most text is ASCII-heavy.
		if data[0] < 0x80 {_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[8], 1);
			data = data[1:]
			continue
		}
		_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[5], 1);if !utf8.FullRune(data) {_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[9], 1);
			// Truncated. Lawful only if more bytes are still coming.
			if final {_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[12], 1);
				return false
			}
			_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[10], 1);if len(data) > len(s.pending) {_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[13], 1);
				// Impossible — FullRune returns true for >4 bytes of any
				// content — but guard anyway.
				return false
			}
			_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[11], 1);s.n = copy(s.pending[:], data)
			return true
		}
		_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[6], 1);r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 {_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[14], 1);
			return false
		}
		_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[7], 1);data = data[size:]
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[2], 1);return true
}

// reset clears pending state for reuse across messages.
func (s *utf8Stream) reset() {_cover_atomic_.AddUint32(&GoCover_b2b_utf8stream.Count[15], 1); s.n = 0 }

var GoCover_b2b_utf8stream = struct {
	Count     [16]uint32
	Pos       [3 * 16]uint32
	NumStmt   [16]uint16
} {
	Pos: [3 * 16]uint32{
		26, 26, 0xd0002, // [0]
		37, 37, 0x140002, // [1]
		62, 62, 0xd0002, // [2]
		30, 35, 0x10003, // [3]
		39, 39, 0x150003, // [4]
		43, 43, 0x1b0003, // [5]
		56, 57, 0x270003, // [6]
		60, 60, 0x150003, // [7]
		40, 41, 0xc0004, // [8]
		45, 45, 0xd0004, // [9]
		48, 48, 0x220004, // [10]
		53, 54, 0xf0004, // [11]
		46, 47, 0x10005, // [12]
		51, 52, 0x10005, // [13]
		58, 59, 0x10004, // [14]
		66, 66, 0x290020, // [15]
	},
	NumStmt: [16]uint16{
		1, // 0
		1, // 1
		1, // 2
		5, // 3
		1, // 4
		1, // 5
		2, // 6
		1, // 7
		2, // 8
		1, // 9
		1, // 10
		2, // 11
		1, // 12
		1, // 13
		1, // 14
		1, // 15
	},
}

var _ = _cover_atomic_.LoadUint32
