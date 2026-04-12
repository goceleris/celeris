package websocket

import (
	"encoding/binary"
	"unsafe"
)

// maskBytes applies the WebSocket XOR mask to payload in-place.
// The mask key is 4 bytes. The mask is applied from offset 0 into the
// mask cycle.
func maskBytes(mask [4]byte, b []byte) {
	if len(b) == 0 {
		return
	}

	// Build 64-bit mask word.
	maskWord := binary.LittleEndian.Uint32(mask[:])
	mask64 := uint64(maskWord) | uint64(maskWord)<<32

	// Process 8 bytes at a time using unsafe pointer arithmetic to
	// eliminate bounds checks in the inner loop. The loop is structured
	// so that the pointer is never advanced past the last byte of the
	// underlying allocation — Go's checkptr arithmetic checker rejects
	// one-past-end pointers, which can be produced by len(b) being an
	// exact multiple of 8 over a precisely-sized backing array.
	n := len(b)
	if chunks := n >> 3; chunks > 0 {
		p := unsafe.Pointer(unsafe.SliceData(b))
		for i := 0; i < chunks-1; i++ {
			*(*uint64)(p) ^= mask64
			p = unsafe.Add(p, 8)
		}
		// Final 8-byte chunk: XOR without advancing past the end.
		*(*uint64)(p) ^= mask64
		done := chunks << 3
		b = b[done:]
	}

	// Process 4 bytes.
	if len(b) >= 4 {
		v := binary.LittleEndian.Uint32(b)
		binary.LittleEndian.PutUint32(b, v^maskWord)
		b = b[4:]
	}

	// Remaining 0-3 bytes.
	for i := range b {
		b[i] ^= mask[i&3]
	}
}
