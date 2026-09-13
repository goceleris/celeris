//go:build linux

package celeris

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/middleware/store"
)

// stackDumped589 guards the one-shot full-stack dump of the CELERIS_589_STACK
// diagnostic; the per-stall tally (stackTally589) runs on every stalled sample.
var stackDumped589 atomic.Bool

// stackTally589 classifies every engine event-loop goroutine at the moment a
// /ping sample has been outstanding for 100 ms, so the site that pins the
// worker is COUNTED across the window rather than named once. The io_uring
// worker is the goroutine running (*Worker).run; the epoll one (*Loop).run.
type stackTally589 struct {
	captures int            // stalled samples at which a stack was taken
	loops    int            // event-loop goroutines seen across captures
	where    map[string]int // event-loop goroutine → blocking site
}

func classifyLoopStack589(g string) string {
	switch {
	case strings.Contains(g, "checkTimeouts") && strings.Contains(g, "sync.(*Mutex).Lock"):
		return "checkTimeouts_detachMu_Lock"
	case strings.Contains(g, "sync.(*Mutex).Lock"):
		return "other_Mutex_Lock"
	case strings.Contains(g, "SubmitAndWait") || strings.Contains(g, "EpollWait"):
		return "kernel_wait" // io_uring_enter / epoll_wait: the loop is idle, not pinned
	case strings.Contains(g, "ProcessH1") || strings.Contains(g, "HandleStream"):
		return "inline_handler"
	}
	// Fallback: the first celeris frame's function name (package.Recv.Method).
	for _, ln := range strings.Split(g, "\n")[1:] {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "github.com/goceleris/celeris") {
			continue
		}
		if i := strings.Index(ln, "(0x"); i > 0 {
			return ln[:i]
		}
		if i := strings.Index(ln, "({"); i > 0 {
			return ln[:i]
		}
		return strings.TrimSuffix(ln, "()")
	}
	return "unclassified"
}

func (st *stackTally589) capture(t *testing.T) {
	t.Helper()
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	st.captures++
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		isLoop := strings.Contains(g, "engine/iouring.(*Worker).run(") || strings.Contains(g, "engine/epoll.(*Loop).run(")
		if !isLoop {
			continue
		}
		st.loops++
		if st.where == nil {
			st.where = map[string]int{}
		}
		st.where[classifyLoopStack589(g)]++
		if stackDumped589.CompareAndSwap(false, true) {
			t.Logf("STACK589 first stalled sample, event-loop goroutine:\n%s", g)
		}
	}
}

func (st *stackTally589) String() string {
	keys := make([]string, 0, len(st.where))
	for k := range st.where {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, st.where[k]))
	}
	return fmt.Sprintf("captures=%d loop_goroutines=%d %s", st.captures, st.loops, strings.Join(parts, " "))
}

