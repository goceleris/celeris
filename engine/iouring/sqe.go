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

// prepMultishotAcceptDirect prepares a multishot accept that installs accepted
// FDs directly into the io_uring fixed file table. The CQE result is a fixed
// file index, not a regular FD.
func prepMultishotAcceptDirect(sqePtr unsafe.Pointer, listenFD int) {
	prepMultishotAccept(sqePtr, listenFD)
	sqe := (*[sqeSize]byte)(sqePtr)
	// file_index at offset 44: IORING_FILE_INDEX_ALLOC for auto-allocation.
	*(*uint32)(unsafe.Pointer(&sqe[44])) = fileIndexAlloc
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

// prepMultishotRecv prepares a multishot recv that uses provided buffers from
// the buffer ring identified by groupID. The kernel selects a buffer per
// completion; the buffer ID is returned in the CQE flags.
func prepMultishotRecv(sqePtr unsafe.Pointer, fd int, groupID uint16, fixedFile bool) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[0] = opRECV
	flags := uint8(sqeBufferSelect)
	if fixedFile {
		flags |= sqeFixedFile
	}
	sqe[1] = flags
	*(*uint16)(unsafe.Pointer(&sqe[2])) = recvMultishot // ioprio: multishot flag
	*(*int32)(unsafe.Pointer(&sqe[4])) = int32(fd)
	*(*uint64)(unsafe.Pointer(&sqe[16])) = 0 // addr: unused (provided buffers)
	*(*uint32)(unsafe.Pointer(&sqe[24])) = 0 // len: unused (provided buffers)
	// buf_group at offset 40
	*(*uint16)(unsafe.Pointer(&sqe[40])) = groupID
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
	sqe[1] = sqeCQESkipSuccess // suppress CQE for successful close
	*(*int32)(unsafe.Pointer(&sqe[4])) = int32(fd)
}

// prepCloseDirect closes a fixed file entry by index. The fd field is unused
// (set to 0); file_index identifies the slot.
func prepCloseDirect(sqePtr unsafe.Pointer, fileIndex int) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[0] = opCLOSE
	sqe[1] = sqeFixedFile | sqeCQESkipSuccess
	*(*int32)(unsafe.Pointer(&sqe[4])) = 0 // unused for direct close
	// file_index at offset 44 (0-based index + 1 for direct)
	*(*uint32)(unsafe.Pointer(&sqe[44])) = uint32(fileIndex)
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

// setSQEFixedFile sets the IOSQE_FIXED_FILE flag on an SQE after initial prep.
func setSQEFixedFile(sqePtr unsafe.Pointer) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[1] |= sqeFixedFile
}

// prepSendPlain prepares a SEND SQE for a regular (non-fixed) file descriptor.
func prepSendPlain(sqePtr unsafe.Pointer, fd int, buf []byte, linked bool) {
	prepSend(sqePtr, fd, buf, linked)
}

// prepSendFixed prepares a SEND SQE for a fixed file descriptor.
func prepSendFixed(sqePtr unsafe.Pointer, fd int, buf []byte, linked bool) {
	prepSend(sqePtr, fd, buf, linked)
	setSQEFixedFile(sqePtr)
}

// prepWritev prepares a WRITEV SQE for scatter-gather transmission. The
// kernel reads from iov entries in order and issues a single writev(2)-
// equivalent syscall. Used on TCP sockets so that a large response (a
// small header block + a large body) can be sent in ONE syscall without
// first memcpying the body into the engine's outbound buffer. Saves one
// full body-sized memcpy per request on json-64k-style responses.
//
// iov must remain valid until the corresponding CQE fires. The iovec
// Go struct is defined in golang.org/x/sys/unix; we pass a pointer to
// the first iovec and the count. The kernel copies the iovec array into
// its own state before returning from io_uring_enter, but the BUFFERS
// pointed to by the iovecs must stay alive until completion.
func prepWritev(sqePtr unsafe.Pointer, fd int, iovPtr unsafe.Pointer, iovLen int, linked bool) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[0] = opWRITEV
	if linked {
		sqe[1] = sqeIOLink
	}
	*(*int32)(unsafe.Pointer(&sqe[4])) = int32(fd)
	*(*uint64)(unsafe.Pointer(&sqe[16])) = uint64(uintptr(iovPtr))
	*(*uint32)(unsafe.Pointer(&sqe[24])) = uint32(iovLen)
}

// prepSendZC prepares a SEND_ZC (zero-copy send) SQE. The kernel avoids
// copying the buffer and instead maps it for DMA. Two CQEs are produced:
// the first with the send result, and a notification CQE (CQE_F_NOTIF)
// when the buffer can be safely reused.
func prepSendZC(sqePtr unsafe.Pointer, fd int, buf []byte, linked bool) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[0] = opSENDZC
	if linked {
		sqe[1] = sqeIOLink
	}
	*(*int32)(unsafe.Pointer(&sqe[4])) = int32(fd)
	if len(buf) > 0 {
		*(*uint64)(unsafe.Pointer(&sqe[16])) = uint64(uintptr(unsafe.Pointer(&buf[0])))
		*(*uint32)(unsafe.Pointer(&sqe[24])) = uint32(len(buf))
	}
}

// prepSendZCFixed prepares a SEND_ZC SQE for a fixed file descriptor.
func prepSendZCFixed(sqePtr unsafe.Pointer, fd int, buf []byte, linked bool) {
	prepSendZC(sqePtr, fd, buf, linked)
	setSQEFixedFile(sqePtr)
}

// prepPollAdd prepares a POLL_ADD SQE. The poll event mask goes in the
// poll32_events field (offset 28). Used for eventfd wakeup monitoring.
func prepPollAdd(sqePtr unsafe.Pointer, fd int, pollMask uint32) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[0] = opPOLLADD
	*(*int32)(unsafe.Pointer(&sqe[4])) = int32(fd)
	*(*uint32)(unsafe.Pointer(&sqe[28])) = pollMask // poll32_events at offset 28
}

// prepCancelFDSkipSuccess prepares an ASYNC_CANCEL SQE with CQE_SKIP_SUCCESS
// to suppress the success CQE (P11).
func prepCancelFDSkipSuccess(sqePtr unsafe.Pointer, fd int) {
	sqe := (*[sqeSize]byte)(sqePtr)
	sqe[0] = opASYNCCANCEL
	sqe[1] = sqeCQESkipSuccess
	*(*int32)(unsafe.Pointer(&sqe[4])) = int32(fd)
	*(*uint32)(unsafe.Pointer(&sqe[28])) = cancelFD | cancelAll
}
