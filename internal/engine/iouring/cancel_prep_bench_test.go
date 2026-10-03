//go:build linux

package iouring

import (
	"testing"
	"unsafe"
)

// BenchmarkPrepCancel records the cost of the two ASYNC_CANCEL SQE encodings.
// celeris#482 switched the WebSocket recv-pause path from the fd-keyed form
// (CANCEL_FD|CANCEL_ALL, which matches every op on the socket) to the
// user_data-keyed form (matches only the armed recv). Both are a handful of
// stores into a 64-byte SQE; this pins that the change is not a hot-path
// cost.
func BenchmarkPrepCancel(b *testing.B) {
	var sqe [sqeSize]byte
	ud := encodeUserDataGen(udRecv, 42, 7)
	b.Run("fd-keyed(old)", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			prepCancelFDSkipSuccess(unsafe.Pointer(&sqe[0]), 42)
		}
	})
	b.Run("user_data-keyed(new)", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			prepCancelUserDataSkipSuccess(unsafe.Pointer(&sqe[0]), ud)
		}
	})
}
