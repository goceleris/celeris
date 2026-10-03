//go:build linux && (amd64 || arm64)

package iouring

import (
	"testing"
	"unsafe"
)

// TestClosedOpsEntryStaysThirtyTwoBytes pins where celeris#657 put its
// handoff flag: in the padding after inflight. Without the flag, conns sits
// at offset 8 (inflight's 4 bytes plus 4 of padding) and an entry is 32
// bytes on 64-bit platforms; with it, both must stay the same. 32-bit
// platforms have no padding there (an int32 and a 12-byte slice header make
// 16 bytes), so the entry grows from 16 to 20 bytes. The build tag keeps
// this test to the two platforms CI and the benchmark cluster run.
func TestClosedOpsEntryStaysThirtyTwoBytes(t *testing.T) {
	var e closedOpsEntry
	size, flagAt, connsAt := unsafe.Sizeof(e), unsafe.Offsetof(e.handoff), unsafe.Offsetof(e.conns)
	if size != 32 || flagAt != unsafe.Sizeof(e.inflight) || connsAt != 8 {
		t.Errorf("closedOpsEntry is %d bytes with handoff at offset %d and conns at %d, want 32 bytes, "+
			"handoff at %d and conns at 8: the celeris#657 flag has to sit in inflight's padding",
			size, flagAt, connsAt, unsafe.Sizeof(e.inflight))
	}
}
