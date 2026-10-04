//go:build linux

package adaptive

import (
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/deferlinger"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
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
//   - The storm actually closed listen sockets under the accepts.
//
// The race this test exists for is a listen-fd CLOSE against in-flight
// accepts. Since celeris#662 a switch only starts the outgoing engine's pause
// and that engine lingers for about 1.5 s before it closes anything, so a
// storm of switches every few hundred microseconds would keep resuming
// lingering listeners and never close one. The test therefore sets the
// linger to 0 and, after each switch, waits for the outgoing engine's
// listeners to close, as the synchronous pause did before, and asserts from
// deferlinger's close counter that closes happened. The shipped configuration,
// with the default linger, has its own storm below
// (TestAdaptiveSwitchVsAcceptChurnDefaultLinger).
func TestAdaptiveSwitchVsAcceptChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short mode")
	}
	oldLinger := deferlinger.SetLinger(0)
	t.Cleanup(func() { deferlinger.SetLinger(oldLinger) })
	e, stop := newBoundAdaptive(t)
	defer stop()
	// Only the storm below switches this engine.
	e.FreezeSwitching()

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
	// close repeatedly races the in-flight accepts above. After each switch,
	// wait for the outgoing engine's listeners to close.
	closes0 := deferlinger.Snapshot().Closes
	switchDone := make(chan struct{})
	switchAttempts := atomic.Int32{}
	go func() {
		defer close(switchDone)
		for time.Now().Before(deadline) {
			e.ForceSwitch()
			waitOutgoingClosed662(e)
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

	closes := deferlinger.Snapshot().Closes - closes0
	t.Logf("switchAttempts=%d switchRejected=%d adaptiveSwitches=%d listenerClosesDuringStorm=%d "+
		"fd baseline=%d after=%d",
		switchAttempts.Load(), e.SwitchRejectedCount(), e.Metrics().AdaptiveSwitches, closes,
		baseline, after)
	if closes == 0 {
		t.Errorf("the storm closed no listen socket, so the listen-fd close this test races " +
			"against in-flight accepts never happened")
	}
}

// TestAdaptiveSwitchVsAcceptChurnDefaultLinger is the same storm in the
// configuration that ships, with the default linger (celeris#662 review): the
// test above sets the linger to 0 so that its storm closes listeners, which
// leaves the default path to single-flap tests only. Here the switches come at
// intervals that land both inside the outgoing engine's linger -- a resume of
// lingering listeners racing the accepts in flight on them -- and after it,
// once the linger's close has happened, so the next switch back re-creates
// the listeners. After the storm and the lingers it started have ended, the
// fd count is back within the slack, the listeners on the port are exactly
// the active engine's, the engine serves, and both paths were taken: at least
// one linger ended in a resume (deferlinger Aborts) and at least one in a
// close (Closes).
func TestAdaptiveSwitchVsAcceptChurnDefaultLinger(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short mode")
	}
	e, port := startAdaptive662(t, resource.Config{})
	addr := e.Addr().String()

	// Build the lazy io_uring standby and switch back, then wait out the
	// warm-up's linger and the io_uring workers' park (see warmUp662), so the
	// baseline holds both engines' fixed fds and only epoll's listeners.
	warmUp662(t, e, port)
	baseline := procSelfFDCount(t)
	s0 := deferlinger.Snapshot()

	const dialers = 8
	gaps := []time.Duration{2 * time.Millisecond, 50 * time.Millisecond, 300 * time.Millisecond,
		deferlinger.Linger() + 300*time.Millisecond}
	stormEnd := time.Now().Add(6 * time.Second)
	var dialErrs, dials atomic.Int64
	var wg sync.WaitGroup
	for range dialers {
		wg.Go(func() {
			for time.Now().Before(stormEnd) {
				c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
				dials.Add(1)
				if err != nil {
					dialErrs.Add(1)
					continue
				}
				_ = c.SetDeadline(time.Now().Add(200 * time.Millisecond))
				_, _ = c.Write([]byte(idleSwitchReq662))
				_ = c.Close()
			}
		})
	}
	switches := 0
	for i := 0; time.Now().Before(stormEnd); i++ {
		e.ForceSwitch()
		switches++
		time.Sleep(gaps[i%len(gaps)])
	}
	wg.Wait()
	// Every linger the storm started ends within Linger of its switch; an
	// io_uring worker whose listener closed parks within one more ring wait.
	time.Sleep(deferlinger.Linger() + 1200*time.Millisecond)

	const slack = 128
	after := procSelfFDCount(t)
	for settle := time.Now().Add(3 * time.Second); after > baseline+slack && time.Now().Before(settle); {
		time.Sleep(20 * time.Millisecond)
		after = procSelfFDCount(t)
	}
	active := e.ActiveEngine()
	want := 0
	if nw, ok := active.(interface{ NumWorkers() int }); ok {
		want = nw.NumWorkers()
	}
	ls := listeners662(t, port)
	serves := serves662(addr, 5*time.Second)
	s1 := deferlinger.Snapshot()
	aborts, closes := s1.Aborts-s0.Aborts, s1.Closes-s0.Closes
	t.Logf("switches=%d dials=%d dialErrors=%d lingersEndedByResume=%d listenerCloses=%d "+
		"active=%v listeners=%s (want %d) fd baseline=%d after=%d serves=%v",
		switches, dials.Load(), dialErrs.Load(), aborts, closes, active.Type(), describe662(ls), want,
		baseline, after, serves)
	if after > baseline+slack {
		t.Errorf("fd leak: baseline=%d after=%d (slack=%d)", baseline, after, slack)
	}
	if want > 0 && len(ls) != want {
		t.Errorf("after the storm settled the port has listeners %s, want the active %v engine's %d",
			describe662(ls), active.Type(), want)
	}
	if !allOn662(ls) {
		t.Errorf("a listener that serves reads TCP_DEFER_ACCEPT off after the storm: %s", describe662(ls))
	}
	if !serves {
		t.Error("the engine does not serve after the storm")
	}
	if aborts == 0 {
		t.Error("no switch landed inside a linger, so the resume of lingering listeners was not exercised")
	}
	if closes == 0 {
		t.Error("no linger ended in a close during the storm, so the linger's close was not exercised")
	}
}

// waitOutgoingClosed662 blocks until the sub-engine the last switch left has
// closed its listeners: PauseAccept on it restarts nothing that has not
// started and returns once every listener is closed.
func waitOutgoingClosed662(e *Engine) {
	e.mu.Lock()
	active := e.ActiveEngine()
	out := e.primary
	if active == e.primary {
		out = e.secondary
	}
	e.mu.Unlock()
	if ac, ok := out.(engine.AcceptController); ok && out != nil {
		_ = ac.PauseAccept()
	}
}
