//go:build linux

package iouring

import (
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// minMemlockPerWorker is the conservative lower bound on the locked-page
// budget io_uring spends on each worker's ring + provided-buffer mmaps.
// Profiling shows 6-10 MB on common configs; 12 MB gives headroom so the
// pre-flight cap doesn't underestimate and let createWorkers ENOMEM after
// we already promised it'd fit.
const minMemlockPerWorker = 12 * 1024 * 1024

// capWorkersToMemlock returns min(want, RLIMIT_MEMLOCK / minMemlockPerWorker).
// When the soft limit is unlimited (RLIM_INFINITY), the request is honoured
// as-is. When the cap forces a reduction, the chosen logger is informed so
// the operator can spot the workaround and decide whether to raise the limit.
// A bottom of 1 worker is enforced — the caller's createWorkers / NewRing
// will surface a precise ENOMEM via ring.go's diagnostic if even that
// doesn't fit, which is a stronger signal than silently dying.
func capWorkersToMemlock(want int, logger *slog.Logger) int {
	if want <= 1 {
		return want
	}
	var rlim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_MEMLOCK, &rlim); err != nil {
		return want
	}
	if rlim.Cur == ^uint64(0) {
		return want
	}
	maxByMemlock := int(rlim.Cur / minMemlockPerWorker)
	if maxByMemlock < 1 {
		maxByMemlock = 1
	}
	if maxByMemlock >= want {
		return want
	}
	if logger != nil {
		logger.Warn("io_uring workers capped by RLIMIT_MEMLOCK",
			"requested", want,
			"capped_to", maxByMemlock,
			"memlock_cur_bytes", rlim.Cur,
			"min_per_worker_bytes", minMemlockPerWorker,
			"hint", "raise via `ulimit -l unlimited`, systemd LimitMEMLOCK=infinity, or run as root",
		)
	}
	return maxByMemlock
}

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
	fd           int
	sqRing       []byte
	cqRing       []byte
	sqes         []byte
	params       ioUringParams
	sqMask       uint32
	cqMask       uint32
	pending      uint32
	singleIssuer bool
	sqHead       unsafe.Pointer
	sqTail       unsafe.Pointer
	sqArray      unsafe.Pointer
	sqFlagsPtr   unsafe.Pointer // pointer to SQ ring flags (for SQPOLL wakeup check)
	cqHead       unsafe.Pointer
	cqTail       unsafe.Pointer
	cqesBase     unsafe.Pointer
}

// NewRing creates a new io_uring instance. sqPollIdle sets the kernel
// SQPOLL thread idle timeout in milliseconds (only used with IORING_SETUP_SQPOLL).
func NewRing(entries uint32, flags uint32, sqPollIdle uint32) (*Ring, error) {
	return NewRingCPU(entries, flags, sqPollIdle, -1)
}

