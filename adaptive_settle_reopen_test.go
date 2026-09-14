package celeris

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRouteAdaptive_SettledRouteIsReopened covers the celeris#592 mechanism at
// the router level: settling is no longer terminal. A settled route returned to
// the timed path by the re-opener promotes on its first blocking run, and a
// route that is still fast re-settles on its very next run — which is what
// bounds the re-timing cost to one timed inline run per route per
// adaptiveSettleTTL.
func TestRouteAdaptive_SettledRouteIsReopened(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	s.GET("/s", noopHandler)
	rt := s.router

	for i := 0; i < adaptiveSettleStreak; i++ {
		rt.recordInlineRun("/s", false)
	}
	if rt.adaptiveLearning("/s") {
		t.Fatalf("route must settle after %d consecutive fast runs", adaptiveSettleStreak)
	}

	// Re-open: the route is timed again (this is the run that catches a
	// backend that has since turned slow).
	rt.reopenSettled()
	if !rt.adaptiveLearning("/s") {
		t.Fatal("reopenSettled must return a settled route to the timed path")
	}

	// Still fast → re-settles on the single re-timed run, because the fast
	// streak is deliberately preserved across the re-open.
	rt.recordInlineRun("/s", false)
	if rt.adaptiveLearning("/s") {
		t.Fatal("a still-fast route must re-settle on its first re-timed run (fast streak preserved)")
	}

	// Backend turned slow: the re-timed run is over adaptiveBlockingThreshold,
	// so handler.go calls promoteRouteImmediate and the route goes async.
	rt.reopenSettled()
	if !rt.adaptiveLearning("/s") {
		t.Fatal("reopenSettled must return the re-settled route to the timed path")
	}
	rt.promoteRouteImmediate("/s")
	if !rt.isPromoted("/s") {
		t.Fatal("a settled route that turns blocking must promote once re-timed")
	}
	if !rt.routeAsync("GET", "/s") {
		t.Fatal("a promoted route must dispatch async")
	}
}

// TestRouteAdaptive_SettleReopenerLifecycle verifies that the re-opener
// goroutine is started only when there are adaptive routes, actually clears the
// settled set on its tick, and is stopped (no goroutine left behind) by
// stopSettleReopener. Start/stop are both idempotent.
func TestRouteAdaptive_SettleReopenerLifecycle(t *testing.T) {
	// No adaptive routes (AsyncHandlers=false) → no goroutine at all.
	plain := New(Config{})
	plain.GET("/p", noopHandler)
	before := runtime.NumGoroutine()
	plain.router.startSettleReopener(time.Millisecond)
	if plain.router.reopenStop != nil {
		t.Fatal("a server with no adaptive routes must not start the re-opener")
	}
	plain.router.stopSettleReopener() // idempotent on a never-started router

	s := New(Config{AsyncHandlers: true})
	s.GET("/s", noopHandler)
	rt := s.router
	for i := 0; i < adaptiveSettleStreak; i++ {
		rt.recordInlineRun("/s", false)
	}
	if rt.adaptiveLearning("/s") {
		t.Fatal("precondition: route must be settled")
	}

	rt.startSettleReopener(time.Millisecond)
	rt.startSettleReopener(time.Millisecond) // idempotent: still one goroutine

	deadline := time.Now().Add(5 * time.Second)
	for !rt.adaptiveLearning("/s") {
		if time.Now().After(deadline) {
			t.Fatal("the re-opener did not clear the settled set within 5 s")
		}
		time.Sleep(time.Millisecond)
	}

	rt.stopSettleReopener()
	rt.stopSettleReopener() // idempotent
	if rt.reopenStop != nil {
		t.Fatal("stopSettleReopener must clear the stop channel")
	}
	// The goroutine exits on the stop channel; give it a moment and check we
	// are back to the baseline count.
	for i := 0; i < 200 && runtime.NumGoroutine() > before; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines after stop = %d, want <= %d (re-opener leaked)", after, before)
	}
}

