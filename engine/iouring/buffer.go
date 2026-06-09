//go:build linux

package iouring

import (
	"fmt"
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
}

// NewBufferRing creates and registers a ring-mapped provided buffer group.
// The buffers are mmap'd outside the Go heap. count must be a power of 2.
func NewBufferRing(ring *Ring, groupID uint16, count, size int) (*BufferRing, error) {
	if count&(count-1) != 0 {
		return nil, fmt.Errorf("buffer ring count must be power of 2, got %d", count)
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
	entry.Addr = uint64(uintptr(unsafe.Pointer(&br.bufRegion[offset])))
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
	offset := int(bufID) * br.bufferSize
	return br.bufRegion[offset : offset+dataLen]
}

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
		_ = unix.Munmap(br.bufRegion)
	}
	if br.ringRegion != nil {
		_ = unix.Munmap(br.ringRegion)
	}
}

// BufferGroup managed a group of provided buffers for multishot recv using
// the legacy PROVIDE_BUFFERS SQE. Removed in v1.5.0 (celeris#320) — the
// kernel only reports a single completion per buffer when using
// PROVIDE_BUFFERS (vs. one for the head + one for the tail when using
// IORING_REGISTER_PBUF_RING), and the multishot recv path is opt-in
// anyway (CELERIS_IOURING_MULTISHOT_RECV=1). The BufferRing type above
// is the supported path.
