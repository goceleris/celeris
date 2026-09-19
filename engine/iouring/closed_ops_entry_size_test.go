//go:build linux && (amd64 || arm64)

package iouring

import (
	"testing"
	"unsafe"
)

// TestClosedOpsEntryStaysThirtyTwoBytes pins where celeris#657 put its
// handoff flag: in the padding after inflight. On 64-bit platforms an entry
// is then the same 32 bytes it was without the flag. 32-bit platforms have
// no padding there (an int32 and a 12-byte slice header make 16 bytes), so
// the entry grows from 16 to 20 bytes. The build tag keeps this test to the
// two platforms CI and the benchmark cluster run.
func TestClosedOpsEntryStaysThirtyTwoBytes(t *testing.T) {
	type withoutFlag struct {
		inflight int32
		conns    []*connState
	}
	got, before := unsafe.Sizeof(closedOpsEntry{}), unsafe.Sizeof(withoutFlag{})
	if got != 32 || before != 32 {
		t.Errorf("closedOpsEntry is %d bytes and the entry without the celeris#657 flag is %d, "+
			"want 32 for both: the flag has to sit in inflight's padding", got, before)
	}
}