// TestRouteAdaptive_FastStreakClampedAcrossReopens covers the second binding
// review correction on celeris#592: the fast streak must be CLAMPED at
// adaptiveSettleStreak, not incremented without bound.
//
// Before the re-opener existed the counter stopped growing on its own — the
// run that first reached adaptiveSettleStreak settled the route, the route
// left the timed path and recordInlineRun was never called for it again. The
// re-opener removes that natural ceiling: every tick returns the route to the
// timed path, so every tick adds at least one more increment for the life of
// the process. Unclamped that is an int32 walking towards MaxInt32, and the
// moment it wraps negative `Add(1) >= adaptiveSettleStreak` stops holding and
// the route can NEVER settle again — the fix would have permanently
// re-introduced the per-request timing celeris#361 removed.
//
// The invariant asserted here is the one that makes the wrap unreachable:
// 0 <= fastStreak <= adaptiveSettleStreak, across arbitrarily many cycles.
func TestRouteAdaptive_FastStreakClampedAcrossReopens(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	s.GET("/s", noopHandler)
	rt := s.router

	for i := 0; i < adaptiveSettleStreak; i++ {
		rt.recordInlineRun("/s", false)
	}
	if rt.adaptiveLearning("/s") {
		t.Fatalf("precondition: route must settle after %d fast runs", adaptiveSettleStreak)
	}
	streak := func() int32 {
		v, ok := rt.fastStreak.Load("/s")
		if !ok {
			t.Fatal("fast streak entry missing")
		}
		return v.(*atomic.Int32).Load()
	}
	if got := streak(); got != adaptiveSettleStreak {
		t.Fatalf("fast streak after settling = %d, want %d (clamped at the threshold)", got, adaptiveSettleStreak)
	}

	// Many settle/re-open cycles. Each one re-times the route (one gate-open
	// window) and re-settles it on the next fast run; the counter must not
	// move. Unclamped this ends at adaptiveSettleStreak+cycles.
	const cycles = 10000
	for i := 0; i < cycles; i++ {
		rt.reopenSettled()
		if !rt.adaptiveLearning("/s") {
			t.Fatalf("cycle %d: reopenSettled must return the route to the timed path", i)
		}
		rt.recordInlineRun("/s", false)
		if rt.adaptiveLearning("/s") {
			t.Fatalf("cycle %d: a still-fast route must re-settle on its first re-timed run", i)
		}
		if got := streak(); got != adaptiveSettleStreak {
			t.Fatalf("cycle %d: fast streak = %d, want %d (clamp lost: the counter is growing per re-open)",
				i, got, adaptiveSettleStreak)
		}
	}

	// Several runs inside ONE re-open window (the real shape: every request
	// already in flight when the settled set is cleared is timed) must not
	// push it past the clamp either.
	rt.reopenSettled()
	for i := 0; i < 1000; i++ {
		rt.recordInlineRun("/s", false)
	}
	if got := streak(); got != adaptiveSettleStreak {
		t.Fatalf("fast streak after 1000 runs in one window = %d, want %d", got, adaptiveSettleStreak)
	}
	// The clamp must not break the classifier: a slow run still zeroes the
	// streak and a blocking route still promotes.
	rt.recordInlineRun("/s", true)
	if got := streak(); got != 0 {
		t.Fatalf("fast streak after a slow run = %d, want 0", got)
	}
}

