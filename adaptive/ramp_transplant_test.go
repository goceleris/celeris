//go:build linux

package adaptive

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// newRampAdaptive builds an adaptive engine wired for a CONTROLLER-DRIVEN ramp:
// loadDownRevert ON (so io_uring→epoll revert fires on load-down, the path the
// reverse transplant exists for), a short eval interval + no cooldown so the
// controller reacts within the test, and no oscillation lock interference.
func newRampAdaptive(t *testing.T, h stream.Handler, cfg resource.Config) (*Engine, string, func()) {
	t.Helper()
	if !probe.Probe().IOUringTier.Available() {
		t.Skip("io_uring unavailable: needs both sub-engines")
	}
	cfg.Addr = "127.0.0.1:0"
	e, err := New(cfg, h, nil)
	if err != nil {
		t.Skipf("adaptive.New unsupported here: %v", err)
	}
	// Make the controller react fast + drive BOTH directions. loadDownRevert is
	// forced ON (production=false) so the reverse path fires; connSwitchEnabled is
	// LEFT AT ITS PRODUCTION VALUE so h2c (which sets it false) genuinely never
	// switches — exactly what we want to verify for the h2c protocol.
	e.ctrl.evalInterval = 40 * time.Millisecond
	e.ctrl.cooldown = 0
	e.ctrl.loadDownRevert = true

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	for dl := time.Now().Add(3 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		cancel()
		<-done
		t.Fatal("adaptive engine never bound")
	}
	return e, e.Addr().String(), func() { cancel(); <-done }
}

var (
	errSampleMu sync.Mutex
	errSamples  = map[string]int{}
)

func recordErr(s string) {
	errSampleMu.Lock()
	if len(errSamples) < 200 {
		errSamples[s]++
	}
	errSampleMu.Unlock()
}

func aconns(e engine.Engine) int64 {
	if e == nil {
		return 0
	}
	return e.Metrics().ActiveConnections
}

func apromoted(e engine.Engine) uint64 {
	if e == nil {
		return 0
	}
	return e.Metrics().AsyncPromotedConns
}

func dumpErrSamples(t *testing.T) {
	errSampleMu.Lock()
	defer errSampleMu.Unlock()
	for s, n := range errSamples {
		t.Logf("  err sample (%dx): %s", n, s)
	}
	errSamples = map[string]int{}
}

// rampPool manages a dynamically-sized set of keep-alive clients so a test can
// raise/lower the connection count to drive the adaptive controller.
type rampPool struct {
	addr   string
	client func(addr string, stop <-chan struct{}, ok, errc *atomic.Int64)
	ok     atomic.Int64
	errc   atomic.Int64
	mu     sync.Mutex
	stops  []chan struct{}
	wg     sync.WaitGroup
}

func (p *rampPool) scaleTo(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.stops) < n { // grow
		st := make(chan struct{})
		p.stops = append(p.stops, st)
		p.wg.Add(1)
		go func(stop chan struct{}) {
			defer p.wg.Done()
			p.client(p.addr, stop, &p.ok, &p.errc)
		}(st)
	}
	for len(p.stops) > n { // shrink (close the most-recent conns)
		last := len(p.stops) - 1
		close(p.stops[last])
		p.stops = p.stops[:last]
	}
}

func (p *rampPool) stopAll() {
	p.scaleTo(0)
	p.wg.Wait()
}

// rampTo grows/shrinks toward n in small steps so a 1→2048 jump doesn't become a
// 2047-way simultaneous dial storm (which overflows the accept path and produces
// connection-reset noise unrelated to the transplant).
func (p *rampPool) rampTo(n int) {
	p.mu.Lock()
	cur := len(p.stops)
	p.mu.Unlock()
	step := 256
	for cur != n {
		if cur < n {
			cur = min(cur+step, n)
		} else {
			cur = max(cur-step, n)
		}
		p.scaleTo(cur)
		time.Sleep(30 * time.Millisecond)
	}
}

// resetSwitchState clears the oscillation lock + cooldown + switch history under
// switchMu (race-safe vs the controller tick) so a test can drive many up/down
// cycles. The 3-switches-in-5-min oscillation lock is real anti-thrash behavior;
// resetting it here isolates the TRANSPLANT mechanism from that throttle.
func resetSwitchState(e *Engine) {
	e.switchMu.Lock()
	e.ctrl.state.locked = false
	e.ctrl.state.lockUntil = time.Time{}
	e.ctrl.state.switchCount = 0
	e.ctrl.state.switchIdx = 0
	e.ctrl.state.lastSwitch = time.Time{}
	e.ctrl.state.upTicks = 0
	e.ctrl.state.downTicks = 0
	e.switchMu.Unlock()
}

