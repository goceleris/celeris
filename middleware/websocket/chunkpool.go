package websocket

import (
	"math/bits"
	"sync"
	"unsafe"
)

// Inbound chunk buffers for the engine path (celeris#756).
//
// Every read the engine hands to the WebSocket callback is copied before the
// callback returns (the engine reuses its read buffer), and the copy used to be
// a fresh make([]byte, n) per chunk: a 65550-byte masked frame arriving in
// 8 KiB reads cost eight allocations on the loop thread, ~64 KiB of garbage per
// message, charged to the loop as GC assist and as mark work on the few Ps the
// handlers share with the GC. chunkPools recycles those buffers instead.
//
// Ownership is exact: a pooled buffer is created by chanReader.AppendCopy,
// travels through the channel or the spill to chanReader.Read, which copies it
// into the caller's buffer and returns it here once the last byte has been
// copied. Nothing else holds a reference: the chunk never leaves chanReader.
// A chunk that is never read (the reader closed with chunks buffered) is simply
// left to the garbage collector, which is always safe.

const (
	chunkPoolMinShift = 9  // 512 B: smaller reads are not worth a pool round trip
	chunkPoolMaxShift = 18 // 256 KiB: the largest Config.BufferSize
)

// chunkPools[i] holds buffers of exactly 1<<(i+chunkPoolMinShift) bytes. The
// pooled value is the *byte at the head of the array, not the slice: a slice
// stored in a sync.Pool is boxed and allocates its header on every Put.
var chunkPools [chunkPoolMaxShift - chunkPoolMinShift + 1]sync.Pool

// getChunk returns a buffer of length n whose capacity is a pool size class,
// or a plain, exactly sized allocation when n is outside the classes: a read
// under the smallest class is not rounded up (a backed-up reader holds up to
// two channels of chunks, and rounding 100-byte reads to 512 B each would pin
// five times their size). Its contents are undefined: callers overwrite all n
// bytes.
func getChunk(n int) []byte {
	if n <= 0 {
		return nil
	}
	if n < 1<<chunkPoolMinShift {
		return make([]byte, n)
	}
	shift := bits.Len(uint(n - 1))
	if shift > chunkPoolMaxShift {
		return make([]byte, n)
	}
	size := 1 << shift
	if p, ok := chunkPools[shift-chunkPoolMinShift].Get().(*byte); ok {
		return unsafe.Slice(p, size)[:n]
	}
	return make([]byte, n, size)
}

// putChunk returns b to its size class. Buffers that did not come from
// getChunk (capacity not an exact class size) are ignored. b must not be used
// after the call.
func putChunk(b []byte) {
	c := cap(b)
	if c < 1<<chunkPoolMinShift || c > 1<<chunkPoolMaxShift || c&(c-1) != 0 {
		return
	}
	chunkPools[bits.TrailingZeros(uint(c))-chunkPoolMinShift].Put(unsafe.SliceData(b))
}
