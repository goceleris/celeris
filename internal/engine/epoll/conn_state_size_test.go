//go:build linux

package epoll

import (
	"testing"
	"unsafe"
)

// TestConnStateStaysInItsSizeClass: connState is allocated one per conn (a
// pool, then the Go allocator), and 448 bytes is a Go size class; one more
// byte makes it 480, 7 percent more memory per conn and a sweep over 4096
// conns that measured 7 to 15 percent slower (BenchmarkCheckTimeouts865, the
// field detectDeadline added to a struct that had padding to spare). A field
// that does not fit is placed into an existing hole, or this bound is raised
// on purpose, with a measurement.
func TestConnStateStaysInItsSizeClass(t *testing.T) {
	const class = 448
	if sz := unsafe.Sizeof(connState{}); sz > class {
		t.Fatalf("connState is %d bytes, over its %d-byte size class: move the new field into a padding hole (see the flags beside peerClosed)", sz, class)
	}
}
