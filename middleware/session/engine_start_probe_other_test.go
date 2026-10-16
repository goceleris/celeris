//go:build !linux

package session

import "testing"

// ioUringUsable is false off Linux: io_uring does not exist there, so an
// io_uring start failure skips, as it did before (startLoginServer).
func ioUringUsable(testing.TB) bool { return false }
