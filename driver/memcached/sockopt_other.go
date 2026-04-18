//go:build !linux

package memcached

// tuneConnSocket is a no-op on non-Linux platforms. TCP_QUICKACK and
// SO_BUSY_POLL are Linux-specific.
func tuneConnSocket(_ int) {}
