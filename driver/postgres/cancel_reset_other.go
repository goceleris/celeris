//go:build !windows

package postgres

import (
	"errors"
	"syscall"
)

// isConnReset reports whether err is the peer resetting the connection.
func isConnReset(err error) bool { return errors.Is(err, syscall.ECONNRESET) }
