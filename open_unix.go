//go:build !windows

package celeris

import (
	"os"
	"syscall"
)

// openNonblock opens path read-only with O_NONBLOCK: for a regular file it
// changes nothing, and open(2) of a FIFO returns at once instead of waiting for
// a writer.
func openNonblock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
