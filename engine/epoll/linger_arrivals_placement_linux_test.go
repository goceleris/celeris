//go:build linux

package epoll

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/deferlinger"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#662 x celeris#657: where the linger's own arrivals end up.
//
// The pause keeps this engine's listeners open, with TCP_DEFER_ACCEPT
// cleared, for deferlinger.Linger (celeris#662). For that whole second and a
// half the engine a switch is leaving keeps its SO_REUSEPORT share of NEW
// connections, and it has to hand every one of them to the incoming engine
// like any other.
//
// Accepting them is the celeris#662 fix; the engine holding on to them is the
// celeris#657 defect, and it is what failed the base-vs-branch suite gate on
// 2026-09-18: with the linger in and celeris#657 open, an async 2048-conn
// ramp left 972-1170 connections on the outgoing epoll after 20 s with err=0.
// Not a loss -- placement.
//
// The test covers both orders in which a linger arrival can meet the drain,
// because different code reaches them:
//
//	PHASE A -- served BEFORE the drain was set. Its dispatch goroutine
//	parked when no drain existed, so askAtPark returned without asking
//	(ask.go) and nothing is owed for it. This is the flap case: a second
//	switch inside the first one's linger. Only the sweep looks again.
//
//	PHASE B -- served AFTER the drain was set. It joins the live set past
//	the sweep cursor, but this rig does NOT exercise the celeris#657 cycle
//	rule (R2 MAJOR-1): its arrivals come 20 ms apart, each one re-wakes the
//	sweep through wakeSweep, which clears dormancy whether or not it
//	records the arrival, and its own park-boundary ask (askAtPark, with the
//	drain set) moves it anyway. So phase B is the weaker half here and
//	phase A is what the controls turn. With the rule removed (wakeSweep not
//	recording the arrival) this test still passes;
//	TestSweepCannotGoDormantWithAnUnexaminedArrival and
//	TestSweepCostIsBoundedUnderContinuousArrivals, in this package and in
//	engine/iouring, are the tests that fail then.
//
// The handler is ASYNC on purpose: the per-event hand-off site examines a
// connection while its dispatch goroutine is inside the handler, where the
// parked-and-idle gate refuses it. Both phases then go SILENT, which is the
// state that produces no further event at all.
//
// Placement is judged twice. PLACE: once every listener has closed, the
// engine holds none of them. ORDER: they reach the incoming engine while
// the linger is still RUNNING, i.e. before the first outgoing listener
// closes. ORDER is what separates a drain that moves the linger's arrivals
// from one that merely waits for the linger to end and moves them then;
// PLACE alone cannot, because it starts looking at that end.
//
// Controls, each of which must FAIL:
//   - the linger at 0 (celeris#662 off): the listeners close at once, no
//     connection arrives at all, and ADMIT fires;
//   - the sweep and the park-boundary ask removed (celeris#657 P7/P8 off):
//     the arrivals are accepted and served and then stay, and PLACE fires;
//   - a sweep that does nothing while this loop lingers: every arrival is
//     still placed, phase A 1.5 s late, just after the close, and ORDER
//     fires.
//
// It changes deferlinger's package-level hooks through the rig it shares, so
// it does not run in parallel.

const (
	// lingerArrivalsL674 is how many connections arrive in each phase.
	lingerArrivalsL674 = 6
	// lingerArrivalSpaceL674 spaces them, so they land across the linger and
	// not all in one accept batch.
	lingerArrivalSpaceL674 = 20 * time.Millisecond
	// lingerPlaceBoundL674 is how long after the last listener closed the
	// drain is given to empty this engine. The sweep's own cadence is 2 ms
	// backing off to 64 ms, so this is two orders of magnitude of slack.
	lingerPlaceBoundL674 = 3 * time.Second
)

// asyncRespHandler674 answers every request immediately and marks every route
// async, so each connection is promoted to a per-conn dispatch goroutine.
type asyncRespHandler674 struct{}

func (asyncRespHandler674) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}

func (asyncRespHandler674) RouteAsync(_, _ string) bool { return true }
func (asyncRespHandler674) HasAsyncRoutes() bool        { return true }

var _ stream.AsyncRouteResolver = asyncRespHandler674{}

