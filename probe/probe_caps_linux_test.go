//go:build linux

package probe

import (
	"testing"

	"golang.org/x/sys/unix"
)

// getNice reads the current process scheduling priority (nice value) so
// TestCheckCapSysNiceIsSideEffectFree can assert the capability probe
// does not mutate it. Linux-only: golang.org/x/sys/unix is imported only
// under the linux build tag here.
func getNice(t *testing.T) (int, error) {
	t.Helper()
	return unix.Getpriority(unix.PRIO_PROCESS, 0)
}