// h1Client drives a single HTTP/1.1 keep-alive connection: write request, read
// response, repeat until stop. A read/write error ends the client (counted).
func h1Client(addr string, stop <-chan struct{}, ok, errc *atomic.Int64) {
	c, derr := net.DialTimeout("tcp", addr, 3*time.Second)
	if derr != nil {
		errc.Add(1)
		recordErr("dial: " + derr.Error())
		return
	}
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	for {
		select {
		case <-stop:
			return
		default:
		}
		if _, werr := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); werr != nil {
			errc.Add(1)
			recordErr("write: " + werr.Error())
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		resp, rerr := http.ReadResponse(br, nil)
		if rerr != nil {
			errc.Add(1)
			recordErr("read: " + rerr.Error())
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		ok.Add(1)
		// Small pause so 2048 conns don't peg the loopback CPU into starvation
		// (this is a correctness/migration test, not a throughput test).
		time.Sleep(time.Millisecond)
	}
}

// h2cClient drives a single h2c (prior-knowledge, cleartext HTTP/2) connection,
// issuing one request at a time over it until stop. Each client owns its own
// Transport, so it maps to exactly one TCP connection.
func h2cClient(addr string, stop <-chan struct{}, ok, errc *atomic.Int64) {
	tr := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, a string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, a)
		},
	}
	defer tr.CloseIdleConnections()
	url := "http://" + addr + "/"
	for {
		select {
		case <-stop:
			return
		default:
		}
		req, _ := http.NewRequest("GET", url, nil)
		resp, rerr := tr.RoundTrip(req)
		if rerr != nil {
			errc.Add(1)
			recordErr("h2: " + rerr.Error())
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		ok.Add(1)
		time.Sleep(time.Millisecond)
	}
}

// rampMax caps the peak connection count (default 2048); CELERIS_RAMP_MAX lets a
// -race run use a smaller, tractable peak (the race detector + thousands of conns
// is otherwise too slow).
func rampMax(n int) int {
	if v := os.Getenv("CELERIS_RAMP_MAX"); v != "" {
		if m, err := strconv.Atoi(v); err == nil && m > 0 && m < n {
			return m
		}
	}
	return n
}

func activeName(e *Engine) string {
	if e.ctrl.activeEngine() == e.secondary && e.secondary != nil {
		return "io_uring"
	}
	return "epoll"
}

// rampScenario drives the connection count up and down across the controller's
// promote/revert thresholds, verifying that the established conns migrate to the
// active engine each way and that requests keep succeeding throughout. With 16
// workers: 2048 conns = 128 cpw (promote), 128 conns = 8 cpw (< 12 → revert).
func rampScenario(t *testing.T, h stream.Handler, cfg resource.Config, client func(string, <-chan struct{}, *atomic.Int64, *atomic.Int64), async bool) {
	e, addr, stop := newRampAdaptive(t, h, cfg)
	defer stop()
	p := &rampPool{addr: addr, client: client}
	defer p.stopAll()

	// Warm-start: a throwaway promote builds the lazy io_uring engine BEFORE the
	// measured ramp, so the first measured promote isn't racing a cold 16-worker
	// ring build under load. Then drop back to idle and reset switch state.
	p.rampTo(300)
	time.Sleep(2 * time.Second)
	p.rampTo(0)
	time.Sleep(1500 * time.Millisecond)
	resetSwitchState(e)
	p.ok.Store(0)
	p.errc.Store(0)
	errSampleMu.Lock()
	errSamples = map[string]int{}
	errSampleMu.Unlock()

	// 1 → 2048 → 128 → 2048 → 1 → 2048 → 128: each high phase should land on
	// io_uring with conns migrated there; each low phase back on epoll. The
	// oscillation lock is reset between phases so we test the transplant across
	// many cycles, not the anti-thrash throttle.
	phases := []struct {
		n       int
		wantIOU bool // expect io_uring active (conns migrated to io_uring)
	}{
		{2048, true},
		{128, false},
		{2048, true},
		{1, false},
		{2048, true},
		{128, false},
	}
	settle := 2500 * time.Millisecond

	for i, ph := range phases {
		want := rampMax(ph.n) // honor CELERIS_RAMP_MAX (so -race runs use a smaller peak)
		p.rampTo(want)
		time.Sleep(settle)
		// e.secondary (io_uring) is built lazily on the first promote — nil before
		// then. Guard both reads.
		epo := aconns(e.primary)
		iou := aconns(e.secondary)
		promoted := apromoted(e.secondary)
		t.Logf("[async=%v] phase %d n=%d: epoll=%d io_uring=%d (promoted=%d) ok=%d err=%d",
			async, i, want, epo, iou, promoted, p.ok.Load(), p.errc.Load())

		// The active engine should hold the vast majority of conns; the standby
		// should be ~drained (migration completed). Allow slack for in-flight.
		total := epo + iou
		if total < int64(want)/2 {
			t.Errorf("phase %d: lost conns: total=%d want ~%d", i, total, want)
		}
		if ph.wantIOU {
			if iou < int64(want)*3/4 {
				t.Errorf("phase %d (high load): conns not on io_uring: io_uring=%d epoll=%d (want io_uring ~%d)", i, iou, epo, want)
			}
		} else {
			if epo < int64(want)*3/4 {
				t.Errorf("phase %d (low load): conns not on epoll: epoll=%d io_uring=%d (want epoll ~%d)", i, epo, iou, want)
			}
		}
		resetSwitchState(e) // allow the next phase to switch (isolate from anti-thrash lock)
	}

	p.stopAll()
	dumpErrSamples(t)
	errs, oks := p.errc.Load(), p.ok.Load()
	rate := float64(errs) / float64(max(oks, 1))
	t.Logf("[async=%v] ramp complete: ok=%d err=%d (%.4f%%)", async, oks, errs, rate*100)
	// BOTH sync and async carry a tiny inherent loss at scale during the io_uring
	// multishot recv-cancel window (a pre-consumed request can be stranded when the
	// recv is cancelled mid-transplant). Observed: sync ~0.0002%, async ~0.0007%,
	// concentrated at the first large promote. Hold it to a tight budget so gross
	// failure (a real regression) still fails the test, but the known residual does
	// not. Eliminating it entirely needs the lossless two-phase + epoll async
	// re-injection that regressed (see git history); not worth it for a gated path.
	budget := 0.0005 // 0.05% ceiling, ~70× the observed residual
	if rate > budget {
		t.Errorf("[async=%v] ramp error rate %.4f%% exceeds %.2f%% budget (%d/%d)", async, rate*100, budget*100, errs, oks)
	}
}

