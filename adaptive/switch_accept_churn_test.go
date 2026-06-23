//go:build linux

package adaptive

import (
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// procSelfFDCount counts open file descriptors for the current process by
// reading /proc/self/fd (linux). It is the phantom-socket-leak probe: a switch
// that closes a listen FD while accepts are in flight could orphan a socket,
// which would show up as a growing FD count that never drains.
func procSelfFDCount(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	// ReadDir itself opens a directory FD that is counted in the listing;
	// subtracting it is unnecessary for a baseline-vs-after delta since both
	// measurements pay the same cost.
	return len(ents)
}

// TestAdaptiveSwitchVsAcceptChurn is the COMPLEMENTARY stress to
// TestAdaptiveConcurrentDriverChurnVsSwitch: that test churns DRIVER FDs vs
// ForceSwitch; this one churns real HTTP connection accept/close against the
// listen port WHILE switches fire, exercising the in-flight-accept-vs-listen-
// fd-close window (the phantom-socket condition the SO_REUSEPORT switch must
// not leak through).
//
// Invariants:
//   - Process FD count does not grow beyond a slack over baseline after the
//     storm quiesces (no phantom-socket leak). The io_uring async-close queue
//     drains asynchronously, so we poll the count down before asserting.
//   - The engine still ACCEPTS after the storm (one final dial succeeds).
//   - SwitchRejectedCount / AdaptiveSwitches are observable (logged).
func TestAdaptiveSwitchVsAcceptChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short mode")
	}
	e, stop := newBoundAdaptive(t)
	defer stop()

	addr := e.Addr().String()

	// Warm up the lazy io_uring standby BEFORE measuring baseline: New() builds
	// the standby only on the first switch, so a switch permanently adds the
	// io_uring engine's fixed FDs (ring + eventfds + per-worker FDs). Counting
	// those as a "leak" would false-fail, so force the build now (and switch
	// back) so both sub-engines' fixed FDs are already in the baseline; after
	// this only orphaned per-connection sockets can grow the count.
	e.ForceSwitch() // epoll -> io_uring (builds + Listens the standby)
	e.ForceSwitch() // io_uring -> epoll (both engines now exist + listen)
	time.Sleep(100 * time.Millisecond)

	// Baseline FD count: both sub-engines built + bound, so only churn-induced
	// phantom-socket leaks push the post-run count higher.
	baseline := procSelfFDCount(t)

	const dialers = 8
	deadline := time.Now().Add(3 * time.Second)

	var wg sync.WaitGroup

	// Connection churn: each goroutine dials, optionally pokes the conn, then
	// closes — driving a high accept+close rate against the listen port.
	wg.Add(dialers)
	for i := 0; i < dialers; i++ {
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
				if err != nil {
					// Dials may transiently fail during the listen-fd close
					// window of a switch; that is expected churn, not a leak,
					// so keep going.
					continue
				}
				_ = c.SetDeadline(time.Now().Add(200 * time.Millisecond))
				_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
				_ = c.Close()
			}
		}()
	}

	// Switch stresser: fire ForceSwitch on a tight cadence so a listen-fd
	// close repeatedly races the in-flight accepts above.
	switchDone := make(chan struct{})
	switchAttempts := atomic.Int32{}
	go func() {
		defer close(switchDone)
		for time.Now().Before(deadline) {
			e.ForceSwitch()
			switchAttempts.Add(1)
			time.Sleep(300 * time.Microsecond)
		}
	}()

	wg.Wait()
	<-switchDone

	// Let the io_uring async close queue drain: poll the FD count down toward
	// baseline for up to ~2s before asserting, so a transient in-flight close
	// is not mistaken for a leak.
	// Generous slack vs in-flight churn + measurement jitter; a genuine
	// phantom-socket leak (v1.5.3 showed ~1100 orphaned FDs) dwarfs it.
	const slack = 128
	settleDeadline := time.Now().Add(2 * time.Second)
	after := procSelfFDCount(t)
	for after > baseline+slack && time.Now().Before(settleDeadline) {
		time.Sleep(20 * time.Millisecond)
		after = procSelfFDCount(t)
	}
	if after > baseline+slack {
		t.Errorf("fd leak: baseline=%d after=%d (slack=%d)", baseline, after, slack)
	}

	// The engine must still accept after the storm.
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("engine refused connections after storm: %v", err)
	}
	_ = c.Close()

	t.Logf("switchAttempts=%d switchRejected=%d adaptiveSwitches=%d fd baseline=%d after=%d",
		switchAttempts.Load(), e.SwitchRejectedCount(), e.Metrics().AdaptiveSwitches,
		baseline, after)
}
