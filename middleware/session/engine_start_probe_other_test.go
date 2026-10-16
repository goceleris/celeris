//go:build !linux

package session

// ioUringUsable is false off Linux: io_uring does not exist there, so an
// io_uring start failure skips, as it did before (startLoginServer).
func ioUringUsable() bool { return false }
