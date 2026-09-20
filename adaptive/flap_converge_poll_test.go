//go:build linux

package adaptive

// celeris#657, face 1, under load: after each switch the engine switched away
// from must CONVERGE, not merely thin out.
//
// TestBidirectionalFlap's verdict is a single sample after a 1.2 s dwell and
// tolerates a quarter of the connections left behind and one lost request per
// connection, which is why the placement bug survived it. This is the same
// scenario with the assertions the fix has to meet: the standby is polled
// every 25 ms and must be down to <= 2 connections within 500 ms of each
// switch, no client may lose a request at all, and the two hand-off witnesses
// must stay at zero.

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

func TestFlapConvergesPollSync(t *testing.T)  { flapConvergesPoll(t, respHandler{}, false) }
func TestFlapConvergesPollAsync(t *testing.T) { flapConvergesPoll(t, asyncRespHandler{}, true) }

func flapConvergesPoll(t *testing.T, h stream.Handler, async bool) {
	if testing.Short() {
		t.Skip("integration")
	}
	const (
		conns  = 64
		flaps  = 3
		bound  = 500 * time.Millisecond
		settle = 700 * time.Millisecond
	)
	e, addr, stop := s0Bind(t, resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, AsyncHandlers: async}, h)
	defer stop()

	stopLoad := make(chan struct{})
	var okCount, errCount atomic.Int64
	var cen s0Census
	wg := s0Drive(addr, conns, stopLoad, &okCount, &errCount, &cen)
	defer func() {
		select {
		case <-stopLoad:
		default:
			close(stopLoad)
		}
		wg.Wait()
	}()
	time.Sleep(settle) // conns establish on epoll, the default active

	for flap := 1; flap <= flaps; flap++ {
		// After flap 1 the active is io_uring, so the engine being switched
		// AWAY from alternates: epoll, io_uring, epoll.
		toIOUring := flap%2 == 1
		srcIOU, dstIOU := !toIOUring, toIOUring
		dir := "promote"
		if !toIOUring {
			dir = "revert"
		}
		before := subActive(e, srcIOU)
		e.ForceSwitch()
		convergedMs, atBound, trace := pollActive(func() int64 { return subActive(e, srcIOU) }, 2, bound)
		time.Sleep(settle)
		t.Logf("celeris657 FLAPPOLL async=%v flap=%d dir=%s before=%d converged_ms=%d at_%dms=%d "+
			"outgoing_after=%d incoming_after=%d ok=%d err=%d trace=[%s]",
			async, flap, dir, before, convergedMs, bound.Milliseconds(), atBound,
			subActive(e, srcIOU), subActive(e, dstIOU), okCount.Load(), errCount.Load(), trace)
		if before < conns/2 {
			t.Fatalf("celeris657 FLAPPOLL PREMISE: flap %d started with %d of %d conns on the outgoing engine",
				flap, before, conns)
		}
		if convergedMs < 0 {
			t.Errorf("celeris657 FLAPPOLL PLACEMENT: flap %d (%s) left %d of %d conns on the engine switched "+
				"away from %d ms later (want <= 2)", flap, dir, atBound, before, bound.Milliseconds())
		}
	}
	close(stopLoad)
	wg.Wait()

	m := e.Metrics()
	w1 := m.StaleRecvDataTransplanted + m.StaleRecvDataUnattributed
	pm, sm := e.primary.Metrics(), e.secondary.Metrics()
	t.Logf("celeris657 FLAPPOLL async=%v RESULT flaps=%d conns=%d wE=%d wI=%d ok=%d err=%d w1=%d w2=%d "+
		"doubleclaim=%d holdrescued=%d reapfailed=%d passes=%d census=%s",
		async, flaps, conns, pm.Workers, sm.Workers, okCount.Load(), errCount.Load(), w1,
		m.TransplantHandoffInFlight, m.TransplantDoubleClaim, m.TransplantHoldRescued,
		m.TransplantReapFailed, metricSoft(m, "TransplantSweepPasses"), cen.String())

	if async && pm.AsyncPromotedConns == 0 && sm.AsyncPromotedConns == 0 {
		t.Errorf("celeris657 FLAPPOLL PREMISE: no conn was promoted to a dispatch goroutine")
	}
	if n := errCount.Load(); n != 0 {
		t.Errorf("celeris657 FLAPPOLL LOSS: %d of %d keep-alive clients lost a request across %d flaps (census %s)",
			n, conns, flaps, cen.String())
	}
	if w1 != 0 {
		t.Errorf("celeris657 FLAPPOLL W1: %d requests were read by a recv that outlived its hand-off "+
			"(StaleRecvDataTransplanted=%d StaleRecvDataUnattributed=%d), want 0",
			w1, m.StaleRecvDataTransplanted, m.StaleRecvDataUnattributed)
	}
	if n := m.TransplantHandoffInFlight; n != 0 {
		t.Errorf("celeris657 FLAPPOLL W2: %d hand-offs were made with an op in flight, want 0", n)
	}
}
