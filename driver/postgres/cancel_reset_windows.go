//go:build windows

package postgres

import (
	"errors"
	"syscall"
)

// isConnReset reports whether err is the peer resetting the connection. On
// Windows that is WSAECONNRESET, which syscall.ECONNRESET (an invented value)
// does not match.
func isConnReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.WSAECONNRESET)
}
