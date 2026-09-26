//go:build linux

package adaptive

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// celeris#662: what the outgoing sub-engine's linger does to a switch.
//
// performSwitch starts the outgoing engine's pause and does not wait for it.
// For about 1.5 s the outgoing engine keeps its listeners, with
// TCP_DEFER_ACCEPT cleared, and keeps its SO_REUSEPORT share of new
// connections. These tests pin the three consequences that matter: a switch
// and Metrics() stay fast, a switch back inside the linger resumes the same
// listeners, and the #383 transplant ledger still balances when connections
// keep arriving on the outgoing engine during the linger.

// warmUp662 builds the io_uring standby, switches back to epoll, and waits
// for io_uring's linger to end, so only epoll's listeners are on the port.
func warmUp662(t *testing.T, e *Engine, port int) {
	t.Helper()
	_, iouringSet := buildStandby662(t, e, port)
	e.ForceSwitch()
	if got := e.ActiveEngine().Type(); got != engine.Epoll {
		t.Fatalf("warm-up left %v active, want epoll", got)
	}
	waitGone662(t, port, iouringSet, "the io_uring standby after the warm-up")
	// A paused io_uring worker whose listener has closed waits out one more
	// ring wait (up to a second) before it parks, and a resume that lands in
	// that wait is only seen after it: a separate, known lag. Let the workers
	// park, so the next switch to io_uring re-creates its listeners at once.
	time.Sleep(1100 * time.Millisecond)
}

// TestPerformSwitchDoesNotBlockOnLinger: the switch starts the outgoing
// engine's pause and returns; it does not wait out the linger, and Metrics(),
// which takes the same lock as a switch, is not held up by it either.
func TestPerformSwitchDoesNotBlockOnLinger(t *testing.T) {
	e, port := startAdaptive662(t, resource.Config{})
	warmUp662(t, e, port)
	before := listeners662(t, port)

	t0 := time.Now()
	e.ForceSwitch()
	switchWall := time.Since(t0)
	t1 := time.Now()
	_ = e.Metrics()
	metricsWall := time.Since(t1)
	after := listeners662(t, port)
	lingering := len(after) - len(without662(after, cookies662(before)))
	t.Logf("switchWall=%v metricsWall=%v outgoingListenersStillOpen=%d of %d",
		switchWall, metricsWall, lingering, len(before))

	if got := e.ActiveEngine().Type(); got != engine.IOUring {
		t.Fatalf("the switch left %v active, want io_uring", got)
	}
	if lingering == 0 {
		t.Errorf("premise: the outgoing epoll listeners were already closed when the switch " +
			"returned, so either there is no linger or the switch waited for it")
	}
	if switchWall >= 100*time.Millisecond {
		t.Errorf("ForceSwitch took %v, want < 100ms: the switch must not wait for the outgoing "+
			"engine's linger, which would hold e.mu for about 1.5 s", switchWall)
	}
	if metricsWall >= 50*time.Millisecond {
		t.Errorf("Metrics() took %v right after a switch, want < 50ms", metricsWall)
	}
}