// TestRouteAdaptive_NoReopenerWhenEngineCreationFails covers the first binding
// review correction on celeris#592: start the ticker only after the engine is
// created and published.
//
// A failed createEngine leaves doPrepare with startErr set and no engine ever
// stored, so the caller gets an error instead of a running Server and never
// calls Shutdown — which is the only thing that stops the re-opener. Started
// before createEngine, the ticker goroutine would outlive the failed Start for
// the life of the process, holding the router alive with it.
//
// The failure is forced with an EngineType the switch in createEngine does not
// know: it passes Config.Validate (which only rejects the Linux-only engines
// off Linux) and fails in createEngine itself, which is exactly the edge the
// correction is about. The second half is the discriminator: on a server whose
// engine DOES come up, the same assertions must find the re-opener running, so
// a test that simply never starts it cannot pass.
func TestRouteAdaptive_NoReopenerWhenEngineCreationFails(t *testing.T) {
	// reopenStop is written under reopenMu by doPrepare on the Start
	// goroutine, so the test reads it under the same lock (a bare field read
	// would be a data race under -race in the discriminator half below).
	reopenerStarted := func(rt *router) bool {
		rt.reopenMu.Lock()
		defer rt.reopenMu.Unlock()
		return rt.reopenStop != nil
	}
	// One "created by ...startSettleReopener" line per LIVE re-opener
	// goroutine in the all-goroutine dump — counting bare occurrences of the
	// name would count each goroutine twice (its frame and its created-by
	// line) and report 2 for a single leak.
	reopenerGoroutines := func() int {
		buf := make([]byte, 1<<20)
		for {
			n := runtime.Stack(buf, true)
			if n >= len(buf) {
				buf = make([]byte, 2*len(buf))
				continue
			}
			live := 0
			for _, line := range strings.Split(string(buf[:n]), "\n") {
				if strings.HasPrefix(line, "created by ") && strings.Contains(line, "startSettleReopener") {
					live++
				}
			}
			return live
		}
	}

	// An adaptive route exists, so startSettleReopener would start a ticker if
	// it were reached.
	bad := New(Config{Engine: EngineType(99), AsyncHandlers: true})
	bad.GET("/s", noopHandler)
	if !bad.router.adaptiveRoutes["/s"] {
		t.Fatal("precondition: /s must be adaptive (otherwise the re-opener never starts and the test is vacuous)")
	}
	err := bad.Start()
	if err == nil {
		t.Fatal("precondition: Start must fail for an unknown engine type")
	}
	if !strings.Contains(err.Error(), "create engine") {
		t.Fatalf("Start error = %v, want a create-engine failure (the test must exercise the createEngine path)", err)
	}
	if reopenerStarted(bad.router) {
		t.Error("a Server whose engine creation failed left the settle re-opener running (reopenStop != nil)")
	}
	if n := reopenerGoroutines(); n != 0 {
		t.Errorf("%d settle-re-opener goroutine(s) alive after a failed Start, want 0 (leaked: nothing will ever stop them)", n)
	}

	// Discriminator: the same checks on a server that starts must find it.
	okSrv := New(Config{Engine: Std, AsyncHandlers: true})
	okSrv.GET("/s", noopHandler)
	ln, lnErr := net.Listen("tcp", "127.0.0.1:0")
	if lnErr != nil {
		t.Fatalf("listen: %v", lnErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- okSrv.StartWithListenerAndContext(ctx, ln) }()
	deadline := time.Now().Add(10 * time.Second)
	for !reopenerStarted(okSrv.router) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !reopenerStarted(okSrv.router) {
		t.Fatal("discriminator: a server that started must have the re-opener running (the leak assertions above would pass vacuously)")
	}
	if n := reopenerGoroutines(); n == 0 {
		t.Fatal("discriminator: no re-opener goroutine found on a started server (the stack scan does not detect it)")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("server did not exit within 30 s of cancel")
	}
	for i := 0; i < 500 && reopenerGoroutines() > 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if n := reopenerGoroutines(); n != 0 {
		t.Errorf("%d settle-re-opener goroutine(s) alive after shutdown, want 0", n)
	}
}

// TestRouteAdaptive_SettleReopenCost MEASURES what a re-open actually costs —
// the third binding review correction on celeris#592. The original doc claimed
// "exactly ONE timed inline run per adaptive route per adaptiveSettleTTL", and
// that is wrong: clearing the settled set opens a gate that stays open until
// the FIRST re-timed run returns and stores `settled` again, so EVERY inline
// run that passes the gate in that window is timed, not just one.
//
// The window is one handler run long, and an inline (non-promoted) handler run
// occupies its engine worker for the whole run, so that worker's next request
// cannot start until the current one has finished — by which time the route
// has re-settled. The per-tick cost is therefore bounded by the number of
// inline handlers running CONCURRENTLY (the engine's worker count on the
// native engines), not by the request rate and not by one.
//
// The rig replicates handler.go's dispatch gate verbatim
// (`rt.adaptiveRoutes[p] && rt.adaptiveLearning(p)` → time the run →
// promoteRouteImmediate / recordInlineRun, handler.go:141-155) on `runners`
// goroutines that never idle — the saturated-worker case, where the cost is
// highest. Timed runs are counted at the gate, so the counts are exact.
//
// Three quantities are measured per case and combined to get the amortized
// figure the doc comment quotes:
//
//	T(K)  timed runs per re-open at K concurrent runners (counted, exact);
//	R     steady-state runs/s with the gate closed, measured with no re-opens;
//	C     the extra cost of one timed run over a settled one (two time.Now()
//	      calls plus recordInlineRun), measured single-threaded.
//
// The real amortized cost of the re-opener is then T(K)·C per route per
// adaptiveSettleTTL, and the fraction of requests that pay the timing is
// T(K)/(R·adaptiveSettleTTL) — neither of which is observable by re-opening in
// a tight loop, so the two rates are measured separately rather than inferred
// from this test's own (artificially fast) re-open cadence.
func TestRouteAdaptive_SettleReopenCost(t *testing.T) {
	if testing.Short() {
		t.Skip("celeris#592 cost measurement saturates every core for a few seconds; -short skips it")
	}
	const (
		path      = "/s"
		reopens   = 200
		spacing   = 200 * time.Microsecond // settled operation between re-opens, so runners hit the gate at a random phase
		rateWin   = 300 * time.Millisecond // steady-state throughput window (gate closed)
		overheadN = 300000
	)
	// sink keeps the measured work from being optimised away.
	var sink atomic.Int64
	work := func() { sink.Add(1) }

	newSettled := func(t *testing.T) *router {
		t.Helper()
		s := New(Config{AsyncHandlers: true})
		s.GET(path, noopHandler)
		rt := s.router
		for i := 0; i < adaptiveSettleStreak; i++ {
			rt.recordInlineRun(path, false)
		}
		if rt.adaptiveLearning(path) {
			t.Fatal("precondition: the route must be settled before the measurement")
		}
		return rt
	}

	// C: the extra work one timed run does that a settled run does not.
	// Measured on an already-settled route, so recordInlineRun takes exactly
	// the path a re-timed fast run takes.
	rtC := newSettled(t)
	t0 := time.Now()
	for i := 0; i < overheadN; i++ {
		start := time.Now()
		work()
		dur := time.Since(start)
		rtC.recordInlineRun(path, dur > adaptivePromoteThreshold)
	}
	timedCost := time.Since(t0)
	t0 = time.Now()
	for i := 0; i < overheadN; i++ {
		work()
	}
	baseCost := time.Since(t0)
	perTimedRun := (timedCost - baseCost) / overheadN

	type result struct {
		runners             int
		timed               int64
		maxTimed            int64
		slowRuns            int64
		blockingRuns        int64
		perReopen           float64
		ratePerSec          float64
		timedFracOfRequests float64
		amortizedPerTTL     time.Duration
	}
	var results []result

	for _, runners := range []int{1, 2, 4, 8} {
		rt := newSettled(t)
		var timedRuns, totalRuns, slowRuns, blockingRuns atomic.Int64
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < runners; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					// handler.go:141-155, verbatim.
					if rt.adaptiveRoutes[path] && rt.adaptiveLearning(path) {
						timedRuns.Add(1)
						start := time.Now()
						work()
						dur := time.Since(start)
						if dur > adaptiveBlockingThreshold {
							// Scheduler jitter over 2 ms. This ZEROES the fast
							// streak (promoteRouteImmediate does), so the route
							// then needs adaptiveSettleStreak more fast runs to
							// re-settle and the re-open window stays open for
							// all of them — counted so the bound below is not
							// asserted against a window that a jitter spike,
							// not the mechanism, held open.
							blockingRuns.Add(1)
							rt.promoteRouteImmediate(path)
						} else {
							if dur > adaptivePromoteThreshold {
								slowRuns.Add(1)
							}
							rt.recordInlineRun(path, dur > adaptivePromoteThreshold)
						}
					} else {
						work()
					}
					totalRuns.Add(1)
				}
			}()
		}

		settled := func() bool { _, ok := rt.settled.Load(path); return ok }
		waitSettled := func(what string) {
			deadline := time.Now().Add(30 * time.Second)
			for !settled() {
				if time.Now().After(deadline) {
					close(stop)
					wg.Wait()
					t.Fatalf("runners=%d: %s: the route did not re-settle within 30 s", runners, what)
				}
				runtime.Gosched()
			}
		}
		waitSettled("before the first re-open")

		// R: steady-state throughput with the gate CLOSED — the denominator of
		// the amortized fraction. Measured before any re-open so not a single
		// run in this window is timed.
		rateStart := totalRuns.Load()
		rateT0 := time.Now()
		time.Sleep(rateWin)
		ratePerSec := float64(totalRuns.Load()-rateStart) / time.Since(rateT0).Seconds()
		if timedRuns.Load() != 0 {
			t.Fatalf("runners=%d: %d timed runs before any re-open: the route did not stay settled", runners, timedRuns.Load())
		}

		// T(K): timed runs per re-open, counted exactly at the gate.
		var maxTimed int64
		for i := 0; i < reopens; i++ {
			before := timedRuns.Load()
			rt.reopenSettled()
			waitSettled(fmt.Sprintf("re-open %d", i))
			if d := timedRuns.Load() - before; d > maxTimed {
				maxTimed = d
			}
			time.Sleep(spacing)
		}
		gotTimed := timedRuns.Load()
		close(stop)
		wg.Wait()

		r := result{
			runners:      runners,
			timed:        gotTimed,
			maxTimed:     maxTimed,
			slowRuns:     slowRuns.Load(),
			blockingRuns: blockingRuns.Load(),
			perReopen:    float64(gotTimed) / float64(reopens),
			ratePerSec:   ratePerSec,
		}
		r.amortizedPerTTL = time.Duration(r.perReopen * float64(perTimedRun.Nanoseconds()))
		if ratePerSec > 0 {
			r.timedFracOfRequests = r.perReopen / (ratePerSec * adaptiveSettleTTL.Seconds())
		}
		results = append(results, r)
		t.Logf("MEASURE592 runners=%d reopens=%d timed_runs=%d timed_per_reopen=%.2f max_timed_in_one_reopen=%d "+
			"slow_classified=%d blocking_classified=%d steady_runs_per_s=%.0f per_timed_run_overhead_ns=%d "+
			"amortized_ns_per_route_per_ttl=%d timed_per_1e9_requests=%.1f gomaxprocs=%d",
			r.runners, reopens, r.timed, r.perReopen, r.maxTimed, r.slowRuns, r.blockingRuns, r.ratePerSec,
			perTimedRun.Nanoseconds(), r.amortizedPerTTL.Nanoseconds(), r.timedFracOfRequests*1e9, runtime.GOMAXPROCS(0))
	}

	// Property 1: the count per re-open is bounded by the number of
	// CONCURRENT inline runs, not by the process and not by the request rate.
	// Slack of 1 absorbs the run already past the gate when the settled store
	// lands.
	//
	// A run that scheduler jitter classifies slow zeroes the fast streak
	// (recordInlineRun on the slow branch, or promoteRouteImmediate over 2 ms),
	// and the route then needs adaptiveSettleStreak fast runs to re-settle, so
	// that one window legitimately stays open for ~256 runs — measured at
	// max_timed_in_one_reopen=258 twice in five -race runs at 8 runners on 4
	// CPUs. That is jitter, not the mechanism, so such a case is REPORTED and
	// the bound is not asserted on it. Both classifications are counted: an
	// earlier version of this guard watched only the 300µs branch and missed
	// the 2 ms one, which made the test intermittently fail under -race.
	for _, r := range results {
		if r.slowRuns > 0 || r.blockingRuns > 0 {
			t.Logf("MEASURE592 runners=%d: %d slow / %d blocking jitter classification(s) zeroed the fast streak and held one gate open "+
				"(max_timed_in_one_reopen=%d); the bound is reported, not asserted, for this case",
				r.runners, r.slowRuns, r.blockingRuns, r.maxTimed)
			continue
		}
		if r.perReopen < 1 {
			t.Errorf("runners=%d: %.2f timed runs per re-open, want >= 1: the re-open did not re-time the route at all",
				r.runners, r.perReopen)
		}
		if r.perReopen > float64(r.runners)+1 {
			t.Errorf("runners=%d: %.2f timed runs per re-open, want <= %d+1: the re-open window is not closing on the first re-timed run",
				r.runners, r.perReopen, r.runners)
		}
		if r.maxTimed > int64(r.runners)+1 {
			t.Errorf("runners=%d: %d timed runs in a single re-open, want <= %d+1", r.runners, r.maxTimed, r.runners)
		}
	}
	// Property 2: at the real tick rate it IS amortized away. The bar sits four
	// orders of magnitude above every value observed (~2.4e-7), not next to
	// them, on purpose: the denominator is this rig's own synthetic loop rate,
	// which collapses several-fold under -race or on a loaded box, and a bar at
	// 1e-6 made that an intermittent failure. The concurrency bound above is
	// the load-bearing property; this one only has to catch a re-opener that
	// re-times per REQUEST rather than per tick, which would read ~1.
	for _, r := range results {
		if r.timedFracOfRequests > 1e-4 {
			t.Errorf("runners=%d: %.3g of requests pay the re-timing (%.2f timed runs per re-open at %.0f req/s), want < 1e-4",
				r.runners, r.timedFracOfRequests, r.perReopen, r.ratePerSec)
		}
	}
}