// startLingerAsyncL674 starts an epoll engine with the default configuration
// (TCP_DEFER_ACCEPT on), two loops and async routes.
func startLingerAsyncL674(t *testing.T) *lingerRigL662 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	accepted := &sync.Map{}
	e, err := New(resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		Resources:     resource.Resources{Workers: 2},
		Logger:        slog.New(slog.DiscardHandler),
		AsyncHandlers: true,
		OnConnect:     func(ra string) { accepted.LoadOrStore(ra, time.Now()) },
	}, asyncRespHandler674{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		_ = e.Listen(ctx)
		close(exited)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("engine did not stop within 10s")
		}
	})
	for dl := time.Now().Add(5 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(5 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine never bound")
	}
	loops := loops662(e)
	return &lingerRigL662{
		e: e, addr: addr, port: port, workers: len(loops),
		closed: func() bool {
			for _, l := range loops {
				if !l.listenFDClosed.Load() {
					return false
				}
			}
			return true
		},
		cancel: cancel, exited: exited, accepted: accepted,
	}
}

// waitClearedL674 returns the first moment at which every LISTEN socket on
// port reads TCP_DEFER_ACCEPT off -- the witness that the linger has begun --
// or the zero time if that never happened within bound.
func waitClearedL674(port int, bound time.Duration) time.Time {
	for dl := time.Now().Add(bound); time.Now().Before(dl); {
		ls, err := listenersL662(port)
		if err == nil && len(ls) > 0 {
			all := true
			for _, s := range ls {
				if s.deferOn {
					all = false
					break
				}
			}
			if all {
				return time.Now()
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	return time.Time{}
}

// arriveDuringLingerL674 dials n connections, spaced, at the lingering
// listener, then sends one request on each and reads its answer. The dials
// are spaced so the arrivals land across the linger rather than in one accept
// batch; the exchanges are concurrent so the whole phase fits inside it.
// Afterwards every connection is open and SILENT, the state a keep-alive is
// in when a drain has to move it. They are closed at cleanup.
func arriveDuringLingerL674(t *testing.T, addr string, n int) (conns []net.Conn, refused int, tally map[string]int) {
	t.Helper()
	for range n {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			refused++
		} else {
			conns = append(conns, c)
		}
		time.Sleep(lingerArrivalSpaceL674)
	}
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return conns, refused, tallyL662(writeAllL662(conns))
}

// lingerServeTarget674 stands in for the INCOMING engine: it adopts the
// descriptor and then answers on it, as the other sub-engine would. A target
// that only holds the descriptor cannot tell a connection that was moved from
// one that was dropped, because either way its client never gets an answer.
type lingerServeTarget674 struct {
	adopted atomic.Int64
	// from records the client address of every adopted connection, stored
	// before AdoptConn returns: the ORDER check reads it.
	from  sync.Map
	mu    sync.Mutex
	conns []net.Conn
	wg    sync.WaitGroup
}

func (s *lingerServeTarget674) AdoptConn(fd int, _ engine.Carryover) error {
	f := os.NewFile(uintptr(fd), "adopted")
	c, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		return nil // fd closed above; nothing for the source to reclaim
	}
	s.from.Store(c.RemoteAddr().String(), struct{}{})
	s.adopted.Add(1)
	s.mu.Lock()
	s.conns = append(s.conns, c)
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		br := bufio.NewReader(c)
		for {
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, req.Body)
			_ = req.Body.Close()
			if _, err := c.Write([]byte("HTTP/1.1 200 OK\r\ncontent-type: text/plain\r\ncontent-length: 2\r\n\r\nok")); err != nil {
				return
			}
		}
	}()
	return nil
}

