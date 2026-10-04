//go:build linux

package iouring

import (
	"testing"
	"unsafe"
)

// TestPrepCancelUserDataSkipSuccess verifies the fixed-file-safe cancel
// encoding (v1.5.0 review 2.5): the target user_data must land in the SQE addr
// field (offset 16), the opcode must be ASYNC_CANCEL, CQE_SKIP_SUCCESS must be
// set, and the cancel_flags (offset 28) must NOT carry the cancelFD bit (so the
// kernel matches by user_data, not by the fixed-file index sitting in the fd
// field).
func TestPrepCancelUserDataSkipSuccess(t *testing.T) {
	var sqe [sqeSize]byte
	target := encodeUserData(udRecv, 12345)
	prepCancelUserDataSkipSuccess(unsafe.Pointer(&sqe[0]), target)

	if sqe[0] != opASYNCCANCEL {
		t.Errorf("opcode = %d, want opASYNCCANCEL(%d)", sqe[0], opASYNCCANCEL)
	}
	if sqe[1]&sqeCQESkipSuccess == 0 {
		t.Errorf("CQE_SKIP_SUCCESS not set in flags byte 0x%02x", sqe[1])
	}
	gotAddr := *(*uint64)(unsafe.Pointer(&sqe[16]))
	if gotAddr != target {
		t.Errorf("addr (user_data match) = 0x%x, want 0x%x", gotAddr, target)
	}
	flags := *(*uint32)(unsafe.Pointer(&sqe[28]))
	if flags&cancelFD != 0 {
		t.Errorf("cancel_flags 0x%x has cancelFD set — would match by fd, not user_data", flags)
	}
	if flags&cancelAll == 0 {
		t.Errorf("cancel_flags 0x%x missing cancelAll", flags)
	}
}

// TestPrepCancelFDStillMatchesByFD is a guard that the plain (non-fixed-file)
// cancel path still matches by fd, so the two paths remain distinct.
func TestPrepCancelFDStillMatchesByFD(t *testing.T) {
	var sqe [sqeSize]byte
	prepCancelFDSkipSuccess(unsafe.Pointer(&sqe[0]), 9)
	if sqe[0] != opASYNCCANCEL {
		t.Errorf("opcode = %d, want opASYNCCANCEL", sqe[0])
	}
	if gotFD := *(*int32)(unsafe.Pointer(&sqe[4])); gotFD != 9 {
		t.Errorf("fd field = %d, want 9", gotFD)
	}
	flags := *(*uint32)(unsafe.Pointer(&sqe[28]))
	if flags&cancelFD == 0 {
		t.Errorf("cancel_flags 0x%x missing cancelFD on the by-fd path", flags)
	}
}