// TestFlapWithinLingerKeepsListeners: a switch back while the engine it left
// is still lingering resumes that engine's own listeners -- never closed,
// never re-created -- with TCP_DEFER_ACCEPT back on, and the engine that was
// active in between lingers and closes in its turn.
func TestFlapWithinLingerKeepsListeners(t *testing.T) {
	e, port := startAdaptive662(t, resource.Config{})
	warmUp662(t, e, port)
	addr := e.Addr().String()
	epollBefore := listeners662(t, port)
	epollSet := cookies662(epollBefore)

	tPromote := time.Now()
	e.ForceSwitch() // epoll -> io_uring: epoll lingers
	// Wait for io_uring's listeners. A resumed io_uring worker that had not
	// parked yet re-creates its listener only after its current ring wait,
	// up to a second: a known, separate lag, not what this test is about.
	var mid []adSock662
	for dl := time.Now().Add(1200 * time.Millisecond); ; {
		mid = listeners662(t, port)
		if len(without662(mid, epollSet)) > 0 || time.Now().After(dl) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if d := time.Since(tPromote); d < 300*time.Millisecond {
		time.Sleep(300*time.Millisecond - d)
		mid = listeners662(t, port)
	}
	var epollMid []adSock662
	for _, s := range mid {
		if epollSet[s.cookie] {
			epollMid = append(epollMid, s)
		}
	}
	iouringSet := cookies662(without662(mid, epollSet))
	flapAt := time.Since(tPromote)

	e.ForceSwitch() // io_uring -> epoll, inside epoll's linger
	if got := e.ActiveEngine().Type(); got != engine.Epoll {
		t.Fatalf("the flap left %v active, want epoll", got)
	}
	var epollAfter []adSock662
	for dl := time.Now().Add(time.Second); ; {
		epollAfter = without662(listeners662(t, port), iouringSet)
		if allOn662(epollAfter) || time.Now().After(dl) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	iouringClosedAfter := waitGone662(t, port, iouringSet, "the io_uring engine after the flap")
	final := listeners662(t, port)
	serves := serves662(addr, 5*time.Second)
	t.Logf("epollBefore=%s epollDuringLinger=%s flapAt=+%v epollAfterFlap=%s iouringClosedAfter=%v "+
		"final=%s serves=%v", describe662(epollBefore), describe662(epollMid), flapAt,
		describe662(epollAfter), iouringClosedAfter, describe662(final), serves)

	if len(epollMid) != len(epollBefore) {
		t.Fatalf("premise: %v after the promotion %d of %d epoll listeners were open, so "+
			"epoll was not lingering", flapAt, len(epollMid), len(epollBefore))
	}
	for _, s := range epollMid {
		if s.deferOn {
			t.Fatal("premise: a lingering epoll listener reads TCP_DEFER_ACCEPT on")
		}
	}
	if len(iouringSet) == 0 {
		t.Fatal("premise: no io_uring listener was open after the promotion")
	}
	if !allOn662(epollAfter) {
		t.Errorf("after the flap the epoll listeners read %s, want all on: the resume did not "+
			"restore the option", describe662(epollAfter))
	}
	if len(final) != len(epollBefore) {
		t.Errorf("after the flap settled the port has listeners %s, want epoll's %s",
			describe662(final), describe662(epollBefore))
	}
	for i := range final {
		if i < len(epollBefore) && final[i].cookie != epollBefore[i].cookie {
			t.Errorf("an epoll listener was re-created across a flap within its linger "+
				"(cookie %d -> %d)", epollBefore[i].cookie, final[i].cookie)
		}
	}
	if !serves {
		t.Error("the engine does not serve after the flap")
	}
}

// TestLingerTransplantConserves: keep-alive connections and connection churn
// across a promotion. The keep-alives are transplanted to io_uring (#383)
// while the outgoing epoll keeps accepting churn during its linger, and when
// everything has closed the hand-off ledger balances: every connection
// detached was adopted (the celeris#624 witness), no bucket of silent loss
// moved, and the engine's accept and close counts match the hooks.
func TestLingerTransplantConserves(t *testing.T) {
	var connects, disconnects atomic.Int64
	e, port := startAdaptive662(t, resource.Config{
		OnConnect:    func(string) { connects.Add(1) },
		OnDisconnect: func(string) { disconnects.Add(1) },
	})
	warmUp662(t, e, port)
	addr := e.Addr().String()

	const kaConns = 32
	pauseKA := make(chan struct{})
	stopKA := make(chan struct{})
	var kaOK, kaErr atomic.Int64
	kaWG := driveKeepAlive(addr, kaConns, pauseKA, stopKA, &kaOK, &kaErr)

	var churnOK, churnErr atomic.Int64
	var mu sync.Mutex
	churnKinds := map[string]int{}
	churnStop := make(chan struct{})
	var churnWG sync.WaitGroup
	for range 4 {
		churnWG.Go(func() {
			for {
				select {
				case <-churnStop:
					return
				default:
				}
				c, err := net.DialTimeout("tcp", addr, time.Second)
				if err != nil {
					churnErr.Add(1)
					mu.Lock()
					churnKinds["dial"]++
					mu.Unlock()
					continue
				}
				_, werr := c.Write([]byte(idleSwitchReq662))
				o := "write"
				if werr == nil {
					o = readOutcome662(c)
				}
				_ = c.Close()
				if o == "200" {
					churnOK.Add(1)
					continue
				}
				churnErr.Add(1)
				mu.Lock()
				churnKinds[o]++
				mu.Unlock()
			}
		})
	}

	time.Sleep(500 * time.Millisecond) // keep-alives establish on epoll
	e.mu.Lock()
	epollEng := e.primary
	e.mu.Unlock()
	epollAccepts0 := epollEng.Metrics().AcceptCount
	e.ForceSwitch() // epoll -> io_uring: keep-alives transplant, epoll lingers
	if got := e.ActiveEngine().Type(); got != engine.IOUring {
		t.Fatalf("the promotion left %v active", got)
	}
	time.Sleep(2500 * time.Millisecond) // past the linger's close
	epollAcceptsDuringLinger := epollEng.Metrics().AcceptCount - epollAccepts0
	close(churnStop)
	churnWG.Wait()
	close(pauseKA) // idle keep-alives reach a clean boundary and converge
	time.Sleep(1500 * time.Millisecond)
	close(stopKA)
	kaWG.Wait()
	for dl := time.Now().Add(5 * time.Second); e.Metrics().ActiveConnections != 0 && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	m := e.Metrics()
	mu.Lock()
	kinds := churnKinds
	mu.Unlock()
	t.Logf("keepalive ok=%d err=%d | churn ok=%d err=%d kinds=%v | epoll accepts during the linger=%d | "+
		"transplant detached=%d adopted=%d slotOccupied=%d handoffRefused=%d drainStopped=%d "+
		"stranded=%d adoptRefused=%d | active=%d accepts=%d closes=%d onConnect=%d onDisconnect=%d errors=%d",
		kaOK.Load(), kaErr.Load(), churnOK.Load(), churnErr.Load(), kinds, epollAcceptsDuringLinger,
		m.TransplantDetached, m.TransplantAdopted, m.TransplantAdoptSlotOccupied, m.TransplantHandoffRefused,
		m.TransplantDrainStopped, m.TransplantStranded, m.TransplantAdoptRefused,
		m.ActiveConnections, m.AcceptCount, m.CloseCount, connects.Load(), disconnects.Load(), m.ErrorCount)

	if m.TransplantDetached == 0 {
		t.Fatal("premise: the promotion transplanted nothing, so the ledger was not exercised")
	}
	if epollAcceptsDuringLinger == 0 {
		t.Error("premise: the outgoing epoll accepted nothing during its linger, so the ledger " +
			"was not exercised under arrivals on the outgoing engine")
	}
	if m.TransplantDetached != m.TransplantAdopted {
		t.Errorf("transplant ledger does not balance: detached %d, adopted %d -- a connection "+
			"was lost in the hand-off (celeris#624)", m.TransplantDetached, m.TransplantAdopted)
	}
	for name, v := range map[string]uint64{
		"TransplantAdoptSlotOccupied": m.TransplantAdoptSlotOccupied,
		"TransplantHandoffRefused":    m.TransplantHandoffRefused,
		"TransplantDrainStopped":      m.TransplantDrainStopped,
		"TransplantStranded":          m.TransplantStranded,
		"TransplantAdoptRefused":      m.TransplantAdoptRefused,
	} {
		if v != 0 {
			t.Errorf("%s = %d, want 0", name, v)
		}
	}
	if m.ActiveConnections != 0 {
		t.Errorf("ActiveConnections = %d after every client closed, want 0", m.ActiveConnections)
	}
	if m.AcceptCount != m.CloseCount {
		t.Errorf("AcceptCount %d != CloseCount %d after every client closed", m.AcceptCount, m.CloseCount)
	}
	if got := connects.Load(); got != int64(m.AcceptCount) {
		t.Errorf("OnConnect fired %d times for %d accepts", got, m.AcceptCount)
	}
	if got := disconnects.Load(); got != int64(m.CloseCount) {
		t.Errorf("OnDisconnect fired %d times for %d closes", got, m.CloseCount)
	}
}
