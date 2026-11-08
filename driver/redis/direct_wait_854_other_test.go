//go:build !linux

package redis

import (
	"testing"
	"time"
)

// TestDirectWaitNeverBlocksOffLinux854 is the non-linux half of celeris#854:
// where the platform has no poll(2) wait, even mode "block" parks in netpoll
// and never counts a block or a handoff. (The netpoll path counts no parks, so
// "did not block" is blocks == 0 and handoffs == 0.) It passes before and after the behaviour change,
// by design; the blocking tests live in the _linux_test files.
func TestDirectWaitNeverBlocksOffLinux854(t *testing.T) {
	if directBlockSupported {
		t.Fatal("directBlockSupported is true off linux")
	}
	f := startWaitFake(t)
	f.setDelay(2 * time.Millisecond)
	cl := newWaitClient(t, f.addr(), 1)
	setDirectWaitForTest(t, directWaitBlock)
	execN(t, cl, 3, 10) // warm
	before := directWaitSnapshot()
	execN(t, cl, 10, 10)
	d := directWaitSnapshot().since(before)
	if d.Blocks != 0 || d.SliceExpiries != 0 || d.Handoffs != 0 {
		t.Fatalf("blocked off linux: %+v", d)
	}
}