// envInt589 reads a positive integer diagnostic override or returns def.
func envInt589(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// Regression rig for celeris#592 (the fix) — measurement rig for celeris#589
// (celeris#493 fix-plan item (4)), with the `settled` subtest INVERTED.
//
// Defect that was measured (celeris#589, 20 runs per engine on celeris ededb6c): an adaptive route (inherited AsyncHandlers=true, no
// explicit .Async()) that SETTLED as fast (adaptiveSettleStreak consecutive
// sub-300µs inline runs) is never re-timed — handler.go only times a route
// while router.adaptiveLearning() is true, and the only statement that removes
// a route from `settled` is an explicit .Async()/.Sync() at registration. So a
// store-backed handler that later turns slow keeps running inline on the
// engine worker thread for every request, and an unrelated fast request on the
// same worker (/ping) queues behind the blocked store call.
//
// The fix (celeris#592) makes settling non-terminal: a background re-opener
// clears the settled set every adaptiveSettleTTL, so the route is re-timed, the
// re-timed run is ~D (far over adaptiveBlockingThreshold) and it promotes. The
// `settled` subtest therefore asserts the INVERSE of the #589 claim:
//
//   - STATE (both engines): isPromoted("/kv") is true and
//     EngineMetrics.AsyncPromotedConns >= 1, reached within stall592PromoteBound
//     of the store turning slow;
//   - LATENCY (epoll only): fewer than stall592MaxStalledFrac of the /ping
//     samples taken AFTER promotion exceed stall589StallBar.
//
// io_uring's fraction is PRINTED, not asserted: even the fully async control
// pins the io_uring worker ~30% of wall time through the celeris#593 timeout
// sweep (see the note below), which is a separate defect with its own fix.
//
// The observable follows the two binding adversarial-review corrections on the
// issue: the PRIMARY assertion is the dispatch STATE after the route has turned
// slow (settled.Load, isPromoted, EngineMetrics.AsyncPromotedConns); the
// latency observable is the FRACTION of /ping samples above 5 ms taken over a
// window that starts only after every /kv connection has completed one slow run
// (so the unavoidable first post-hoc inline run per worker is excluded) and,
// in the settled mode, only after the route has been promoted, and the
// promotion TTL is pinned out of the picture with the existing nowNano test
// hook (frozen clock) so the promotion made during the run cannot expire. The
// re-opener runs off a real time.Ticker, so the frozen clock does not stall it.
//
// Negative controls (the same rig, the same blocked Set):
//   - explicit .Async() on /kv (item (4)'s "store-backed middleware blocking by
//     default" flavour): the route is never adaptive, every /kv conn is handed
//     to the dispatch goroutine, the worker stays free;
//   - /kv still LEARNING (warmed with fewer than adaptiveSettleStreak runs): the
//     first slow inline run promotes the route immediately, later runs go async.
//
// The #589 claim assertion (assertSettledStall589 — the DEFECT's signature)
// must now FAIL on all three modes, and the fixed-behaviour assertion
// (assertSettledRetimed592) must PASS on the settled rig; the two controls are
// unchanged from the measurement branch and still assert their inverse state.
// A test that passes on both trees proves nothing, so both directions are
// asserted explicitly: on origin/main the settled subtest FAILS because the
// route is never promoted.
//
// Latency observable per engine (measured while building this rig): on epoll
// the controls fully invert (0 of ~940 /ping samples above 5 ms). On io_uring
// they invert in the MEDIAN (sub-ms vs ≥ D) but NOT in the stalled fraction:
// the worker still blocks for ≈ D − 30 ms of every D cycle even though every
// /kv conn is on a dispatch goroutine. A goroutine stack captured mid-stall
// (CELERIS_589_STACK=1) shows both workers in sync.Mutex.Lock inside
// Worker.checkTimeouts (engine/iouring/worker.go:4186, the celeris#548
// h1State snapshot under detachMu) while runAsyncHandler holds cs.detachMu
// across the whole ProcessH1 (worker.go:3307→3460). The epoll sweep takes no
// lock. That is a distinct io_uring defect — an async-dispatched HTTP/1 conn
// with a slow handler pins the worker in the timeout sweep — not the item (4)
// dispatch policy, so the controls assert the median and report the fraction,
// and log an ANOMALY589 line whenever a control's fraction exceeds 5 %.
//
// Diagnostics (env, test-only): CELERIS_589_STACK=1 tallies the event-loop
// goroutines' blocking site 100 ms into every stalled /ping (STACKTALLY589);
// CELERIS_589_DUMP=1 logs the /ping latency series; CELERIS_589_FRESH=1 samples
// on a fresh conn per request; CELERIS_589_DELAY_MS, CELERIS_589_WORKERS and
// CELERIS_589_KVCONNS override D, the worker count and the slow-conn count
// (KVCONNS=1 with 2 workers makes the stall bimodal across runs if — and only
// if — it is local to the worker that owns the sleeping async conn).
const (
	stall589Delay    = 300 * time.Millisecond // D: injected per-call Set latency (CELERIS_589_DELAY_MS overrides)
	stall589Workers  = 2                      // CELERIS_589_WORKERS overrides (diagnostic)
	stall589KVConnsX = 4                      // C = 4×Workers so every worker holds a /kv conn (p≈1-2·2^-8); CELERIS_589_KVCONNS overrides
	stall589Window   = 10 * time.Second
	stall589Spacing  = 10 * time.Millisecond
	stall589StallBar = 5 * time.Millisecond
	stall589WarmMax  = 20000 // bound on the warm-up loop (settle needs 256 CONSECUTIVE fast runs)
	stall589LearnReq = 100   // learning control: warm with fewer than adaptiveSettleStreak runs

	// stall592PromoteBound is the design's detection bound for celeris#592,
	// measured from the store turning slow: adaptiveSettleTTL (5 s — the
	// re-opener's period, so worst case the gate flips just after a tick) plus
	// the re-timed run itself and the slow run already in flight (2xD) plus
	// 2 s of scheduling slack. Spelled as a literal, NOT as adaptiveSettleTTL,
	// so this exact file also compiles on origin/main for the negative control.
	stall592PromoteBound = 8 * time.Second
	// stall592MaxStalledFrac is the post-promotion /ping stall budget. Asserted
	// on epoll only; io_uring's fraction is printed (celeris#593).
	stall592MaxStalledFrac = 0.05
)

// gatedKV wraps a store.KV whose Set is fast until the gate flips and then
// sleeps stall589Delay on every call — the #493 shape ("the store got slow
// after the route settled"). slowCalls counts COMPLETED slow calls, so waiting
// for slowCalls ≥ C guarantees every /kv conn's first slow run has returned.
type gatedKV struct {
	store.KV
	slow      atomic.Bool
	delay     time.Duration
	slowCalls atomic.Int64
}

func (g *gatedKV) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if g.slow.Load() {
		time.Sleep(g.delay)
		g.slowCalls.Add(1)
	}
	return g.KV.Set(ctx, key, value, ttl)
}

