//go:build linux

package adaptive

import (
	"bufio"
	"context"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	tr := &http.Transport{
		Protocols: unencryptedHTTP2Only(),
		DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
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

// The ramp's load bands, celeris#631.
//
// The controller decides on CONNS PER WORKER — nothing it looks at is a
// connection count — so a phase written as an absolute number of connections
// only lands in the intended band on the machine it was written for. These
// tests were written for a 16-worker box, where the comment's arithmetic
// holds: 2048 conns = 128 cpw (promote), 128 conns = 8 cpw, under the 12 cpw
// revert edge. On a 4-worker container the SAME 128 conns are 32 cpw — above
// the 24 cpw PROMOTE threshold — so the controller is right to stay on
// io_uring and the low phases fail:
//
//	phase 1 (low load): conns not on epoll: epoll=0 io_uring=128 (want epoll ~128)
//
// measured on unmodified main, `--cpus 4`, for both TestRampH1Sync and
// TestRampH1Async (phases 1 and 5). That is the test's arithmetic being wrong
// for the box, not the engine misbehaving: the same run reverts correctly at
// phase 3 (1 conn = 0.25 cpw) and migrates every high phase.
//
// So the bands are derived from the controller's own thresholds and the
// engines' REAL worker counts, in the same run — the #625 treatment one level
// out: judge against the input the decision is actually made on.
const (
	// rampPeakConns is the historical peak. It is kept as a FLOOR rather than
	// a target so the transplant still runs at scale on small boxes, where it
	// is far above the promote band anyway.
	rampPeakConns = 2048
	// rampUpMargin/rampDownMargin place a phase clear of the edge it must be
	// on the far side of: the high phase at >= 1.25x upThreshold (30 cpw at
	// the shipped 24), the low phase at <= 0.5x downThreshold (6 cpw at the
	// shipped 12, which is what the original 128-on-16-workers gave).
	rampUpMargin   = 1.25
	rampDownMargin = 0.5
	// rampSettleBound replaces the flat 2.5 s sleep per phase as the VERDICT
	// deadline: the phase's outcome is polled and asserted as soon as it
	// holds, so a loaded box gets the time it needs instead of failing on a
	// wall clock. rampSettleDwell keeps the old 2.5 s as the measurement DWELL
	// floor rather than a deadline.
	rampSettleBound = 20 * time.Second
	rampSettleDwell = 2500 * time.Millisecond
	rampSettlePoll  = 50 * time.Millisecond

	// rampLowConnSeconds holds the run's REQUEST VOLUME fixed across boxes.
	//
	// The error budget at the bottom of rampScenario is a rate over the
	// requests the run served, and a phase's request volume is conns × dwell:
	// the original low phase contributed 128 conns for 2.5 s = 320
	// connection-seconds. Deriving the revert band from the worker count
	// (above) makes that band 24 conns on a 4-worker box, so at a flat dwell
	// the SAME absolute transplant residual — which is produced per migrated
	// connection, not per request — lands on a fifth of the denominator and
	// the RATE moves without the engine changing. Measured, --cpus 4, five
	// runs each of the same engine: the sync ramp's rate ran 0.0055-0.0577 %
	// on this branch against 0.0005-0.0229 % on main, and one run in five
	// crossed the 0.05 % budget on a residual main produces too.
	//
	// So a low phase runs long enough to contribute the sample the budget was
	// calibrated on and no longer: dwell = 320/conns seconds, floored at the
	// 2.5 s every phase already had and capped for sanity. On a 16-worker box
	// the band is 96-144 conns and the dwell stays at the floor, exactly as
	// before. Phases below rampDwellMinConns (the single-conn state check)
	// are left at the floor — no dwell makes one connection a traffic sample.
	//
	// The budget itself is untouched, and its form is worth revisiting on its
	// own: a per-migration failure mode judged as a fraction of requests is
	// a ratio against an unrelated denominator, which is why it moves with
	// the box. Not widened here.
	rampLowConnSeconds = 320
	rampDwellMax       = 12 * time.Second
	rampDwellMinConns  = 8
)

// phaseDwell is how long a phase must run before it is judged, so that its
// contribution to the run's error-rate denominator does not depend on how
// many workers the box has. See rampLowConnSeconds.
func phaseDwell(n int) time.Duration {
	if n < rampDwellMinConns {
		return rampSettleDwell
	}
	d := time.Duration(float64(rampLowConnSeconds)/float64(n)*float64(time.Second) + 0.5)
	return min(max(d, rampSettleDwell), rampDwellMax)
}

// rampBands holds one run's derived phase targets plus the worker counts they
// were derived from, so the result line can show its own arithmetic.
type rampBands struct {
	high, low, warm      int
	epollWorkers, iouWkr int
}

func engineWorkers(e engine.Engine) int {
	if e == nil {
		return 0
	}
	return max(e.Metrics().Workers, 0)
}

// rampBandsFor derives the phase targets from the controller's thresholds and
// the engines' worker counts. It runs BEFORE the warm-start (the high band and
// the warm load are needed to drive it) and reads epoll's worker count, which
// is real from bind; the lazy io_uring engine does not exist yet, so the
// revert band is provisional until refineLow re-derives it below. It skips
// rather than fails when CELERIS_RAMP_MAX caps the peak below the promote
// band: a cap that cannot drive a switch is an environment fact, and a ramp
// test that cannot ramp proves nothing either way.
func rampBandsFor(t *testing.T, e *Engine) rampBands {
	t.Helper()
	epw := engineWorkers(e.primary)
	if epw == 0 {
		t.Fatal("epoll engine reports 0 workers: nothing to derive the ramp bands from")
	}
	iow := engineWorkers(e.secondary)
	if iow == 0 {
		// The warm-start never built io_uring (or it reports no workers yet);
		// the epoll count is the best available estimate of the revert band.
		iow = epw
	}
	b := rampBands{epollWorkers: epw, iouWkr: iow}
	needUp := int(math.Ceil(e.ctrl.upThreshold * rampUpMargin * float64(epw)))
	peak := rampMax(rampPeakConns)
	if peak < needUp {
		t.Skipf("CELERIS_RAMP_MAX caps the peak at %d conns, but %d epoll workers need %d to reach %.0f conns/worker "+
			"(%.2fx the controller's %.0f upThreshold): the ramp could not drive a switch",
			peak, epw, needUp, e.ctrl.upThreshold*rampUpMargin, rampUpMargin, e.ctrl.upThreshold)
	}
	b.high = max(peak, needUp)
	b.warm = needUp
	b.low = max(1, int(float64(iow)*e.ctrl.downThreshold*rampDownMargin))
	if b.low >= b.high {
		t.Skipf("derived bands collapse on this box (low=%d high=%d, epoll_workers=%d io_uring_workers=%d)", b.low, b.high, epw, iow)
	}
	t.Logf("ramp bands (celeris#631): high=%d conns (%.1f cpw over %d epoll workers, up=%.0f watermark=%.0f), "+
		"low=%d conns (%.1f cpw over %d workers on the engine that will be active, down=%.0f)",
		b.high, float64(b.high)/float64(epw), epw, e.ctrl.upThreshold, e.ctrl.highWatermark,
		b.low, float64(b.low)/float64(iow), iow, e.ctrl.downThreshold)
	return b
}

// refineLow re-derives the revert band once the lazy io_uring engine exists,
// since it is io_uring's worker count the controller divides by while
// io_uring is the active engine. Called after the warm-start promote.
func (b *rampBands) refineLow(t *testing.T, e *Engine) {
	t.Helper()
	iow := engineWorkers(e.secondary)
	if iow == 0 || iow == b.iouWkr {
		t.Logf("ramp bands final: high=%d low=%d (io_uring workers=%d, unchanged)", b.high, b.low, b.iouWkr)
		return
	}
	b.iouWkr = iow
	b.low = max(1, int(float64(iow)*e.ctrl.downThreshold*rampDownMargin))
	t.Logf("ramp bands final: high=%d low=%d (%.1f cpw over the %d io_uring workers the warm-start built, down=%.0f)",
		b.high, b.low, float64(b.low)/float64(iow), iow, e.ctrl.downThreshold)
}

// phaseHolds is the phase's claim, read from the engines' own metrics: the
// conns are on the engine the controller should have moved them to, and none
// were lost. It is both the wait predicate and (after the bound) the
// assertion, so the test asserts exactly what it waited for.
func phaseHolds(epo, iou int64, want int, wantIOU bool) bool {
	if epo+iou < int64(want)/2 {
		return false
	}
	if wantIOU {
		return iou >= phaseQuorum(want)
	}
	return epo >= phaseQuorum(want)
}

// phaseQuorum is the "vast majority of conns" bar: three quarters, but never
// zero. int64(1)*3/4 is 0, so the single-conn phase used to assert `epo >= 0`
// — vacuously true with the conn still on the standby, which the drain-
// disabled control proved by passing that phase at epoll=0 io_uring=1.
func phaseQuorum(want int) int64 {
	return max(1, int64(want)*3/4)
}

// clearOscillationLock drops the anti-thrash lock — and ONLY that: the tick
// counters and lastSwitch are left alone, so a sustained-load switch still has
// to earn its sustainTicks.
//
// The rig already declares the 3-switches-in-5-minutes lock out of scope and
// resets it between phases ("isolates the TRANSPLANT mechanism from that
// throttle"), but a phase that ramps ACROSS the thresholds can burn the whole
// switch budget inside itself: measured on this branch, the 24 → 2048 ramp
// promoted at 536 conns, the drain's own residual errors tripped the
// always-on error-rate revert (error_rate=0.131 over the 14 conns still on
// the engine being drained), it promoted again at 818, and the third switch
// locked the controller for five minutes — freezing 703 conns on the standby
// with no switch left to re-drain them. Unlocking during the wait keeps the
// phase measuring the transplant instead of the throttle.
func clearOscillationLock(e *Engine) bool {
	e.switchMu.Lock()
	defer e.switchMu.Unlock()
	if !e.ctrl.state.locked {
		return false
	}
	e.ctrl.state.locked = false
	e.ctrl.state.lockUntil = time.Time{}
	e.ctrl.state.switchCount = 0
	e.ctrl.state.switchIdx = 0
	return true
}

// awaitPhase polls the two engines until the phase's claim holds or the bound
// expires, and reports the final counts. Replaces a flat sleep: the verdict is
// then the engines' state, not the wall clock a shared box happened to need.
func awaitPhase(e *Engine, want int, wantIOU bool) (epo, iou int64, waited time.Duration, unlocks int) {
	start := time.Now()
	dwell := phaseDwell(want)
	for {
		epo, iou = aconns(e.primary), aconns(e.secondary)
		waited = time.Since(start)
		if waited >= dwell && phaseHolds(epo, iou, want, wantIOU) {
			return epo, iou, waited, unlocks
		}
		if waited >= max(rampSettleBound, dwell) {
			return epo, iou, waited, unlocks
		}
		if clearOscillationLock(e) {
			unlocks++
		}
		time.Sleep(rampSettlePoll)
	}
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
	// The warm load is the controller's own promote band (celeris#631), not a
	// flat 300 conns: 300 is 75 conns/worker on a 4-worker box and 4.7 on a
	// 64-worker one, and at 4.7 the warm-start silently promotes nothing and
	// leaves the cold ring build inside the first measured phase.
	bands := rampBandsFor(t, e)
	p.rampTo(bands.warm)
	time.Sleep(2 * time.Second)
	p.rampTo(0)
	time.Sleep(1500 * time.Millisecond)
	resetSwitchState(e)
	p.ok.Store(0)
	p.errc.Store(0)
	errSampleMu.Lock()
	errSamples = map[string]int{}
	errSampleMu.Unlock()
	// io_uring exists now, so its real worker count fixes the revert band.
	bands.refineLow(t, e)

	// high → low → high → 1 → high → low: each high phase should land on
	// io_uring with conns migrated there; each low phase back on epoll. The
	// bands are the controller's own (celeris#631: derived from its
	// thresholds and the engines' real worker counts, not the 2048/128 pair
	// that only means "promote"/"revert" on a 16-worker box), and the
	// oscillation lock is reset between phases so we test the transplant
	// across many cycles, not the anti-thrash throttle.
	phases := []struct {
		n       int
		wantIOU bool // expect io_uring active (conns migrated to io_uring)
	}{
		{bands.high, true},
		{bands.low, false},
		{bands.high, true},
		{1, false},
		{bands.high, true},
		{bands.low, false},
	}

	for i, ph := range phases {
		want := ph.n
		p.rampTo(want)
		// Poll the engines' own metrics instead of sleeping a fixed 2.5 s:
		// the phase is judged on the state it reaches, and the time it took
		// is reported rather than asserted. e.secondary (io_uring) is built
		// lazily on the first promote — nil before then; aconns guards it.
		epo, iou, waited, unlocks := awaitPhase(e, want, ph.wantIOU)
		promoted := apromoted(e.secondary)
		workers := bands.epollWorkers
		if ph.wantIOU {
			workers = bands.iouWkr
		}
		t.Logf("[async=%v] phase %d n=%d (%.1f cpw over %d workers): epoll=%d io_uring=%d (promoted=%d) ok=%d err=%d settled_in=%v osc_unlocks=%d",
			async, i, want, float64(want)/float64(max(workers, 1)), workers, epo, iou, promoted, p.ok.Load(), p.errc.Load(), waited.Round(time.Millisecond), unlocks)

		// The active engine should hold the vast majority of conns; the standby
		// should be ~drained (migration completed). Allow slack for in-flight.
		total := epo + iou
		if total < int64(want)/2 {
			t.Errorf("phase %d: lost conns: total=%d want ~%d after %v", i, total, want, waited)
		}
		if ph.wantIOU {
			if iou < phaseQuorum(want) {
				t.Errorf("phase %d (high load, %.1f conns/worker >= %.0f upThreshold): conns not on io_uring after %v: io_uring=%d epoll=%d (want io_uring ~%d)",
					i, float64(want)/float64(max(bands.epollWorkers, 1)), e.ctrl.upThreshold, waited, iou, epo, want)
			}
		} else {
			if epo < phaseQuorum(want) {
				t.Errorf("phase %d (low load, %.1f conns/worker < %.0f downThreshold): conns not on epoll after %v: epoll=%d io_uring=%d (want epoll ~%d)",
					i, float64(want)/float64(max(bands.iouWkr, 1)), e.ctrl.downThreshold, waited, epo, iou, want)
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

// unencryptedHTTP2Only makes an http.Transport speak prior-knowledge
// cleartext HTTP/2 (h2c) and nothing else, the stdlib replacement for the
// deprecated http2.Transport{AllowHTTP: true} shape.
func unencryptedHTTP2Only() *http.Protocols {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	return p
}
