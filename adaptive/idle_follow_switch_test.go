//go:build linux

package adaptive

// celeris#657, face 1: PLACEMENT.
//
// An engine examines a connection for the #383 hand-off only at that
// connection's own event. A keep-alive connection that is IDLE when a switch
// happens produces no event, so nothing ever looks at it: measured on main,
// all 64 stayed on the outgoing epoll for 3 s at a promotion, and 64 of 64 at
// every sample for 1.5 s at a revert. When the clients resumed, the
// connections moved with a recv still armed and lost requests.
//
// These four tests are that, as a verdict. Sixty-four keep-alive clients go
// idle, the engine switches, and within 500 ms the engine switched AWAY from
// must hold none of them. Then the clients resume: A's post-resume placement
// assertion, that every connection is now served by the new active, with no
// client error and no request read by a recv that outlived its hand-off (W1)
// and no hand-off made with an op in flight (W2).
//
// The revert cells run in X1's no-listener shape: the switch to io_uring
// happens with no client connected, so the outgoing epoll listener is already
// gone and every connection below is accepted by io_uring. On main that is
// also the shape in which the outgoing io_uring worker waits a full second
// between ring completions, which is what P10 (A2w) is for.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// metricSoft reads one EngineMetrics field by name, or -1 when this tree has
// no such field. It lets a log line name a counter the base does not have yet
// without failing the package build, so the same test file produces the
// failing-first observation and the fixed one.
func metricSoft(m engine.EngineMetrics, name string) int64 {
	v := reflect.ValueOf(m).FieldByName(name)
	if !v.IsValid() {
		return -1
	}
	return int64(v.Uint())
}

// Client modes for s0DriveMode.
const (
	modeRun  = 0
	modeIdle = 1
	modeStop = 2
)

// s0DriveMode is s0Drive with a mode word, so the SAME connections can be
// driven, held open and idle, and then driven again. It keeps s0Drive's shape:
// write a request, read the response with a 2 s deadline, stop at the first
// error and never redial, so an error count is the number of connections that
// lost a request.
func s0DriveMode(addr string, conns int, mode *atomic.Int32, ok, errc *atomic.Int64, cen *s0Census) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
			if derr != nil {
				errc.Add(1)
				cen.add("dial", derr)
				return
			}
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			for {
				switch mode.Load() {
				case modeStop:
					return
				case modeIdle:
					time.Sleep(5 * time.Millisecond)
					continue
				}
				if _, werr := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); werr != nil {
					errc.Add(1)
					cen.add("write", werr)
					return
				}
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				resp, rerr := http.ReadResponse(br, nil)
				if rerr != nil {
					errc.Add(1)
					cen.add("read", rerr)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				ok.Add(1)
			}
		}()
	}
	return &wg
}

// subEngine is one sub-engine of e, and subActive its live-connection gauge.
// The standby is built lazily, so before the first promotion e.secondary is
// nil and reads as zero (the same accessor TestFlapConnsPerRing uses).
func subEngine(e *Engine, ioUring bool) engine.Engine {
	if ioUring {
		return e.secondary
	}
	return e.primary
}

func subActive(e *Engine, ioUring bool) int64 {
	en := subEngine(e, ioUring)
	if en == nil {
		return 0
	}
	return en.Metrics().ActiveConnections
}