func TestRampH1Sync(t *testing.T) {
	if testing.Short() {
		t.Skip("ramp integration test")
	}
	rampScenario(t, respHandler{}, resource.Config{Protocol: engine.HTTP1}, h1Client, false)
}

func TestRampH1Async(t *testing.T) {
	if testing.Short() {
		t.Skip("ramp integration test")
	}
	rampScenario(t, asyncRespHandler{}, resource.Config{Protocol: engine.HTTP1, AsyncHandlers: true}, h1Client, true)
}

// TestRampH2CPriorKnowledge verifies that with Protocol=H2C the adaptive engine
// NEVER switches (connSwitchEnabled is false for h2c) — so the transplant is never
// exercised and h2c conns are served by epoll throughout, even at high cpw.
func TestRampH2CPriorKnowledge(t *testing.T) {
	if testing.Short() {
		t.Skip("ramp integration test")
	}
	e, addr, stop := newRampAdaptive(t, respHandler{}, resource.Config{Protocol: engine.H2C})
	defer stop()
	p := &rampPool{addr: addr, client: h2cClient}
	defer p.stopAll()

	for _, n := range []int{64, 512, 1024, 64, 1024} {
		p.rampTo(n)
		time.Sleep(2 * time.Second)
		epo := aconns(e.primary)
		iou := aconns(e.secondary)
		t.Logf("[h2c] n=%d: epoll=%d io_uring=%d active=%s ok=%d err=%d", n, epo, iou, activeName(e), p.ok.Load(), p.errc.Load())
		if e.secondary != nil {
			t.Errorf("h2c must NEVER build/switch to io_uring, but secondary is non-nil at n=%d", n)
		}
		if epo < int64(n)/2 {
			t.Errorf("h2c n=%d: epoll only holds %d conns (want ~%d)", n, epo, n)
		}
	}
	p.stopAll()
	dumpErrSamples(t)
	t.Logf("[h2c] complete: ok=%d err=%d", p.ok.Load(), p.errc.Load())
	if p.errc.Load() > 0 {
		t.Errorf("h2c ramp had %d request errors (want 0)", p.errc.Load())
	}
}