func (s *lingerServeTarget674) close() {
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

var _ engine.TransplantTarget = (*lingerServeTarget674)(nil)

// acceptedHereL674 counts how many of conns this engine itself accepted.
// OnConnect fires on the accept path, so a client's local address in the
// rig's map is the witness that the lingering listener -- and not some other
// socket -- took it.
func acceptedHereL674(r *lingerRigL662, conns []net.Conn) int {
	return len(acceptedAddrsL674(r, conns))
}

// acceptedAddrsL674 returns the client addresses of those of conns this
// engine itself accepted (see acceptedHereL674).
func acceptedAddrsL674(r *lingerRigL662, conns []net.Conn) []string {
	var out []string
	for _, c := range conns {
		a := c.LocalAddr().String()
		if _, ok := r.accepted.Load(a); ok {
			out = append(out, a)
		}
	}
	return out
}

// placedBeforeCloseL674 is the ORDER check. It polls until either every
// address in want has been adopted by tgt while no outgoing listener had
// closed (placed), or an outgoing listener has closed first, or bound has
// passed. Each poll reads the adoption set FIRST and the listeners' closed
// flags SECOND, and there is no time constant in the verdict: only the order
// of two events.
//
// Why a drain that waits for the linger to end cannot pass: a loop stores
// listenFDClosed at the top of the iteration that ends its linger (the
// stepAcceptPause -> closeListenerAfterDrain close, then the flag store) and
// runs its sweep LATER IN THAT SAME ITERATION; lingerUntil stays nonzero
// through closeListenerAfterDrain's accept-queue drain and is reset only
// after unix.Close. So a sweep that does nothing while lingerUntil != 0
// adopts a phase-A connection only after that loop's flag is set, and a poll
// that has seen the adoption then reads the flag as set. Unmutated, every
// adoption here completes more than a second before the first close
// (measured in golang:1.27 containers, --cpus 4, -race: last adoption
// +228-233 ms after BeginPauseAccept, first close +1500 ms).
func placedBeforeCloseL674(tgt *lingerServeTarget674, want []string, anyClosed func() bool, bound time.Duration) (placed bool, missing int) {
	dl := time.Now().Add(bound)
	for {
		missing = 0
		for _, a := range want {
			if _, ok := tgt.from.Load(a); !ok {
				missing++
			}
		}
		closedNow := anyClosed()
		switch {
		case missing == 0 && !closedNow:
			return true, 0
		case closedNow, time.Now().After(dl):
			return false, missing
		}
		time.Sleep(time.Millisecond)
	}
}

// TestLingerArrivalsReachTheIncomingEngine is the joint celeris#662 x
// celeris#657 gate described at the top of this file.
func TestLingerArrivalsReachTheIncomingEngine(t *testing.T) {
	requireMigrateReqZeroL662(t)
	r := startLingerAsyncL674(t)

	tgt := &lingerServeTarget674{}
	t.Cleanup(tgt.close)

	// Start the outgoing engine's pause without waiting for its linger, as
	// adaptive's performSwitch does (celeris#662). The listeners stay open
	// and accepting from here until their own deadline.
	r.e.BeginPauseAccept()
	tCleared := waitClearedL674(r.port, 2*time.Second)

	// PHASE A: arrivals served while NO drain is set. Their dispatch
	// goroutines park with nothing owed for them.
	connsA, refusedA, tallyA := arriveDuringLingerL674(t, r.addr, lingerArrivalsL674)

	// The drain, still inside the linger.
	r.e.StartTransplant(tgt)
	t.Cleanup(r.e.StopTransplant)

	// PHASE B: arrivals served with the drain already set. They join the
	// live set past the sweep cursor.
	connsB, refusedB, tallyB := arriveDuringLingerL674(t, r.addr, lingerArrivalsL674)

	lingerStillOpen := !r.closed()
	wanted := 2 * lingerArrivalsL674
	acceptedHere := acceptedHereL674(r, connsA) + acceptedHereL674(r, connsB)
	served := tallyA["200"] + tallyB["200"]
	refused := refusedA + refusedB

	// ORDER: every arrival this engine accepted reaches the incoming engine
	// before the first outgoing listener closes.
	loops := loops662(r.e)
	anyClosed := func() bool {
		for _, l := range loops {
			if l.listenFDClosed.Load() {
				return true
			}
		}
		return false
	}
	placedEarly, missingAtClose := placedBeforeCloseL674(tgt,
		append(acceptedAddrsL674(r, connsA), acceptedAddrsL674(r, connsB)...),
		anyClosed, deferlinger.Linger()+3*time.Second)

	for dl := time.Now().Add(deferlinger.Linger() + 3*time.Second); !r.closed() && time.Now().Before(dl); {
		time.Sleep(5 * time.Millisecond)
	}
	closedOK := r.closed()

	adopted := 0
	for dl := time.Now().Add(lingerPlaceBoundL674); ; {
		adopted = int(tgt.adopted.Load())
		if adopted >= acceptedHere || time.Now().After(dl) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	m := r.e.Metrics()
	t.Logf("celeris662 LINGERPLACE loops=%d clearSeen=%v arrivals=%d servedA=%v servedB=%v refused=%d "+
		"acceptedHere=%d openAtLastArrival=%v placedBeforeClose=%v missingAtClose=%d listenersClosed=%v adopted=%d live=%d detached=%d "+
		"passes=%d residual=[det=%d h2=%d pin=%d uns=%d busy=%d] deferlinger=%+v",
		r.workers, !tCleared.IsZero(), wanted, tallyA, tallyB, refused,
		acceptedHere, lingerStillOpen, placedEarly, missingAtClose, closedOK, adopted, m.ActiveConnections, m.TransplantDetached,
		metricSoft(r.e, "TransplantSweepPasses"),
		metricSoft(r.e, "TransplantResidualDetached"), metricSoft(r.e, "TransplantResidualH2"),
		metricSoft(r.e, "TransplantResidualPinned"), metricSoft(r.e, "TransplantResidualUnstarted"),
		metricSoft(r.e, "TransplantResidualBusy"), deferlinger.Snapshot())

	// PREMISE: the arrivals landed on a listener that was still open because
	// of the linger, and the drain's end was reached.
	if !lingerStillOpen {
		t.Errorf("celeris662 LINGERPLACE PREMISE: every listener had already closed when the last "+
			"arrival dialled, so the arrivals say nothing about the linger (clear seen: %v)",
			!tCleared.IsZero())
	}
	if !closedOK {
		t.Errorf("celeris662 LINGERPLACE PREMISE: a listener never closed, so the drain's end was " +
			"never reached")
	}

	// ADMIT (celeris#662): the lingering listener took every one of them, and
	// every one got its answer.
	if refused != 0 || acceptedHere != wanted {
		t.Errorf("celeris662 LINGERPLACE ADMIT: of %d connections dialled after the pause began, %d "+
			"were refused and %d were accepted by this engine (clear seen: %v). Keeping the listener "+
			"open and accepting for the linger is the whole of the celeris#662 fix",
			wanted, refused, acceptedHere, !tCleared.IsZero())
	}
	if served != wanted {
		t.Errorf("celeris662 LINGERPLACE ADMIT: %d of %d arrivals were served (phase A %v, phase B %v)",
			served, wanted, tallyA, tallyB)
	}

	// ORDER (celeris#662 x celeris#657): and it moved them while the linger
	// was still running. With nothing accepted there is nothing to order,
	// and ADMIT has already said so.
	if acceptedHere > 0 && !placedEarly {
		t.Errorf("celeris662 LINGERPLACE ORDER: %d of the %d connections this engine accepted during "+
			"its own pause linger had not reached the incoming engine when the first outgoing listener "+
			"closed. The drain was set more than a second before that close; a drain that only moves them "+
			"once the linger is over leaves the engine a switch is leaving holding them for the whole "+
			"linger, which is the placement celeris#657 is about",
			missingAtClose, acceptedHere)
	}

	// PLACE (celeris#657): and the outgoing engine holds none of them.
	if adopted < acceptedHere {
		t.Errorf("celeris662 LINGERPLACE PLACE: %d of the %d connections this engine accepted during "+
			"its own pause linger reached the incoming engine; %d are still here with the drain set. "+
			"Phase A parked before the drain existed, so no ask was made for it; phase B joined past "+
			"the sweep cursor. Both then went silent, and nothing but the celeris#657 sweep and its "+
			"park-boundary ask looks at a connection that produces no event",
			adopted, acceptedHere, acceptedHere-adopted)
	}
	if m.ActiveConnections != 0 {
		t.Errorf("celeris662 LINGERPLACE PLACE: the outgoing engine still holds %d connections after "+
			"the drain emptied it of %d", m.ActiveConnections, adopted)
	}
	if adopted > 0 && m.TransplantDetached != uint64(adopted) {
		t.Errorf("celeris662 LINGERPLACE LEDGER: TransplantDetached=%d against %d adopts",
			m.TransplantDetached, adopted)
	}
}
