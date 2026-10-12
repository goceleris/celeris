//go:build unix

package postgres

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// assertNumberReleased fails unless Close has closed c's descriptor: F_GETFD
// on it returns EBADF. F_GETFD only looks; a second close(2) here would close
// whatever the process has opened on the number since (celeris#859).
func assertNumberReleased(t *testing.T, c *pgConn) {
	t.Helper()
	_, err := unix.FcntlInt(uintptr(c.fd), unix.F_GETFD, 0)
	if !errors.Is(err, unix.EBADF) {
		t.Fatalf("expected EBADF after Close(), got %v", err)
	}
}