// NewRingCPU creates a new io_uring instance with optional SQPOLL CPU affinity.
// If cpuID >= 0 and SQPOLL is enabled, IORING_SETUP_SQ_AFF is set to pin the
// kernel's SQPOLL thread to the specified CPU, ensuring NUMA-local processing.
func NewRingCPU(entries uint32, flags uint32, sqPollIdle uint32, cpuID int) (*Ring, error) {
	var params ioUringParams
	params.flags = flags
	params.sqThreadIdle = sqPollIdle

	if cpuID >= 0 && flags&setupSQPoll != 0 {
		params.flags |= setupSQAff
		params.sqThreadCPU = uint32(cpuID)
	}

	fd, _, errno := unix.Syscall(
		uintptr(sysIOUringSetup),
		uintptr(entries),
		uintptr(unsafe.Pointer(&params)),
		0,
	)
	if errno != 0 {
		// ENOMEM during io_uring_setup is almost always RLIMIT_MEMLOCK,
		// not actual OOM: each ring locks ~4-12MB of pages, and the
		// default per-process limit is 8MB, so any worker count > 1
		// trips it. Surface the diagnosis so users don't waste time
		// chasing a system-memory red herring.
		if errno == unix.ENOMEM {
			var rlim unix.Rlimit
			limit := "unknown"
			if e := unix.Getrlimit(unix.RLIMIT_MEMLOCK, &rlim); e == nil {
				limit = fmt.Sprintf("%d bytes (soft) / %d bytes (hard)", rlim.Cur, rlim.Max)
			}
			return nil, fmt.Errorf("io_uring_setup: %w "+
				"(likely RLIMIT_MEMLOCK; current=%s; "+
				"raise via `ulimit -l unlimited`, "+
				"systemd LimitMEMLOCK=infinity, or run as root)",
				errno, limit)
		}
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}

	r := &Ring{
		fd:           int(fd),
		params:       params,
		singleIssuer: flags&setupSingleIssuer != 0,
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
	r.sqFlagsPtr = unsafe.Pointer(&r.sqRing[r.params.sqOff.flags])
	r.cqHead = unsafe.Pointer(&r.cqRing[r.params.cqOff.head])
	r.cqTail = unsafe.Pointer(&r.cqRing[r.params.cqOff.tail])
	r.cqesBase = unsafe.Pointer(&r.cqRing[r.params.cqOff.cqes])

	return nil
}

// GetSQE returns a pointer to the next available SQE, or nil if the ring is full.
func (r *Ring) GetSQE() unsafe.Pointer {
	var tail, head uint32
	if r.singleIssuer {
		tail = *(*uint32)(r.sqTail)
		head = *(*uint32)(r.sqHead)
	} else {
		tail = atomic.LoadUint32((*uint32)(r.sqTail))
		head = atomic.LoadUint32((*uint32)(r.sqHead))
	}
	if tail-head >= r.params.sqEntries {
		return nil
	}
	idx := tail & r.sqMask
	arrayPtr := (*uint32)(unsafe.Add(r.sqArray, uintptr(idx)*4))
	*arrayPtr = idx
	sqePtr := unsafe.Add(unsafe.Pointer(&r.sqes[0]), uintptr(idx)*sqeSize)
	*(*[48]byte)(sqePtr) = [48]byte{}
	if r.singleIssuer {
		*(*uint32)(r.sqTail) = tail + 1
	} else {
		atomic.StoreUint32((*uint32)(r.sqTail), tail+1)
	}
	r.pending++
	// validateSQEWrite is a no-op in production (see
	// validation_default.go); under -tags=validation it asserts
	// monotonic tail across all GetSQE callers and increments
	// validation.IouringSQECorruptions on violation.
	validateSQEWrite(r, tail+1)
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

// Pending returns the number of SQEs submitted but not yet sent to the kernel.
func (r *Ring) Pending() uint32 { return r.pending }

// ClearPending resets the pending counter without issuing a syscall. Used with
// SQPOLL where the kernel thread submits SQEs automatically.
func (r *Ring) ClearPending() { r.pending = 0 }

// BeginCQ returns the current CQ head and tail for batch processing.
// Under SINGLE_ISSUER, cqHead is a plain load (we own it).
func (r *Ring) BeginCQ() (head, tail uint32) {
	if r.singleIssuer {
		head = *(*uint32)(r.cqHead)
	} else {
		head = atomic.LoadUint32((*uint32)(r.cqHead))
	}
	tail = atomic.LoadUint32((*uint32)(r.cqTail))
	return
}

// cqeAt returns the CQE at the given head position.
func (r *Ring) cqeAt(head uint32) *completionEntry {
	idx := head & r.cqMask
	return (*completionEntry)(unsafe.Add(r.cqesBase, uintptr(idx)*cqeSize))
}

// EndCQ advances the CQ head to newHead with a single atomic store.
func (r *Ring) EndCQ(newHead uint32) {
	atomic.StoreUint32((*uint32)(r.cqHead), newHead)
}

// SQNeedWakeup returns true if the SQPOLL kernel thread went idle and needs
// a wakeup via WakeupSQPoll() to resume submitting SQEs.
func (r *Ring) SQNeedWakeup() bool {
	return atomic.LoadUint32((*uint32)(r.sqFlagsPtr))&sqNeedWakeup != 0
}

// WaitCQETimeout waits for at least one CQE without submitting any SQEs.
// Used by SQPOLL workers where the kernel thread handles submission.
// Returns nil on timeout (ETIME) or EINTR.
func (r *Ring) WaitCQETimeout(timeout time.Duration) error {
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
		0,
		1,
		uintptr(enterGetEvents|enterExtArg),
		uintptr(unsafe.Pointer(&arg)),
		unsafe.Sizeof(arg),
	)
	if errno != 0 {
		if errno == unix.ETIME || errno == unix.EINTR {
			return nil
		}
		return fmt.Errorf("io_uring_enter wait cqe timeout: %w", errno)
	}
	return nil
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

// RegisterFiles pre-registers a fixed file table with the kernel. Each entry
// is initialized to -1 (empty). Use UpdateFixedFile to install FDs into slots.
func (r *Ring) RegisterFiles(count int) error {
	fds := make([]int32, count)
	for i := range fds {
		fds[i] = -1
	}
	_, _, errno := unix.Syscall6(
		uintptr(sysIOUringRegister),
		uintptr(r.fd),
		uintptr(registerFiles),
		uintptr(unsafe.Pointer(&fds[0])),
		uintptr(count),
		0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("io_uring_register files: %w", errno)
	}
	return nil
}

// filesUpdate mirrors struct io_uring_files_update.
type filesUpdate struct {
	Offset uint32
	Resv   uint32
	FDs    uint64
}

// UpdateFixedFile installs or removes a file descriptor in a fixed file slot.
// Set fd to -1 to clear a slot.
func (r *Ring) UpdateFixedFile(slot int, fd int) error {
	fds := [1]int32{int32(fd)}
	arg := filesUpdate{
		Offset: uint32(slot),
		FDs:    uint64(uintptr(unsafe.Pointer(&fds[0]))),
	}
	_, _, errno := unix.Syscall6(
		uintptr(sysIOUringRegister),
		uintptr(r.fd),
		uintptr(registerFilesUpdate),
		uintptr(unsafe.Pointer(&arg)),
		1,
		0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("io_uring_register files_update slot %d: %w", slot, errno)
	}
	return nil
}

// pbufRingSetup mirrors struct io_uring_buf_reg for IORING_REGISTER_PBUF_RING.
type pbufRingSetup struct {
	RingAddr    uint64
	RingEntries uint32
	Bgid        uint16
	Pad         uint16
	Resv        [3]uint64
}

// RegisterPbufRing registers a ring-mapped provided buffer ring with the kernel.
// The ring memory is allocated by the caller via mmap and passed in ringAddr.
// The caller is responsible for munmapping the ring memory after unregistering.
func (r *Ring) RegisterPbufRing(groupID uint16, entries uint32, ringAddr unsafe.Pointer) error {
	arg := pbufRingSetup{
		RingAddr:    uint64(uintptr(ringAddr)),
		RingEntries: entries,
		Bgid:        groupID,
	}
	_, _, errno := unix.Syscall6(
		uintptr(sysIOUringRegister),
		uintptr(r.fd),
		uintptr(registerPbufRing),
		uintptr(unsafe.Pointer(&arg)),
		1,
		0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("io_uring_register pbuf_ring: %w", errno)
	}
	return nil
}

// UnregisterPbufRing unregisters a ring-mapped provided buffer ring.
func (r *Ring) UnregisterPbufRing(groupID uint16) error {
	arg := pbufRingSetup{
		Bgid: groupID,
	}
	_, _, errno := unix.Syscall6(
		uintptr(sysIOUringRegister),
		uintptr(r.fd),
		uintptr(unregisterPbufRing),
		uintptr(unsafe.Pointer(&arg)),
		1,
		0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("io_uring_unregister pbuf_ring: %w", errno)
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
