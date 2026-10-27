//go:build linux

package iouring

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// chargeMemlock makes an engine start fail with the ring-setup ENOMEM for d,
// the way a CI runner does when the rings of the engines before it are closed
// but not yet uncharged (ring_budget_linux_test.go): it sets this process's
// soft RLIMIT_MEMLOCK to 8 MiB, GitHub's default, and creates rings until the
// limit refuses one, then closes them after d (or at cleanup). It is the
// deterministic form of the flake celeris#1104 lists, for the tests of the
// starters that must ride it out. The locked-memory count is per user, so a
// test that charges must not run beside one that starts a ring (none here is
// parallel).
func chargeMemlock(t *testing.T, d time.Duration) {
	t.Helper()
	var old unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_MEMLOCK, &old); err != nil {
		t.Skipf("getrlimit(RLIMIT_MEMLOCK): %v", err)
	}
	limit := unix.Rlimit{Cur: 8 << 20, Max: old.Max}
	if old.Max < limit.Cur {
		limit.Cur = old.Max
	}
	if err := unix.Setrlimit(unix.RLIMIT_MEMLOCK, &limit); err != nil {
		t.Skipf("setrlimit(RLIMIT_MEMLOCK): %v", err)
	}
	// Fill the budget with large rings first, then with ever smaller ones until
	// a 4-entry ring is refused too: the capability probe's own private ring is
	// small, and when it fails New answers "io_uring not available on this
	// system" instead of the ring-setup ENOMEM (see isRingStartPressure). The
	// closed rings of an earlier run are uncharged within ~25 ms and give the
	// budget back while this fills, so it fills again until two passes 40 ms
	// apart find nothing to take.
	var held []*Ring
	fill := func() (added int) {
		for _, entries := range []uint32{8192, 1024, 128, 16, 4} {
			for len(held) < 512 {
				r, err := NewRing(entries, 0, 0)
				if err != nil {
					break
				}
				held = append(held, r)
				added++
			}
		}
		return added
	}
	for idle, deadline := 0, time.Now().Add(3*time.Second); idle < 2 && time.Now().Before(deadline); time.Sleep(40 * time.Millisecond) {
		if fill() == 0 {
			idle++
		} else {
			idle = 0
		}
	}
	if len(held) == 0 || len(held) >= 512 {
		for _, r := range held {
			_ = r.Close()
		}
		_ = unix.Setrlimit(unix.RLIMIT_MEMLOCK, &old)
		t.Skipf("could not fill the memlock budget (%d rings held)", len(held))
	}
	done := make(chan struct{})
	free := func() {
		select {
		case <-done:
			return
		default:
		}
		close(done)
		for _, r := range held {
			_ = r.Close()
		}
		_ = unix.Setrlimit(unix.RLIMIT_MEMLOCK, &old)
	}
	timer := time.AfterFunc(d, free)
	t.Cleanup(func() { timer.Stop(); free() })
}
