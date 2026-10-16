//go:build linux

package session

import "testing"

// ioUringUsable reports whether io_uring works on this host: the probe tier is
// High or better and provided buffers are present. It is the test's criterion
// for failing rather than skipping an io_uring start (startLoginServer), and
// it is c731ProbeIOUring, so with CELERIS_REQUIRE_IOURING_WORKERS=1 it retries
// the probe for 10 s like the sse package's (c714ProbeIOUring), logging each
// round.
func ioUringUsable(tb testing.TB) bool {
	usable, _ := c731ProbeIOUring(tb)
	return usable
}
