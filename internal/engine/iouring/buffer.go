//go:build linux

package iouring

import (
	"fmt"
	"os"
	"slices"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// bufRingEntry mirrors struct io_uring_buf in the kernel. The ring is a
// contiguous array of these entries followed by a uint16 tail at offset 0
// of the first entry's resv field.
type bufRingEntry struct {
	Addr uint64
	Len  uint32
	Bid  uint16
	Resv uint16
}

const bufRingEntrySize = 16 // sizeof(bufRingEntry)

// BufferRing manages a ring-mapped provided buffer group (IORING_REGISTER_PBUF_RING).
// Both ring and buffer memory are allocated via mmap outside the Go heap to
// avoid inflating GC accounting.
type BufferRing struct {
	groupID    uint16
	count      int
	bufferSize int
	mask       uint16
	ringAddr   unsafe.Pointer // mmap'd ring header (io_uring_buf_ring)
	ringRegion []byte         // mmap'd ring entries (for cleanup)
	bufRegion  []byte         // mmap'd buffer memory (outside Go heap)
	tail       uint16         // local tail counter

	// repl[bid], when set, is the heap buffer that stands in for slot bid:
	// RetireBuffer put a request's buffer out of the ring's reach (a
	// hijacker keeps strings that view it) and gave the ring this one
	// instead. nil until the first retirement, so the common path pays one
	// nil check (pushEntry, GetBuffer). Worker thread only.
	repl [][]byte
	// kept lists the slots whose mmap'd bytes a retired buffer still is:
	// Close leaves their pages mapped for the life of the process. Each slot
	// appears once, whatever the number of retirements (the next retirement
	// of a slot finds repl set, and its old buffer is the garbage collector's).
	kept []uint16
	// retired counts RetireBuffer calls (tests, and the cost in the PR).
	retired uint64
}

// NewBufferRing creates and registers a ring-mapped provided buffer group.
// The buffers are mmap'd outside the Go heap. count must be a power of 2.
func NewBufferRing(ring *Ring, groupID uint16, count, size int) (*BufferRing, error) {
	if count&(count-1) != 0 {
		return nil, fmt.Errorf("buffer ring count must be power of 2, got %d", count)
	}
	// The kernel's IORING_REGISTER_PBUF_RING accepts at most 32768 entries,
	// and BufferRing's uint16 tail/mask/bid arithmetic cannot address more
	// than 32768 distinct buffer IDs without wrapping. Reject over-large
	// rings explicitly rather than letting the kernel return an opaque
	// EINVAL or, worse, silently aliasing buffer IDs (celeris#322).
	if count > bufRingCountMax {
		return nil, fmt.Errorf("buffer ring count %d exceeds kernel PBUF_RING cap of %d", count, bufRingCountMax)
	}

	// Allocate ring entry memory via mmap. Each entry is 16 bytes (io_uring_buf).
	ringSize := count * bufRingEntrySize
	ringRegion, err := unix.Mmap(-1, 0, ringSize,
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS|unix.MAP_POPULATE)
	if err != nil {
		return nil, fmt.Errorf("mmap ring region: %w", err)
	}
	ringAddr := unsafe.Pointer(&ringRegion[0])

	if err := ring.RegisterPbufRing(groupID, uint32(count), ringAddr); err != nil {
		_ = unix.Munmap(ringRegion)
		return nil, fmt.Errorf("register pbuf_ring: %w", err)
	}

	// Allocate buffer memory via mmap outside Go heap. This prevents GC from
	// accounting these bytes, which would otherwise cause GC to never trigger
	// on actual request allocations.
	totalSize := count * size
	bufRegion, err := unix.Mmap(-1, 0, totalSize,
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS|unix.MAP_POPULATE)
	if err != nil {
		_ = ring.UnregisterPbufRing(groupID)
		_ = unix.Munmap(ringRegion)
		return nil, fmt.Errorf("mmap buffer region: %w", err)
	}

	br := &BufferRing{
		groupID:    groupID,
		count:      count,
		bufferSize: size,
		mask:       uint16(count - 1),
		ringAddr:   ringAddr,
		ringRegion: ringRegion,
		bufRegion:  bufRegion,
		tail:       0,
	}

	// Populate all buffer entries into the ring.
	for i := range count {
		br.pushEntry(uint16(i))
	}
	// Publish all entries by storing the tail.
	br.publishTail()

	return br, nil
}

// pushEntry adds a buffer entry to the ring at the current tail position.
func (br *BufferRing) pushEntry(bufID uint16) {
	idx := br.tail & br.mask
	entry := (*bufRingEntry)(unsafe.Add(br.ringAddr, uintptr(idx)*bufRingEntrySize))
	offset := int(bufID) * br.bufferSize
	addr := unsafe.Pointer(&br.bufRegion[offset])
	if br.repl != nil && br.repl[bufID] != nil {
		addr = unsafe.Pointer(&br.repl[bufID][0])
	}
	entry.Addr = uint64(uintptr(addr))
	entry.Len = uint32(br.bufferSize)
	entry.Bid = bufID
	br.tail++
}

// publishTail makes all pushed entries visible to the kernel by storing the
// tail pointer with release ordering. The tail is a uint16 at ring offset 14.
// Since Go has no atomic.StoreUint16, we use a uint32 atomic store at offset 12
// which covers both the reserved field (offset 12, always 0) and the tail
// (offset 14). On little-endian (arm64/amd64), the tail occupies bits 16-31.
func (br *BufferRing) publishTail() {
	ptr := (*uint32)(unsafe.Add(br.ringAddr, 12))
	atomic.StoreUint32(ptr, uint32(br.tail)<<16)
}

// GetBuffer returns a slice of the buffer for the given buffer ID and data length.
func (br *BufferRing) GetBuffer(bufID uint16, dataLen int) []byte {
	if int(bufID) >= br.count || dataLen > br.bufferSize {
		return nil
	}
	if br.repl != nil && br.repl[bufID] != nil {
		return br.repl[bufID][:dataLen]
	}
	offset := int(bufID) * br.bufferSize
	return br.bufRegion[offset : offset+dataLen]
}

// RetireBuffer is PushBuffer for a buffer that must not be written again
// (celeris#868): it gives the ring a fresh entry under the same buffer ID, a
// new heap buffer, and leaves the buffer the kernel just filled to whoever
// holds a view of it. A hijacking handler keeps the strings it read from the
// request, which view that buffer, and hands them to the goroutine that
// serves the connection; pushed back, the buffer would hold the next
// connection's bytes by then.
//
// The retired bytes are the mmap'd slot the first time a slot is retired:
// Close then keeps that slot's pages mapped (kept). A slot already replaced
// holds a heap buffer, which the garbage collector frees once the last view
// of it is gone, so the pages kept are bounded by the ring's size, however
// many connections are hijacked. The fresh buffer is held by repl, as the
// kernel's pointer to it is invisible to the collector. Like PushBuffer it
// does not publish. Worker thread only.
//
// The cost is paid in steady state, on top of the mapped ring: the kernel
// cycles through the buffer IDs, so after about count hijacks nearly every
// slot is heap-backed, and the ring then holds count x bufferSize bytes of
// live Go heap (8 MiB per worker at the smallest ring, 1024 buffers of 8 KiB; up to bufRingCountMax x
// BufferSize), which the mmap design of NewBufferRing exists to keep off the
// heap, besides the mapped pages that are never reused. The alternative, a
// pool of replacements mapped outside the heap, would put the unmapping of
// buffers hijackers still view on the engine; it is not done here.
func (br *BufferRing) RetireBuffer(bufID uint16) {
	if br.repl == nil {
		br.repl = make([][]byte, br.count)
	}
	if br.repl[bufID] == nil {
		br.kept = append(br.kept, bufID)
	}
	br.repl[bufID] = make([]byte, br.bufferSize)
	br.retired++
	br.pushEntry(bufID)
}

// Retired returns how many buffers RetireBuffer has taken out of the ring.
func (br *BufferRing) Retired() uint64 { return br.retired }

// ReturnBuffer returns a buffer to the ring by pushing a new entry and
// publishing the updated tail. Must be called after the buffer data has been
// fully consumed.
func (br *BufferRing) ReturnBuffer(bufID uint16) {
	br.pushEntry(bufID)
	br.publishTail()
}

// PushBuffer queues a buffer for return without publishing to the kernel.
// Call PublishBuffers once after batching multiple PushBuffer calls.
func (br *BufferRing) PushBuffer(bufID uint16) {
	br.pushEntry(bufID)
}

// PublishBuffers makes all pushed entries visible to the kernel with a single
// atomic store. Call after one or more PushBuffer calls.
func (br *BufferRing) PublishBuffers() {
	br.publishTail()
}

// Close unregisters the buffer ring and releases mmap'd memory.
func (br *BufferRing) Close(ring *Ring) {
	_ = ring.UnregisterPbufRing(br.groupID)
	if br.bufRegion != nil {
		br.unmapBuffers()
	}
	if br.ringRegion != nil {
		_ = unix.Munmap(br.ringRegion)
	}
}

// unmapBuffers releases the buffer memory. A slot RetireBuffer kept stays
// mapped: a hijacking handler may still read the strings that view it
// (celeris#868), and the engine does not know when the last one goes. It
// unmaps everything else, so the memory left behind is the retired slots'
// pages, at most one ring's worth and none in a process that never hijacks
// under multishot receive. (The partial unmap leaves the region's entry in
// x/sys/unix's mmap bookkeeping, which Munmap of the whole slice would have
// removed. It is a map entry of a few words that lives as long as the mapping
// it names does, and nothing reads it again.) Worker thread only.
func (br *BufferRing) unmapBuffers() {
	region := br.bufRegion
	br.bufRegion = nil
	if len(br.kept) == 0 {
		_ = unix.Munmap(region)
		return
	}
	page := uintptr(os.Getpagesize())
	base := unsafe.Pointer(&region[0])
	end := (uintptr(len(region)) + page - 1) &^ (page - 1)
	ids := slices.Clone(br.kept)
	slices.Sort(ids)
	// [at, end) is what is still to be dealt with: each kept slot's pages
	// are skipped, and the gap before them unmapped.
	at := uintptr(0)
	for _, id := range ids {
		lo := uintptr(id) * uintptr(br.bufferSize) &^ (page - 1)
		hi := (uintptr(id)*uintptr(br.bufferSize) + uintptr(br.bufferSize) + page - 1) &^ (page - 1)
		if lo > at {
			_ = unix.MunmapPtr(unsafe.Add(base, at), lo-at)
		}
		at = max(at, hi)
	}
	if at < end {
		_ = unix.MunmapPtr(unsafe.Add(base, at), end-at)
	}
}

// BufferGroup managed a group of provided buffers for multishot recv using
// the legacy PROVIDE_BUFFERS SQE. Removed in v1.5.0 (celeris#320) — the
// kernel only reports a single completion per buffer when using
// PROVIDE_BUFFERS (vs. one for the head + one for the tail when using
// IORING_REGISTER_PBUF_RING), and the multishot recv path is opt-in
// anyway (CELERIS_IOURING_MULTISHOT_RECV=1). The BufferRing type above
// is the supported path.