type stall589Mode int

const (
	stall589Settled       stall589Mode = iota // the claim: /kv settled before the store turns slow
	stall589ExplicitAsync                     // control M1-a: /kv registered .Async()
	stall589Learning                          // control M1-b: /kv warmed <256 runs, still learning
)

func (m stall589Mode) String() string {
	switch m {
	case stall589Settled:
		return "settled"
	case stall589ExplicitAsync:
		return "negctrl_async"
	case stall589Learning:
		return "negctrl_learning"
	}
	return "?"
}

// stall589Obs is one run's observation set. Every field is final before the
// assertion reads it (the hammer goroutines are joined and the state is read
// after the window closes).
type stall589Obs struct {
	engine, mode       string
	workers            int
	kvConns            int
	gomaxprocs         int
	warmReqs           int
	settledBefore      bool // after warm-up, before the gate flips
	promotedBefore     bool
	adaptive           bool // router.adaptiveRoutes["/kv"]
	settledAfter       bool // after the slow window
	promotedAfter      bool
	asyncPromotedConns uint64
	slowCalls          int64
	kvReqs             int64
	settledAtFlip      bool          // settled at the instant the store turned slow
	promoteLatency     time.Duration // gate flip → isPromoted observed true
	promotedInBound    bool          // promotion seen within stall592PromoteBound
	preSample          time.Duration // gate flip → sampling start
	window             time.Duration
	samples            int
	stalled            int // /ping samples > stall589StallBar
	stalledFrac        float64
	stalledTime        time.Duration // sum of stalled sample latencies
	pingMin, pingMed   time.Duration
	pingMax            time.Duration
	stacks             *stackTally589 // CELERIS_589_STACK diagnostic, nil otherwise
}

// assertSettledStall589 is the CLAIM: on current main a settled route that
// turns slow stays inline and pins the worker. It returns nil when the
// observation matches the claim and an error naming the first mismatch.
func assertSettledStall589(o stall589Obs) error {
	switch {
	case !o.settledAfter:
		return errors.New("/kv is not in router.settled after the slow window")
	case o.promotedAfter:
		return errors.New("/kv was promoted (isPromoted=true) after the slow window")
	case o.asyncPromotedConns != 0:
		return fmt.Errorf("AsyncPromotedConns=%d, expected 0 (a conn was handed to the dispatch goroutine)", o.asyncPromotedConns)
	case o.stalledFrac < 0.9:
		return fmt.Errorf("stalled fraction %.3f < 0.9 (%d/%d /ping samples > %v)", o.stalledFrac, o.stalled, o.samples, stall589StallBar)
	}
	return nil
}

