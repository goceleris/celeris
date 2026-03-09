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
	setupCoopTaskrun  = 1 << 8
	setupSingleIssuer = 1 << 12
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
	opACCEPT         = 13
	opRECV           = 27
	opSEND           = 26
	opCLOSE          = 19
	opPROVIDEBUFFERS = 31
)

// SQE flags.
const (
	sqeIOLink       = 1 << 2
	sqeBufferSelect = 1 << 5
)

// CQE flags.
const (
	cqeFBuffer = 1 << 0
	cqeFMore   = 1 << 1
)

// Accept flags.
const (
	acceptMultishot = 1 << 0
)

// Recv flags.
const (
	recvMultishot = 1 << 1
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
