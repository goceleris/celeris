//go:build linux

package iouring

import (
	"fmt"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// kernelTimespec mirrors struct __kernel_timespec.
type kernelTimespec struct {
	Sec  int64
	Nsec int64
}

// geteventsArg mirrors struct io_uring_getevents_arg (used with IORING_ENTER_EXT_ARG).
type geteventsArg struct {
	Sigmask   uint64
	SigmaskSz uint32
	Pad       uint32
	Ts        uint64 // pointer to kernelTimespec
}

// ioUringParams mirrors the kernel io_uring_params struct.
type ioUringParams struct {
	sqEntries    uint32
	cqEntries    uint32
	flags        uint32
	sqThreadCPU  uint32
	sqThreadIdle uint32
	features     uint32
	wqFD         uint32
	resv         [3]uint32
	sqOff        sqRingOffsets
	cqOff        cqRingOffsets
}

type sqRingOffsets struct {
	head        uint32
	tail        uint32
	ringMask    uint32
	ringEntries uint32
	flags       uint32
	dropped     uint32
	array       uint32
	resv1       uint32
	userAddr    uint64
}

type cqRingOffsets struct {
	head        uint32
	tail        uint32
	ringMask    uint32
	ringEntries uint32
	overflow    uint32
	cqes        uint32
	flags       uint32
	resv1       uint32
	userAddr    uint64
}

// Ring is an io_uring instance with mmap'd SQ/CQ rings and SQE array.
type Ring struct {
	fd       int
	sqRing   []byte
	cqRing   []byte
	sqes     []byte
	params   ioUringParams
	sqMask   uint32
	cqMask   uint32
	pending  uint32
	sqHead   unsafe.Pointer
	sqTail   unsafe.Pointer
	sqArray  unsafe.Pointer
	cqHead   unsafe.Pointer
	cqTail   unsafe.Pointer
	cqesBase unsafe.Pointer
}

// NewRing creates a new io_uring instance.
func NewRing(entries uint32, flags uint32) (*Ring, error) {
	var params ioUringParams
	params.flags = flags

	fd, _, errno := unix.Syscall(
		uintptr(sysIOUringSetup),
		uintptr(entries),
		uintptr(unsafe.Pointer(&params)),
		0,
	)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}

	r := &Ring{
		fd:     int(fd),
		params: params,
	}

	if err := r.mmap(); err != nil {
		_ = unix.Close(int(fd))
		return nil, err
	}

	return r, nil
}

func (r *Ring) mmap() error {
	sqSize := uint64(r.params.sqOff.array) + uint64(r.params.sqEntries)*4
	sqRing, err := unix.Mmap(r.fd, offSQRing, int(sqSize),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap sq ring: %w", err)
	}
	r.sqRing = sqRing

	cqSize := uint64(r.params.cqOff.cqes) + uint64(r.params.cqEntries)*uint64(cqeSize)
	cqRing, err := unix.Mmap(r.fd, offCQRing, int(cqSize),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		_ = unix.Munmap(sqRing)
		return fmt.Errorf("mmap cq ring: %w", err)
	}
	r.cqRing = cqRing

	sqesSize := uint64(r.params.sqEntries) * sqeSize
	sqes, err := unix.Mmap(r.fd, offSQEs, int(sqesSize),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		_ = unix.Munmap(sqRing)
		_ = unix.Munmap(cqRing)
		return fmt.Errorf("mmap sqes: %w", err)
	}
	r.sqes = sqes

	r.sqMask = *(*uint32)(unsafe.Pointer(&r.sqRing[r.params.sqOff.ringMask]))
	r.cqMask = *(*uint32)(unsafe.Pointer(&r.cqRing[r.params.cqOff.ringMask]))

	r.sqHead = unsafe.Pointer(&r.sqRing[r.params.sqOff.head])
	r.sqTail = unsafe.Pointer(&r.sqRing[r.params.sqOff.tail])
	r.sqArray = unsafe.Pointer(&r.sqRing[r.params.sqOff.array])
	r.cqHead = unsafe.Pointer(&r.cqRing[r.params.cqOff.head])
	r.cqTail = unsafe.Pointer(&r.cqRing[r.params.cqOff.tail])
	r.cqesBase = unsafe.Pointer(&r.cqRing[r.params.cqOff.cqes])

	return nil
}

