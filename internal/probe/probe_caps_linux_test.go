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

// TestCheckCapSysNiceIsSideEffectFree guards against a regression of the
// dangerous Setpriority-based probe (finding 5.2): the real read-only
// implementation must not change the process scheduling priority. We
// snapshot the current nice value, run the check, and assert it is
// unchanged. The boolean result itself is environment-dependent and not
// asserted. Linux-only (lives here with getNice so the cross-platform
// probe_test.go compiles on darwin/non-linux).
func TestCheckCapSysNiceIsSideEffectFree(t *testing.T) {
	sp := defaultProber()
	if sp.CheckCapSysNice == nil {
		t.Skip("no CheckCapSysNice on this platform")
	}
	before, err := getNice(t)
	if err != nil {
		t.Skipf("cannot read nice value: %v", err)
	}
	_ = sp.CheckCapSysNice() // result is environment-dependent; we test for side effects
	after, err := getNice(t)
	if err != nil {
		t.Fatalf("cannot read nice value after check: %v", err)
	}
	if before != after {
		t.Fatalf("CheckCapSysNice mutated process nice: before=%d after=%d", before, after)
	}
}
