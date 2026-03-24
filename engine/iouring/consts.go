//go:build linux

package iouring

// io_uring syscall numbers.
const (
	sysIOUringSetup    = 425
	sysIOUringEnter    = 426
	sysIOUringRegister = 427
)

// io_uring setup flags.
const (
	setupSQPoll       = 1 << 1
	setupSQAff        = 1 << 2 // pin SQPOLL thread to sq_thread_cpu
	setupCoopTaskrun  = 1 << 8
	setupSingleIssuer = 1 << 12
	setupDeferTaskrun = 1 << 13
)

// io_uring enter flags.
const (
	enterGetEvents = 1 << 0
	enterSQWakeup  = 1 << 1
	enterExtArg    = 1 << 3
)

// io_uring opcodes.
const (
	opNOP            = 0
	opREADV          = 1
	opWRITEV         = 2
	opPOLLADD        = 6 // IORING_OP_POLL_ADD
	opACCEPT         = 13
	opASYNCCANCEL    = 14
	opCLOSE          = 19
	opSEND           = 26
	opRECV           = 27
	opPROVIDEBUFFERS = 31
	opSHUTDOWN       = 52 // IORING_OP_SHUTDOWN (kernel 5.11+)
	opSENDZC         = 53 // IORING_OP_SEND_ZC (kernel 6.0+)
)

// SQE flags.
const (
	sqeFixedFile      = 1 << 0
	sqeIOLink         = 1 << 2
	sqeBufferSelect   = 1 << 5
	sqeCQESkipSuccess = 1 << 6
)

// CQE flags.
const (
	cqeFBuffer = 1 << 0
	cqeFMore   = 1 << 1
	cqeFNotif  = 1 << 2 // IORING_CQE_F_NOTIF: zero-copy send notification
)

// Accept flags.
const (
	acceptMultishot = 1 << 0
)

// Recv flags (ioprio field).
const (
	recvMultishot = 1 << 1
)

// io_uring_register opcodes.
const (
	registerFiles       = 2
	registerFilesUpdate = 6
	registerPbufRing    = 22
	unregisterPbufRing  = 23
)

// Fixed file index sentinel for auto-allocation (IORING_FILE_INDEX_ALLOC).
const fileIndexAlloc = ^uint32(0)

// SQ ring flags (read from shared memory).
const (
	sqNeedWakeup = 1 << 0 // IORING_SQ_NEED_WAKEUP
)

// Async cancel flags.
const (
	cancelFD  = 1 << 1 // IORING_ASYNC_CANCEL_FD
	cancelAll = 1 << 0 // IORING_ASYNC_CANCEL_ALL
)

// Mmap offsets for ring buffers.
const (
	offSQRing = 0
	offCQRing = 0x8000000
	offSQEs   = 0x10000000
)

// SQE and CQE sizes.
const (
	sqeSize = 64
	cqeSize = 16
)
