//go:build linux

package iouring

import (
	"fmt"
	"unsafe"
)

// BufferGroup manages a group of provided buffers for multishot recv.
type BufferGroup struct {
	groupID    uint16
	buffers    [][]byte
	bufferSize int
	available  []bool
}

// NewBufferGroup creates and registers a provided buffer group with the ring.
func NewBufferGroup(ring *Ring, groupID uint16, count, size int) (*BufferGroup, error) {
	bg := &BufferGroup{
		groupID:    groupID,
		buffers:    make([][]byte, count),
		bufferSize: size,
		available:  make([]bool, count),
	}

	region := make([]byte, count*size)
	for i := range count {
		bg.buffers[i] = region[i*size : (i+1)*size]
		bg.available[i] = true
	}

	sqe := ring.GetSQE()
	if sqe == nil {
		return nil, fmt.Errorf("no SQE available for provide_buffers")
	}
	prepProvideBuffers(sqe, unsafe.Pointer(&region[0]), size, count, groupID, 0)
	setSQEUserData(sqe, encodeUserData(udProvide, 0))
	if _, err := ring.Submit(); err != nil {
		return nil, fmt.Errorf("provide_buffers submit: %w", err)
	}

	return bg, nil
}

// GetBuffer returns the buffer for the given buffer ID.
func (bg *BufferGroup) GetBuffer(bufID uint16) []byte {
	if int(bufID) >= len(bg.buffers) {
		return nil
	}
	bg.available[bufID] = false
	return bg.buffers[bufID]
}

// ReturnBuffer returns a buffer to the group and re-provides it to the kernel.
func (bg *BufferGroup) ReturnBuffer(ring *Ring, bufID uint16) error {
	if int(bufID) >= len(bg.buffers) {
		return fmt.Errorf("invalid buffer ID: %d", bufID)
	}
	bg.available[bufID] = true
	buf := bg.buffers[bufID]

	sqe := ring.GetSQE()
	if sqe == nil {
		return fmt.Errorf("no SQE available for provide_buffers")
	}
	prepProvideBuffers(sqe, unsafe.Pointer(&buf[0]), bg.bufferSize, 1, bg.groupID, bufID)
	setSQEUserData(sqe, encodeUserData(udProvide, 0))
	return nil
}

// AvailableCount returns the number of available buffers.
func (bg *BufferGroup) AvailableCount() int {
	n := 0
	for _, avail := range bg.available {
		if avail {
			n++
		}
	}
	return n
}