// pollActive samples a live-connection gauge every 25 ms for up to bound, and
// returns the first elapsed time at which it was <= want (-1 if never), the
// value at bound, and a trace.
func pollActive(get func() int64, want int64, bound time.Duration) (convergedMs, atBound int64, trace string) {
	convergedMs, atBound = -1, -1
	var b strings.Builder
	t0 := time.Now()
	for {
		el := time.Since(t0)
		n := get()
		if convergedMs < 0 && n <= want {
			convergedMs = el.Milliseconds()
		}
		fmt.Fprintf(&b, "%d:%d ", el.Milliseconds(), n)
		if el >= bound {
			atBound = n
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return convergedMs, atBound, strings.TrimSpace(b.String())
}

func TestIdleConnsFollowSwitchPromoteSync(t *testing.T) {
	idleFollowSwitch(t, respHandler{}, false, false)
}

func TestIdleConnsFollowSwitchPromoteAsync(t *testing.T) {
	idleFollowSwitch(t, asyncRespHandler{}, true, false)
}

func TestIdleConnsFollowSwitchRevertSync(t *testing.T) {
	idleFollowSwitch(t, respHandler{}, false, true)
}

func TestIdleConnsFollowSwitchRevertAsync(t *testing.T) {
	idleFollowSwitch(t, asyncRespHandler{}, true, true)
}

func idleFollowSwitch(t *testing.T, h stream.Handler, async, revert bool) {
	if testing.Short() {
		t.Skip("integration")
	}
	const (
		conns = 64
		bound = 500 * time.Millisecond
	)
	e, addr, stop := s0Bind(t, resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, AsyncHandlers: async}, h)
	defer stop()

	dir := "promote"
	if revert {
		dir = "revert"
		// X1's no-listener shape: switch to io_uring with nothing connected,
		// so the outgoing epoll listener is gone before the first dial and
		// every connection below is accepted by io_uring.
		e.ForceSwitch()
		time.Sleep(500 * time.Millisecond)
	}
	// The engine being switched AWAY from, and the one being switched to.
	// Read through subActive: on a promotion the io_uring standby does not
	// exist until this test's switch builds it.
	srcIOU := revert
	dstIOU := !revert

	var mode atomic.Int32
	var okCount, errCount atomic.Int64
	var cen s0Census
	wg := s0DriveMode(addr, conns, &mode, &okCount, &errCount, &cen)
	defer func() { mode.Store(modeStop); wg.Wait() }()

	time.Sleep(700 * time.Millisecond)
	promoted := subEngine(e, srcIOU).Metrics().AsyncPromotedConns
	mode.Store(modeIdle) // each client finishes its request, then sends nothing
	time.Sleep(300 * time.Millisecond)

	before := subActive(e, srcIOU)
	t.Logf("celeris657 IDLE-%s async=%v BEFORE outgoing=%d incoming=%d promoted=%d ok=%d err=%d",
		dir, async, before, subActive(e, dstIOU), promoted, okCount.Load(), errCount.Load())

	e.ForceSwitch()
	convergedMs, atBound, trace := pollActive(func() int64 { return subActive(e, srcIOU) }, 2, bound)
	t.Logf("celeris657 IDLE-%s async=%v TRACE ms:outgoing %s", dir, async, trace)

	// Post-resume placement (A): the same connections must now be served by
	// the engine the switch made active, and serve without a single error.
	mode.Store(modeRun)
	time.Sleep(1500 * time.Millisecond)
	srcAfter := subActive(e, srcIOU)
	dstAfter := subActive(e, dstIOU)
	mode.Store(modeStop)
	wg.Wait() // a request lost at the hand-off surfaces here, as its read deadline

	m := e.Metrics()
	w1 := m.StaleRecvDataTransplanted + m.StaleRecvDataUnattributed
	t.Logf("celeris657 IDLE-%s async=%v RESULT before=%d converged_ms=%d at_%dms=%d resumed_outgoing=%d "+
		"resumed_incoming=%d ok=%d err=%d w1=%d w2=%d passes=%d residual=[det=%d h2=%d pin=%d busy=%d] census=%s",
		dir, async, before, convergedMs, bound.Milliseconds(), atBound, srcAfter, dstAfter,
		okCount.Load(), errCount.Load(), w1, m.TransplantHandoffInFlight,
		metricSoft(m, "TransplantSweepPasses"), metricSoft(m, "TransplantResidualDetached"),
		metricSoft(m, "TransplantResidualH2"), metricSoft(m, "TransplantResidualPinned"),
		metricSoft(m, "TransplantResidualBusy"), cen.String())

	if before < conns-2 {
		t.Fatalf("celeris657 IDLE-%s PREMISE: only %d of %d conns were on the outgoing engine before the switch",
			dir, before, conns)
	}
	if async && promoted == 0 {
		t.Errorf("celeris657 IDLE-%s PREMISE: no conn was promoted to its dispatch goroutine", dir)
	}
	if convergedMs < 0 {
		t.Errorf("celeris657 IDLE-%s PLACEMENT: %d of %d idle conns were still on the engine switched away from "+
			"%d ms after the switch (want <= 2): nothing examines a connection that sends nothing",
			dir, atBound, before, bound.Milliseconds())
	}
	if srcAfter > 2 || dstAfter < conns-2 {
		t.Errorf("celeris657 IDLE-%s PLACEMENT-RESUME: after the clients resumed, %d conns were on the engine "+
			"switched away from and %d on the new active (want <= 2 and >= %d)", dir, srcAfter, dstAfter, conns-2)
	}
	if n := errCount.Load(); n != 0 {
		t.Errorf("celeris657 IDLE-%s LOSS: %d of %d keep-alive clients lost a request across the idle switch and "+
			"the resume (census %s)", dir, n, conns, cen.String())
	}
	if w1 != 0 {
		t.Errorf("celeris657 IDLE-%s W1: %d requests were read by a recv that outlived its hand-off "+
			"(StaleRecvDataTransplanted=%d StaleRecvDataUnattributed=%d), want 0",
			dir, w1, m.StaleRecvDataTransplanted, m.StaleRecvDataUnattributed)
	}
	if n := m.TransplantHandoffInFlight; n != 0 {
		t.Errorf("celeris657 IDLE-%s W2: %d hand-offs were made with an op in flight, want 0", dir, n)
	}
}