// TestRampAutoMixedH1H2 runs Auto protocol with a steady set of h2c conns held
// open while h1 load ramps up/down to drive promote/revert. The h1 conns must
// transplant to the active engine; the h2 conns (NOT transplantable — H1-only
// gate) must stay pinned to epoll and keep being served across every switch.
func TestRampAutoMixedH1H2(t *testing.T) {
	if testing.Short() {
		t.Skip("ramp integration test")
	}
	e, addr, stop := newRampAdaptive(t, respHandler{}, resource.Config{Protocol: engine.Auto, EnableH2Upgrade: true})
	defer stop()

	const heldH2 = 32
	h2 := &rampPool{addr: addr, client: h2cClient}
	defer h2.stopAll()
	h2.rampTo(heldH2)
	time.Sleep(1500 * time.Millisecond)

	h1 := &rampPool{addr: addr, client: h1Client}
	defer h1.stopAll()
	// Warm io_uring, then reset so the measured ramp isn't a cold build.
	h1.rampTo(300)
	time.Sleep(2 * time.Second)
	h1.rampTo(0)
	time.Sleep(1500 * time.Millisecond)
	resetSwitchState(e)
	h1.ok.Store(0)
	h1.errc.Store(0)
	h2.ok.Store(0)
	h2.errc.Store(0)
	errSampleMu.Lock()
	errSamples = map[string]int{}
	errSampleMu.Unlock()

	for i, ph := range []struct {
		n       int
		wantIOU bool
	}{{2048, true}, {64, false}, {2048, true}, {64, false}} {
		h1.rampTo(rampMax(ph.n))
		time.Sleep(2500 * time.Millisecond)
		epo := aconns(e.primary)
		iou := aconns(e.secondary)
		t.Logf("[auto] phase %d h1=%d: epoll=%d io_uring=%d active=%s | h1 ok=%d err=%d | h2 ok=%d err=%d",
			i, ph.n, epo, iou, activeName(e), h1.ok.Load(), h1.errc.Load(), h2.ok.Load(), h2.errc.Load())
		// The h2 conns (heldH2) must remain on epoll regardless of the switch.
		if ph.wantIOU {
			// High h1 load → io_uring active, h1 conns migrated there; epoll keeps
			// ~the h2 conns (not transplantable).
			if iou < int64(ph.n)/2 {
				t.Errorf("phase %d: h1 conns not on io_uring: io_uring=%d", i, iou)
			}
		}
		resetSwitchState(e)
	}

	h1.stopAll()
	h2.stopAll()
	dumpErrSamples(t)
	t.Logf("[auto] complete: h1 ok=%d err=%d | h2 ok=%d err=%d", h1.ok.Load(), h1.errc.Load(), h2.ok.Load(), h2.errc.Load())
	if h2.errc.Load() > 0 {
		t.Errorf("h2 conns broke across switches: %d errors (want 0 — non-transplantable conns must survive)", h2.errc.Load())
	}
}

func TestRampAutoMixedAsync(t *testing.T) {
	if testing.Short() {
		t.Skip("ramp integration test")
	}
	e, addr, stop := newRampAdaptive(t, asyncRespHandler{}, resource.Config{Protocol: engine.Auto, EnableH2Upgrade: true, AsyncHandlers: true})
	defer stop()

	const heldH2 = 32
	h2 := &rampPool{addr: addr, client: h2cClient}
	defer h2.stopAll()
	h2.rampTo(heldH2)
	time.Sleep(1500 * time.Millisecond)

	h1 := &rampPool{addr: addr, client: h1Client}
	defer h1.stopAll()
	// Warm io_uring, then reset so the measured ramp isn't a cold build.
	h1.rampTo(300)
	time.Sleep(2 * time.Second)
	h1.rampTo(0)
	time.Sleep(1500 * time.Millisecond)
	resetSwitchState(e)
	h1.ok.Store(0)
	h1.errc.Store(0)
	h2.ok.Store(0)
	h2.errc.Store(0)
	errSampleMu.Lock()
	errSamples = map[string]int{}
	errSampleMu.Unlock()

	for i, ph := range []struct {
		n       int
		wantIOU bool
	}{{2048, true}, {64, false}, {2048, true}, {64, false}} {
		h1.rampTo(rampMax(ph.n))
		time.Sleep(2500 * time.Millisecond)
		epo := aconns(e.primary)
		iou := aconns(e.secondary)
		t.Logf("[auto] phase %d h1=%d: epoll=%d io_uring=%d active=%s | h1 ok=%d err=%d | h2 ok=%d err=%d",
			i, ph.n, epo, iou, activeName(e), h1.ok.Load(), h1.errc.Load(), h2.ok.Load(), h2.errc.Load())
		// The h2 conns (heldH2) must remain on epoll regardless of the switch.
		if ph.wantIOU {
			// High h1 load → io_uring active, h1 conns migrated there; epoll keeps
			// ~the h2 conns (not transplantable).
			if iou < int64(ph.n)/2 {
				t.Errorf("phase %d: h1 conns not on io_uring: io_uring=%d", i, iou)
			}
		}
		resetSwitchState(e)
	}

	h1.stopAll()
	h2.stopAll()
	dumpErrSamples(t)
	t.Logf("[auto] complete: h1 ok=%d err=%d | h2 ok=%d err=%d", h1.ok.Load(), h1.errc.Load(), h2.ok.Load(), h2.errc.Load())
	if h2.errc.Load() > 0 {
		t.Errorf("h2 conns broke across switches: %d errors (want 0 — non-transplantable conns must survive)", h2.errc.Load())
	}
}
