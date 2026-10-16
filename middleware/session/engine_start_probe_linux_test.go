//go:build linux

package session

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ioUringExists reports whether io_uring exists on this host: a raw
// io_uring_setup answers ENOMEM (it exists, and is short of memory), or the
// probe finds a tier of High or better with provided buffers (it works). The
// probe is c731ProbeIOUring, so with CELERIS_REQUIRE_IOURING_WORKERS=1 it
// retries for 10 s like the sse package's (c714ProbeIOUring), logging each
// round.
func ioUringExists(tb testing.TB) bool {
	if ioUringSetupErrno() == unix.ENOMEM {
		return true
	}
	usable, _ := c731ProbeIOUring(tb)
	return usable
}

// ioUringSetupErrno makes one io_uring_setup call and returns its errno, 0 when
// it worked (the ring is closed again).
func ioUringSetupErrno() unix.Errno {
	var params [120]byte // sizeof(struct io_uring_params)
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, 4, uintptr(unsafe.Pointer(&params[0])), 0)
	if errno == 0 {
		_ = unix.Close(int(fd))
	}
	return errno
}