// assertSettledRetimed592 is the FIXED behaviour: the settled classification is
// re-timed, so a settled route whose store turns slow is promoted and stops
// running on the engine worker. State is asserted on both engines; the /ping
// stall fraction is asserted on epoll only, because io_uring separately pins
// its worker in the timeout sweep even when every conn is async-dispatched
// (celeris#593) — that fraction is printed instead.
func assertSettledRetimed592(o stall589Obs) error {
	switch {
	case !o.promotedInBound:
		return fmt.Errorf("/kv was not promoted within %v of the store turning slow (promote_latency=%v): the settled classification was never re-timed",
			stall592PromoteBound, o.promoteLatency)
	case !o.promotedAfter:
		return errors.New("/kv is not promoted (isPromoted=false) at the end of the window")
	case o.asyncPromotedConns < 1:
		return fmt.Errorf("AsyncPromotedConns=%d, want >= 1 (no conn was handed to the dispatch goroutine)", o.asyncPromotedConns)
	case o.engine == "epoll" && o.stalledFrac >= stall592MaxStalledFrac:
		return fmt.Errorf("epoll stalled fraction %.3f >= %.2f (%d/%d /ping samples > %v) AFTER promotion",
			o.stalledFrac, stall592MaxStalledFrac, o.stalled, o.samples, stall589StallBar)
	}
	return nil
}

var stall589RunSeq atomic.Int64

func TestAdaptiveSettledRouteRetime592(t *testing.T) {
	if testing.Short() {
		t.Skip("celeris#592 regression run takes ~20 s per case; -short skips it")
	}
	for _, eng := range []struct {
		name string
		typ  EngineType
	}{{"iouring", IOUring}, {"epoll", Epoll}} {
		t.Run(eng.name, func(t *testing.T) {
			t.Run("settled", func(t *testing.T) {
				o := runStall589(t, eng.name, eng.typ, stall589Settled)
				// The #589 defect signature must be GONE, and the #592 fixed
				// behaviour must hold. Both directions, one run.
				claimErr := assertSettledStall589(o)
				fixErr := assertSettledRetimed592(o)
				verdict := "FIXED"
				if fixErr != nil {
					verdict = "NOT_FIXED"
				}
				logStall589(t, o, verdict, claimErr)
				if fixErr != nil {
					t.Errorf("celeris#592 fixed-behaviour assertion failed on the settled rig: %v", fixErr)
				}
				if claimErr == nil {
					t.Error("the celeris#589 defect signature still holds on the settled rig (settled, never promoted, stalled_frac >= 0.9)")
				}
			})
			t.Run("negctrl_async", func(t *testing.T) {
				o := runStall589(t, eng.name, eng.typ, stall589ExplicitAsync)
				err := assertSettledStall589(o)
				verdict := "CONTROL_OK"
				var ctlErr error
				switch {
				case err == nil:
					ctlErr = errors.New("claim assertion PASSED on the explicit-.Async() tree (control does not discriminate)")
				case o.adaptive || o.settledAfter:
					ctlErr = errors.New("explicit .Async() route must not be adaptive/settled")
				case int(o.asyncPromotedConns) < o.kvConns:
					ctlErr = fmt.Errorf("AsyncPromotedConns=%d < %d /kv conns", o.asyncPromotedConns, o.kvConns)
				case o.pingMed >= stall589StallBar:
					ctlErr = fmt.Errorf("/ping median %v >= %v with the same blocked Set", o.pingMed, stall589StallBar)
				}
				if ctlErr != nil {
					verdict = "CONTROL_BROKEN"
				}
				logStall589(t, o, verdict, err)
				if ctlErr != nil {
					t.Error(ctlErr)
				}
			})
			t.Run("negctrl_learning", func(t *testing.T) {
				o := runStall589(t, eng.name, eng.typ, stall589Learning)
				err := assertSettledStall589(o)
				verdict := "CONTROL_OK"
				var ctlErr error
				switch {
				case err == nil:
					ctlErr = errors.New("claim assertion PASSED on the still-learning tree (control does not discriminate)")
				case o.settledBefore || o.settledAfter:
					ctlErr = errors.New("learning control must never settle")
				case !o.promotedAfter:
					ctlErr = errors.New("the first >2ms inline run must promote a learning route (isPromoted=false)")
				case int(o.asyncPromotedConns) < o.kvConns:
					ctlErr = fmt.Errorf("AsyncPromotedConns=%d < %d /kv conns", o.asyncPromotedConns, o.kvConns)
				case o.pingMed >= stall589StallBar:
					ctlErr = fmt.Errorf("/ping median %v >= %v after the first inline run per worker", o.pingMed, stall589StallBar)
				}
				if ctlErr != nil {
					verdict = "CONTROL_BROKEN"
				}
				logStall589(t, o, verdict, err)
				if ctlErr != nil {
					t.Error(ctlErr)
				}
			})
		})
	}
}

