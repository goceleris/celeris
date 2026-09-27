//line mask.go:1:1
package websocket; import _cover_atomic_ "sync/atomic"

import (
	"encoding/binary"
	"unsafe"
)

// maskBytes applies the WebSocket XOR mask to payload in-place.
// The mask key is 4 bytes. The mask is applied from offset 0 into the
// mask cycle.
func maskBytes(mask [4]byte, b []byte) {_cover_atomic_.AddUint32(&GoCover_b2b_mask.Count[0], 1);
	if len(b) == 0 {_cover_atomic_.AddUint32(&GoCover_b2b_mask.Count[5], 1);
		return
	}

	// Build 64-bit mask word.
	_cover_atomic_.AddUint32(&GoCover_b2b_mask.Count[1], 1);maskWord := binary.LittleEndian.Uint32(mask[:])
	mask64 := uint64(maskWord) | uint64(maskWord)<<32

	// Process 8 bytes at a time using unsafe pointer arithmetic to
	// eliminate bounds checks in the inner loop. The loop is structured
	// so that the pointer is never advanced past the last byte of the
	// underlying allocation — Go's checkptr arithmetic checker rejects
	// one-past-end pointers, which can be produced by len(b) being an
	// exact multiple of 8 over a precisely-sized backing array.
	_cover_atomic_.AddUint32(&GoCover_b2b_mask.Count[2], 1);n := len(b)
	if chunks := n >> 3; chunks > 0 {_cover_atomic_.AddUint32(&GoCover_b2b_mask.Count[6], 1);
		p := unsafe.Pointer(unsafe.SliceData(b))
		for i := 0; i < chunks-1; i++ {_cover_atomic_.AddUint32(&GoCover_b2b_mask.Count[8], 1);
			*(*uint64)(p) ^= mask64
			p = unsafe.Add(p, 8)
		}
		// Final 8-byte chunk: XOR without advancing past the end.
		_cover_atomic_.AddUint32(&GoCover_b2b_mask.Count[7], 1);*(*uint64)(p) ^= mask64
		done := chunks << 3
		b = b[done:]
	}

	// Process 4 bytes.
	_cover_atomic_.AddUint32(&GoCover_b2b_mask.Count[3], 1);if len(b) >= 4 {_cover_atomic_.AddUint32(&GoCover_b2b_mask.Count[9], 1);
		v := binary.LittleEndian.Uint32(b)
		binary.LittleEndian.PutUint32(b, v^maskWord)
		b = b[4:]
	}

	// Remaining 0-3 bytes.
	_cover_atomic_.AddUint32(&GoCover_b2b_mask.Count[4], 1);for i := range b {_cover_atomic_.AddUint32(&GoCover_b2b_mask.Count[10], 1);
		b[i] ^= mask[i&3]
	}
}

// maskBytesOffset applies the WebSocket XOR mask starting at position
// `offset` within the mask cycle. Used by streaming frame readers that
// consume a payload in multiple chunks — each chunk is masked as if its
// first byte were at index `offset` of the full-frame mask sequence.
func maskBytesOffset(mask [4]byte, b []byte, offset int) {_cover_atomic_.AddUint32(&GoCover_b2b_mask.Count[11], 1);
	if len(b) == 0 {_cover_atomic_.AddUint32(&GoCover_b2b_mask.Count[13], 1);
		return
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_mask.Count[12], 1);var rotated [4]byte
	rotated[0] = mask[offset&3]
	rotated[1] = mask[(offset+1)&3]
	rotated[2] = mask[(offset+2)&3]
	rotated[3] = mask[(offset+3)&3]
	maskBytes(rotated, b)
}

var GoCover_b2b_mask = struct {
	Count     [14]uint32
	Pos       [3 * 14]uint32
	NumStmt   [14]uint16
} {
	Pos: [3 * 14]uint32{
		12, 12, 0x110002, // [0]
		17, 19, 0x10002, // [1]
		26, 27, 0x220002, // [2]
		40, 40, 0x110002, // [3]
		47, 47, 0x130002, // [4]
		13, 14, 0x10003, // [5]
		28, 29, 0x210003, // [6]
		34, 36, 0xf0003, // [7]
		30, 32, 0x10004, // [8]
		41, 44, 0x10003, // [9]
		48, 49, 0x10003, // [10]
		57, 57, 0x110002, // [11]
		60, 65, 0x170002, // [12]
		58, 59, 0x10003, // [13]
	},
	NumStmt: [14]uint16{
		1, // 0
		4, // 1
		4, // 2
		1, // 3
		1, // 4
		1, // 5
		2, // 6
		3, // 7
		2, // 8
		3, // 9
		1, // 10
		1, // 11
		6, // 12
		1, // 13
	},
}

var _ = _cover_atomic_.LoadUint32