// GetSQE returns a pointer to the next available SQE, or nil if the ring is full.
func (r *Ring) GetSQE() unsafe.Pointer {
	tail := atomic.LoadUint32((*uint32)(r.sqTail))
	head := atomic.LoadUint32((*uint32)(r.sqHead))
	if tail-head >= r.params.sqEntries {
		return nil
	}
	idx := tail & r.sqMask
	arrayPtr := (*uint32)(unsafe.Add(r.sqArray, uintptr(idx)*4))
	*arrayPtr = idx
	sqePtr := unsafe.Add(unsafe.Pointer(&r.sqes[0]), uintptr(idx)*sqeSize)
	clear(unsafe.Slice((*byte)(sqePtr), sqeSize))
	atomic.StoreUint32((*uint32)(r.sqTail), tail+1)
	r.pending++
	return sqePtr
}

// Submit submits pending SQEs to the kernel.
func (r *Ring) Submit() (int, error) {
	if r.pending == 0 {
		return 0, nil
	}
	n := r.pending
	r.pending = 0
	ret, _, errno := unix.Syscall6(
		uintptr(sysIOUringEnter),
		uintptr(r.fd),
		uintptr(n),
		0, 0, 0, 0,
	)
	if errno != 0 {
		return 0, fmt.Errorf("io_uring_enter submit: %w", errno)
	}
	return int(ret), nil
}

// WaitCQE waits for at least one CQE to become available.
func (r *Ring) WaitCQE() error {
	_, _, errno := unix.Syscall6(
		uintptr(sysIOUringEnter),
		uintptr(r.fd),
		0, 1,
		uintptr(enterGetEvents),
		0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("io_uring_enter wait: %w", errno)
	}
	return nil
}

// SubmitAndWait submits pending SQEs and waits for at least one CQE.
func (r *Ring) SubmitAndWait() error {
	n := r.pending
	r.pending = 0
	_, _, errno := unix.Syscall6(
		uintptr(sysIOUringEnter),
		uintptr(r.fd),
		uintptr(n),
		1,
		uintptr(enterGetEvents),
		0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("io_uring_enter submit+wait: %w", errno)
	}
	return nil
}

// SubmitAndWaitTimeout submits pending SQEs and waits for at least one CQE,
// with a timeout. Returns nil on timeout (ETIME) or EINTR.
func (r *Ring) SubmitAndWaitTimeout(timeout time.Duration) error {
	n := r.pending
	r.pending = 0

	ts := kernelTimespec{
		Sec:  int64(timeout / time.Second),
		Nsec: int64(timeout % time.Second),
	}
	arg := geteventsArg{
		Ts: uint64(uintptr(unsafe.Pointer(&ts))),
	}

	_, _, errno := unix.Syscall6(
		uintptr(sysIOUringEnter),
		uintptr(r.fd),
		uintptr(n),
		1,
		uintptr(enterGetEvents|enterExtArg),
		uintptr(unsafe.Pointer(&arg)),
		unsafe.Sizeof(arg),
	)
	if errno != 0 {
		if errno == unix.ETIME || errno == unix.EINTR {
			return nil
		}
		return fmt.Errorf("io_uring_enter submit+wait timeout: %w", errno)
	}
	return nil
}

// peekCQE returns the next completed CQE without blocking, or nil if empty.
func (r *Ring) peekCQE() *completionEntry {
	head := atomic.LoadUint32((*uint32)(r.cqHead))
	tail := atomic.LoadUint32((*uint32)(r.cqTail))
	if head == tail {
		return nil
	}
	idx := head & r.cqMask
	ptr := unsafe.Add(r.cqesBase, uintptr(idx)*cqeSize)
	return (*completionEntry)(ptr)
}

// AdvanceCQ advances the CQ head by one.
func (r *Ring) AdvanceCQ() {
	head := atomic.LoadUint32((*uint32)(r.cqHead))
	atomic.StoreUint32((*uint32)(r.cqHead), head+1)
}

// WakeupSQPoll wakes up the SQPOLL thread if it went to sleep.
func (r *Ring) WakeupSQPoll() error {
	_, _, errno := unix.Syscall6(
		uintptr(sysIOUringEnter),
		uintptr(r.fd),
		0, 0,
		uintptr(enterSQWakeup),
		0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("io_uring_enter wakeup: %w", errno)
	}
	return nil
}

// Close closes the ring and unmaps memory.
func (r *Ring) Close() error {
	if r.sqes != nil {
		_ = unix.Munmap(r.sqes)
	}
	if r.cqRing != nil {
		_ = unix.Munmap(r.cqRing)
	}
	if r.sqRing != nil {
		_ = unix.Munmap(r.sqRing)
	}
	return unix.Close(r.fd)
}
