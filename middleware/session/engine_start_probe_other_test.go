//go:build !linux

package session

import "testing"

// ioUringExists is false off Linux: io_uring does not exist there, so an
// io_uring start failure skips, as it did before (startLoginServer).
func ioUringExists(testing.TB) bool { return false }
