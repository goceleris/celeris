//go:build linux

package iouring

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

func prepAccept(sqePtr unsafe.Pointer, listenFD int, flags uint32) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[0] = opACCEPT
	*(*int32)(unsafe.Pointer(&sqe[4])) = int32(listenFD)
	*(*uint32)(unsafe.Pointer(&sqe[28])) = flags // accept_flags at offset 28
	*(*uint64)(unsafe.Pointer(&sqe[8])) = 0
	*(*uint64)(unsafe.Pointer(&sqe[16])) = 0
	*(*uint32)(unsafe.Pointer(&sqe[24])) = 0
}

func prepMultishotAccept(sqePtr unsafe.Pointer, listenFD int) {
	prepAccept(sqePtr, listenFD, uint32(unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC))
	sqe := (*[sqeSize]byte)(sqePtr)
	*(*uint16)(unsafe.Pointer(&sqe[2])) = acceptMultishot // ioprio at offset 2
}

func prepRecv(sqePtr unsafe.Pointer, fd int, buf []byte) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[0] = opRECV
	*(*int32)(unsafe.Pointer(&sqe[4])) = int32(fd)
	if len(buf) > 0 {
		*(*uint64)(unsafe.Pointer(&sqe[16])) = uint64(uintptr(unsafe.Pointer(&buf[0])))
		*(*uint32)(unsafe.Pointer(&sqe[24])) = uint32(len(buf))
	}
}

func prepSend(sqePtr unsafe.Pointer, fd int, buf []byte, linked bool) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[0] = opSEND
	if linked {
		sqe[1] = sqeIOLink
	}
	*(*int32)(unsafe.Pointer(&sqe[4])) = int32(fd)
	if len(buf) > 0 {
		*(*uint64)(unsafe.Pointer(&sqe[16])) = uint64(uintptr(unsafe.Pointer(&buf[0])))
		*(*uint32)(unsafe.Pointer(&sqe[24])) = uint32(len(buf))
	}
}

func prepClose(sqePtr unsafe.Pointer, fd int) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[0] = opCLOSE
	*(*int32)(unsafe.Pointer(&sqe[4])) = int32(fd)
}

// prepCancelFD cancels all pending io_uring operations on a file descriptor.
// This releases the kernel's io_uring reference to the underlying file,
// allowing listen sockets to leave the SO_REUSEPORT group immediately.
func prepCancelFD(sqePtr unsafe.Pointer, fd int) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[0] = opASYNCCANCEL
	*(*int32)(unsafe.Pointer(&sqe[4])) = int32(fd)
	*(*uint32)(unsafe.Pointer(&sqe[28])) = cancelFD | cancelAll
}

func prepProvideBuffers(sqePtr unsafe.Pointer, addr unsafe.Pointer, bufLen int, count int, groupID uint16, bufID uint16) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[0] = opPROVIDEBUFFERS
	*(*uint64)(unsafe.Pointer(&sqe[16])) = uint64(uintptr(addr))
	*(*uint32)(unsafe.Pointer(&sqe[24])) = uint32(bufLen)
	*(*int32)(unsafe.Pointer(&sqe[4])) = int32(count)
	*(*uint16)(unsafe.Pointer(&sqe[30])) = groupID
	*(*uint16)(unsafe.Pointer(&sqe[28])) = bufID
}

func setSQEUserData(sqePtr unsafe.Pointer, data uint64) {
	*(*uint64)(unsafe.Pointer(uintptr(sqePtr) + 32)) = data
}