// logStall589 emits the single greppable line per run. It is called AFTER the
// assertion has been evaluated on the final observation, so every number on it
// is the number the verdict was decided on.
func logStall589(t *testing.T, o stall589Obs, verdict string, claimErr error) {
	t.Helper()
	claim := "pass"
	if claimErr != nil {
		claim = "fail(" + claimErr.Error() + ")"
	}
	t.Logf("RESULT592 engine=%s mode=%s run=%d verdict=%s settled_before=%t promoted_before=%t adaptive=%t settled_at_flip=%t "+
		"settled_after=%t promoted_after=%t promoted_in_bound=%t promote_ms=%.0f async_promoted_conns=%d workers=%d kv_conns=%d gomaxprocs=%d warm_reqs=%d slow_calls=%d kv_reqs=%d "+
		"pre_sample_ms=%.0f window_ms=%.0f samples=%d stalled=%d stalled_frac=%.3f stalled_time_ms=%.0f "+
		"ping_min_ms=%.3f ping_med_ms=%.3f ping_max_ms=%.3f claim589_assert=%s",
		o.engine, o.mode, stall589RunSeq.Add(1), verdict, o.settledBefore, o.promotedBefore, o.adaptive, o.settledAtFlip,
		o.settledAfter, o.promotedAfter, o.promotedInBound, ms(o.promoteLatency), o.asyncPromotedConns, o.workers, o.kvConns, o.gomaxprocs, o.warmReqs, o.slowCalls, o.kvReqs,
		ms(o.preSample), ms(o.window), o.samples, o.stalled, o.stalledFrac, ms(o.stalledTime),
		ms(o.pingMin), ms(o.pingMed), ms(o.pingMax), claim)
	if o.stacks != nil {
		t.Logf("STACKTALLY589 engine=%s mode=%s stalled=%d %s", o.engine, o.mode, o.stalled, o.stacks.String())
	}
	// A control whose /kv conns are ALL async-dispatched must leave the worker
	// free; a stalled fraction above 5 % there is not the item (4) dispatch
	// policy but a second defect (io_uring: checkTimeouts blocks on detachMu
	// held by runAsyncHandler across the slow ProcessH1). Name it on its own
	// line so the CONTROL_OK verdict (decided on the median) cannot hide it.
	// io_uring is not judged on the settled mode's latency: PRINT its fraction
	// next to the epoll bar so the unasserted number is on the record.
	if o.engine == "iouring" && o.mode == stall589Settled.String() {
		t.Logf("IOURING592 engine=iouring mode=settled stalled=%d/%d stalled_frac=%.3f (epoll bar %.2f, NOT asserted here) "+
			"ping_med_ms=%.3f ping_max_ms=%.1f promote_ms=%.0f async_promoted_conns=%d: io_uring carries the separate "+
			"celeris#593 sweep pin (checkTimeouts blocks on detachMu held by runAsyncHandler across the slow ProcessH1) until that fix lands",
			o.stalled, o.samples, o.stalledFrac, stall592MaxStalledFrac, ms(o.pingMed), ms(o.pingMax), ms(o.promoteLatency), o.asyncPromotedConns)
	}
	if o.mode != stall589Settled.String() && o.stalledFrac > 0.05 {
		t.Logf("ANOMALY589 engine=%s mode=%s stalled=%d/%d stalled_frac=%.3f ping_med_ms=%.3f ping_max_ms=%.1f: "+
			"worker pinned while every /kv conn is async-dispatched (async_promoted_conns=%d)",
			o.engine, o.mode, o.stalled, o.samples, o.stalledFrac, ms(o.pingMed), ms(o.pingMax), o.asyncPromotedConns)
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// runStall589 runs one full measurement: start a real engine with 2 workers,
// warm /kv, flip the store slow, hammer /kv on C keep-alive conns, sample
// /ping on a pre-opened keep-alive conn, read the dispatch state, shut down.
func runStall589(t *testing.T, engName string, engType EngineType, mode stall589Mode) stall589Obs {
	t.Helper()
	o := stall589Obs{engine: engName, mode: mode.String(), gomaxprocs: runtime.GOMAXPROCS(0)}
	// Diagnostic overrides: CELERIS_589_WORKERS (engine workers, default 2)
	// and CELERIS_589_KVCONNS (slow /kv conns, default 4×workers). With
	// KVCONNS=1 and 2 workers the single sleeping async conn shares a worker
	// with the /ping probe in ~half the runs, so a stall that is worker-local
	// (a lock the worker takes) shows as a bimodal 0 / >0 stalled count across
	// runs, whereas a process-global cause (GOMAXPROCS, scheduler, the
	// dispatch goroutine) would stall every run.
	workers := envInt589("CELERIS_589_WORKERS", stall589Workers)
	o.kvConns = envInt589("CELERIS_589_KVCONNS", stall589KVConnsX*workers)

	// Freeze the adaptive promotion clock: a promotion made during the run
	// never expires, so the fixed/control worlds show exactly one inline run
	// per worker rather than one per TTL (review correction 1).
	clock, restore := stubNowNano()
	*clock = time.Now().UnixNano()
	defer restore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	delay := stall589Delay
	if v := os.Getenv("CELERIS_589_DELAY_MS"); v != "" { // diagnostic override of D
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			delay = time.Duration(n) * time.Millisecond
		}
	}
	kv := &gatedKV{
		KV:    store.NewMemoryKV(store.MemoryKVConfig{CleanupContext: ctx}),
		delay: delay,
	}
	s := New(Config{Engine: engType, AsyncHandlers: true, Workers: workers})
	s.GET("/ping", func(c *Context) error { return c.String(http.StatusOK, "ok") })
	route := s.GET("/kv", func(c *Context) error {
		if err := kv.Set(c.Context(), "k", []byte("v"), time.Minute); err != nil {
			return err
		}
		return c.String(http.StatusOK, "ok")
	})
	if mode == stall589ExplicitAsync {
		route.Async()
	}
	o.adaptive = s.router.adaptiveRoutes["/kv"]
	if mode != stall589ExplicitAsync && !o.adaptive {
		t.Fatal("/kv must be adaptive under AsyncHandlers=true without an explicit override")
	}
	if !s.router.adaptiveRoutes["/ping"] {
		t.Fatal("/ping must be adaptive (it is the inline-on-worker probe)")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	startErr := make(chan error, 1)
	go func() { startErr <- s.StartWithListenerAndContext(ctx, ln) }()

	// Readiness on /ping over a throwaway conn; an early Start error (no
	// io_uring in this kernel/seccomp profile) is reported, not hidden.
	deadline := time.Now().Add(10 * time.Second)
	ready := false
	for time.Now().Before(deadline) && !ready {
		select {
		case err := <-startErr:
			if engType == IOUring {
				t.Skipf("io_uring engine failed to start (run with --security-opt seccomp=unconfined): %v", err)
			}
			t.Fatalf("engine failed to start: %v", err)
		default:
		}
		c, br, err := dial589(addr)
		if err == nil {
			if err = get589(c, br, "/ping"); err == nil {
				ready = true
			}
			_ = c.Close()
		}
		if !ready {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !ready {
		t.Fatal("server did not become ready")
	}
	info := s.EngineInfo()
	if info == nil || info.Type != engType {
		t.Fatalf("engine type = %v, want %v (silent fallback would invalidate the run)", info, engType)
	}
	o.workers = info.Metrics.Workers
	if o.workers != workers {
		t.Fatalf("workers=%d, want %d (memlock cap? run with --ulimit memlock=-1)", o.workers, workers)
	}

	// Warm-up on one keep-alive conn, sequential: settle (bounded loop, since
	// one >300µs jitter run resets the fast streak) or a fixed sub-streak count.
	wc, wbr, err := dial589(addr)
	if err != nil {
		t.Fatalf("dial warm conn: %v", err)
	}
	switch mode {
	case stall589Settled:
		// The celeris#592 re-opener clears the settled set every
		// adaptiveSettleTTL; if a tick lands between the warm loop's exit and
		// the precondition read, warm again — the fast streak survives the
		// re-open, so a single further request re-settles the route.
		for attempt := 0; attempt < 5; attempt++ {
			for o.warmReqs < stall589WarmMax {
				if err := get589(wc, wbr, "/kv"); err != nil {
					t.Fatalf("warm-up GET /kv #%d: %v", o.warmReqs, err)
				}
				o.warmReqs++
				if _, ok := s.router.settled.Load("/kv"); ok {
					break
				}
			}
			if _, ok := s.router.settled.Load("/kv"); ok {
				break
			}
		}
	default:
		for o.warmReqs < stall589LearnReq {
			if err := get589(wc, wbr, "/kv"); err != nil {
				t.Fatalf("warm-up GET /kv #%d: %v", o.warmReqs, err)
			}
			o.warmReqs++
		}
	}
	_ = wc.Close()
	_, o.settledBefore = s.router.settled.Load("/kv")
	o.promotedBefore = s.router.isPromoted("/kv")
	switch mode {
	case stall589Settled:
		if !o.settledBefore || o.promotedBefore {
			t.Fatalf("precondition: /kv must be settled and not promoted after warm-up (settled=%t promoted=%t after %d runs)",
				o.settledBefore, o.promotedBefore, o.warmReqs)
		}
	case stall589Learning:
		if o.settledBefore || o.promotedBefore || !s.router.adaptiveLearning("/kv") {
			t.Fatalf("precondition: /kv must still be learning after %d runs (settled=%t promoted=%t)",
				o.warmReqs, o.settledBefore, o.promotedBefore)
		}
	}

	// The /ping probe conn is opened BEFORE the store turns slow so it is
	// already pinned to a worker.
	pc, pbr, err := dial589(addr)
	if err != nil {
		t.Fatalf("dial ping conn: %v", err)
	}
	defer func() { _ = pc.Close() }()
	if err := get589(pc, pbr, "/ping"); err != nil {
		t.Fatalf("pre-flip GET /ping: %v", err)
	}

	// Flip the gate, then hammer /kv on C keep-alive conns, one request in
	// flight per conn, until told to stop.
	kv.slow.Store(true)
	flipAt := time.Now()
	// Whether the route was still settled at the instant the store turned slow.
	// With the celeris#592 re-opener a tick can land in the few ms between the
	// precondition read and the flip; such a run exercises the learning path
	// instead and is visible on the result line rather than silently folded in.
	_, o.settledAtFlip = s.router.settled.Load("/kv")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var kvReqs atomic.Int64
	hammerErr := make(chan error, o.kvConns)
	for i := 0; i < o.kvConns; i++ {
		c, br, err := dial589(addr)
		if err != nil {
			t.Fatalf("dial kv conn %d: %v", i, err)
		}
		defer func() { _ = c.Close() }()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := get589(c, br, "/kv"); err != nil {
					hammerErr <- err
					return
				}
				kvReqs.Add(1)
			}
		}()
	}

	// Sampling starts only once every /kv conn has completed one slow run —
	// the post-hoc classifier can only act AFTER a slow run returns, so the
	// first inline run per worker is the same in every world and is excluded.
	waitDeadline := time.Now().Add(30 * time.Second)
	for kv.slowCalls.Load() < int64(o.kvConns) {
		if time.Now().After(waitDeadline) {
			t.Fatalf("only %d slow calls completed in 30 s", kv.slowCalls.Load())
		}
		select {
		case err := <-hammerErr:
			t.Fatalf("kv hammer: %v", err)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	// celeris#592: the settled classification is re-opened every
	// adaptiveSettleTTL, the re-timed run is ~D (far over
	// adaptiveBlockingThreshold) and the route promotes. Wait for that —
	// bounded by the design's bound — before sampling, so the stalled fraction
	// the assertion reads is the one for the REST of the window, after
	// promotion. On a tree without the fix this loop runs out the bound and the
	// assertion fails on promoted_in_bound.
	if mode == stall589Settled {
		promoteDeadline := flipAt.Add(stall592PromoteBound)
		for {
			if s.router.isPromoted("/kv") {
				o.promotedInBound = true
				o.promoteLatency = time.Since(flipAt)
				break
			}
			if time.Now().After(promoteDeadline) {
				o.promoteLatency = time.Since(flipAt)
				break
			}
			select {
			case err := <-hammerErr:
				t.Fatalf("kv hammer: %v", err)
			default:
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	o.preSample = time.Since(flipAt)

	// Diagnostics (test-only, env-gated): CELERIS_589_FRESH=1 samples /ping on
	// a FRESH conn per sample (random worker) instead of the pre-opened conn;
	// CELERIS_589_DUMP=1 logs the chronological latency series.
	fresh := os.Getenv("CELERIS_589_FRESH") != ""
	var lat []time.Duration
	winStart := time.Now()
	for time.Since(winStart) < stall589Window {
		t0 := time.Now()
		if fresh {
			fc, fbr, err := dial589(addr)
			if err != nil {
				t.Fatalf("dial fresh ping conn: %v", err)
			}
			err = get589(fc, fbr, "/ping")
			_ = fc.Close()
			if err != nil {
				t.Fatalf("GET /ping (fresh conn) during window: %v", err)
			}
		} else if os.Getenv("CELERIS_589_STACK") != "" {
			// Diagnostic: 100 ms into EVERY stalled /ping, classify the
			// event-loop goroutines' blocking site (tallied on the STACK589
			// result line) and dump the first one in full.
			if o.stacks == nil {
				o.stacks = &stackTally589{}
			}
			done := make(chan error, 1)
			go func() { done <- get589(pc, pbr, "/ping") }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("GET /ping during window: %v", err)
				}
			case <-time.After(100 * time.Millisecond):
				o.stacks.capture(t)
				if err := <-done; err != nil {
					t.Fatalf("GET /ping during window: %v", err)
				}
			}
		} else if err := get589(pc, pbr, "/ping"); err != nil {
			t.Fatalf("GET /ping during window: %v", err)
		}
		lat = append(lat, time.Since(t0))
		time.Sleep(stall589Spacing)
	}
	o.window = time.Since(winStart)
	if os.Getenv("CELERIS_589_DUMP") != "" {
		series := make([]string, 0, len(lat))
		for _, d := range lat {
			series = append(series, fmt.Sprintf("%.1f", ms(d)))
		}
		t.Logf("LAT589 engine=%s mode=%s fresh=%t n=%d ms=[%s]", engName, mode, fresh, len(lat), strings.Join(series, " "))
	}

	// Join the hammers (each finishes its in-flight request), THEN read the
	// state so every field is final before the assertion.
	close(stop)
	wg.Wait()
	select {
	case err := <-hammerErr:
		t.Fatalf("kv hammer: %v", err)
	default:
	}
	o.kvReqs = kvReqs.Load()
	o.slowCalls = kv.slowCalls.Load()
	_, o.settledAfter = s.router.settled.Load("/kv")
	o.promotedAfter = s.router.isPromoted("/kv")
	o.asyncPromotedConns = s.EngineInfo().Metrics.AsyncPromotedConns

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	o.samples = len(lat)
	if o.samples > 0 {
		o.pingMin, o.pingMed, o.pingMax = lat[0], lat[o.samples/2], lat[o.samples-1]
	}
	for _, d := range lat {
		if d > stall589StallBar {
			o.stalled++
			o.stalledTime += d
		}
	}
	if o.samples > 0 {
		o.stalledFrac = float64(o.stalled) / float64(o.samples)
	}

	// Orderly stop: client conns are closed by the deferred Close calls after
	// the engine has exited, so no handler is cut mid-frame by the rig.
	cancel()
	select {
	case err := <-startErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("Start returned after cancel: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("engine did not exit within 30 s of cancel")
	}
	return o
}

func dial589(addr string) (net.Conn, *bufio.Reader, error) {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, nil, err
	}
	return c, bufio.NewReader(c), nil
}

// get589 issues one keep-alive GET on the conn and fully reads the response.
func get589(c net.Conn, br *bufio.Reader, path string) error {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: 589\r\n\r\n"); err != nil {
		return err
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", path, resp.StatusCode)
	}
	return nil
}
