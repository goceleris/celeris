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

	"github.com/goceleris/celeris/engine/iouring"
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
//   - LATENCY (epoll only), celeris#622: the /ping median over the window AFTER
//     promotion is at least stall592MinSpeedup times lower than the median the
//     SAME sampler measured over the window BEFORE it, while the route was
//     still settled and inline. It is a ratio between two measurements of the
//     same run on the same box, not a number of milliseconds picked for a
//     particular one.
//
// io_uring's numbers are PRINTED, not asserted: when this rig was written even
// the fully async control pinned the io_uring worker ~30 % of wall time through
// the celeris#593 timeout sweep (see the note below), a separate defect with
// its own fix. That fix is PR #604, which now sits underneath this branch, and
// the io_uring fraction measured on this tree is 0.000 in 10/10 settled runs.
// The bar stays epoll-only so this change does one thing; the IOURING592 line
// carries the numbers for whoever tightens it.
//
// The observable follows the two binding adversarial-review corrections on the
// issue: the PRIMARY assertion is the dispatch STATE after the route has turned
// slow (settled.Load, isPromoted, EngineMetrics.AsyncPromotedConns); the
// latency observable is taken over a window that starts only after every /kv
// connection has completed one slow run (so the unavoidable first post-hoc
// inline run per worker is excluded) and, in the settled mode, only after the
// route has been promoted, and the promotion TTL is pinned out of the picture
// with the existing nowNano test hook (frozen clock) so the promotion made
// during the run cannot expire. The re-opener runs off a real time.Ticker, so
// the frozen clock does not stall it. The interval between those two points —
// every /kv conn slow, route not yet promoted — is not dead time: it is
// sampled too, and it is the pinned-world reference the post-promotion window
// is divided by (celeris#622).
//
// Negative controls (the same rig, the same blocked Set):
//   - explicit .Async() on /kv (item (4)'s "store-backed middleware blocking by
//     default" flavour): the route is never adaptive, every /kv conn is handed
//     to the dispatch goroutine, the worker stays free;
//   - /kv still LEARNING (warmed with fewer than adaptiveSettleStreak runs): the
//     first slow inline run promotes the route immediately, later runs go async.
//     The learning state is ESTABLISHED, not hoped for (celeris#620 fault 2).
//
// Both controls' latency observable is RELATIVE too (celeris#628). They used
// to end on the same absolute 5 ms bar celeris#622 took off the settled arm —
// and it failed them 3 runs in 20 on a loaded box, at 8.56 ms and 5.43 ms,
// after their state assertions had already proven the worker free. They now
// divide what a blocked Set costs a client in the same window (the /kv
// hammer's own median) by the /ping median: assertCtlWorkerFree628.
//
// Every warm-up here runs against a FAST store, so a promotion during warm-up
// is runner jitter, never a property of the tree under test: handler.go calls
// promoteRouteImmediate on a SINGLE inline run over adaptiveBlockingThreshold,
// so one jittery request is enough, and because this rig freezes the promotion
// clock the TTL that would normally undo it never fires. That cost the
// learning control its precondition on run 34863046623 (`precondition: /kv
// must still be learning after 100 runs (settled=false promoted=true)`) before
// the control had tested anything, and it is worse in the settled arm, where
// the same promotion also clears the fast streak and takes the route off the
// timed path — the route can then never settle, and the warm loop burns its
// whole 20000-request bound before failing the same way.
//
// So both adaptive warm loops drive any such promotion back out through the
// engine's own de-promotion transition (depromote589, the body of isPromoted's
// expiry branch) and count it on the result line as warm_depromotions. The
// preconditions are still asserted afterwards — reaching one now means the
// de-promotion itself did not take, which is a real defect — and neither
// control's claims change: promoted after the flip, every /kv conn
// async-dispatched, a sub-5 ms /ping median, the #589 signature absent. They
// still have to invert.
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
// That paragraph is now HISTORY, and the two fixes compose. celeris#593 is
// fixed on main by PR #604 (checkTimeouts/handleHeaderTimer TryLock the
// h1State snapshot instead of blocking on detachMu), and re-measured on this
// rebased tree — 10 runs per engine, golang:1.27, --cpus 4,
// seccomp=unconfined, --ulimit memlock=128 MiB — every one of the 60 subtests
// passes with stalled_frac 0.000: io_uring settled 0/874-960 samples over 5 ms
// in 10/10 (ping_max 0.30-2.49 ms, previously ~0.30 of the window), epoll
// settled 0.000 in 10/10, and both controls 0.000 on both engines. The route
// promotes in 5415-5422 ms of the 8 s bound with AsyncPromotedConns=8 every
// run, and claim589_assert reports the #589 defect signature as absent in all
// 60. No ANOMALY589 line was emitted and no data race was reported.
//
// Portability: the io_uring half of the matrix needs stall589Workers real
// workers, and io_uring locks ~12 MiB per worker against RLIMIT_MEMLOCK, so on
// a memlock-capped host (a GitHub Actions runner is 8 MiB = one worker) it
// SKIPS via skipIfMemlockCaps589 instead of failing; the epoll half always
// runs. Without that pre-flight the capped runner does not merely under-fill
// the engine, it fails the readiness wait below (which holds out for the whole
// worker set) — the shape that failed CI on PR #603 (`server did not become
// ready (... Workers:1 ...), want 2 workers`, memlock_cur_bytes=8388608).
// Measured under the CI command line (`go test -race -count=1 -timeout=300s`)
// in golang:1.27, --cpus 4, seccomp=unconfined: with `--ulimit
// memlock=8388608` the three io_uring subtests SKIP with the memlock reason,
// epoll runs 3/3 and the package is ok; with 128 MiB all six run at workers=2.
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
	stall589LearnReq = 100   // learning control: warm with fewer than adaptiveSettleStreak runs (jitter promotions are undone, see depromote589)

	// stall592PromoteBound is the design's detection bound for celeris#592,
	// measured from the store turning slow: adaptiveSettleTTL (5 s — the
	// re-opener's period, so worst case the gate flips just after a tick) plus
	// the re-timed run itself and the slow run already in flight (2xD) plus
	// 2 s of scheduling slack. Spelled as a literal, NOT as adaptiveSettleTTL,
	// so this exact file also compiles on origin/main for the negative control.
	stall592PromoteBound = 8 * time.Second

	// The settled arm's latency observable, celeris#622. It used to be "fewer
	// than 5 % of the post-promotion /ping samples exceed 5 ms", and 5 ms of
	// wall clock on a shared runner is a measurement of the runner: under
	// eight spinners plus a cold-cache build loop that arm failed 7 runs in 20
	// (stalled fraction 0.053-0.083) while passing 5/5 on the same tree idle,
	// and negctrl_async — where every /kv conn is async-dispatched from its
	// first request, so the worker is provably free — reached 0.094 on the same
	// box. A bar a provably-free worker exceeds is not measuring the engine.
	//
	// stall592MinSpeedup replaces it with a RATIO taken inside the run:
	// the post-promotion /ping median against the SAME RUN's pre-promotion
	// median, sampled by the same client loop, on the same box, seconds
	// earlier, against the same blocked store, while the route was still
	// settled and therefore still inline on the worker. That is the
	// "still-settled vs re-timed" comparison the arm is really claiming, and a
	// loaded runner inflates numerator and denominator together. Measured: the
	// ratio is 1.0 with the celeris#592 re-opener disabled, and 105x-13,027x
	// across 40 runs of the fixed tree under 14 spinners plus two cold-cache
	// build loops at GOMAXPROCS 4 (load average up to 41.9) — the 105x being
	// the single worst-starved run of the 40. 5x sits 21x clear of that worst
	// case and 5x clear of the defect. Paired against the old bar on the same
	// box, alternating binaries run by run: 14 of 20 loaded runs failed the
	// 5 ms bar, 0 of 20 failed this one, and on the last 20 runs of the final
	// tree the old bar — recomputed from the very same samples, which is why
	// stalled_frac is still on the result line — would have failed 18.
	//
	// A fraction-of-samples-over-a-bar assertion was tried in place of the old
	// one and REJECTED on measurement, which is why only the ratio is
	// asserted. Even with the bar derived from the rig's own injected delay
	// (stall589Delay/2 = 150 ms, far above any plausible scheduling blip),
	// that worst-starved run put 16 of its 110 samples over it — a queued
	// fraction of 0.145 on a worker the same run proves was NOT pinned, since
	// its median /ping was 18.95 ms against the pinned window's 1,994 ms. A
	// fraction bar is a tail statistic, and on a starved runner the tail is
	// the runner's. The fraction is still COMPUTED and printed (queued_frac,
	// with a QUEUED592 line above stall592QueuedNotice) because it is the
	// number to read when the ratio ever does fail; it just does not decide.
	//
	// stall592MinRefSamples is the reference-window floor. Promotion normally
	// lands ~5.4 s after the flip (one adaptiveSettleTTL) and a pinned /ping
	// costs ~0.6-2.1 s — it waits out the in-flight Set plus the other /kv
	// conns the worker serves inline before it — so the pinned window measures
	// a handful of samples, not hundreds (2-13 over those 40 runs). That is
	// enough: each one is a MAGNITUDE (~D or several D) rather than a point in
	// a tight distribution, and the median of an even count takes the upper
	// middle, so a final sample that straddles the promotion instant cannot
	// drag the reference down. Below the floor (a re-open tick landing just
	// after the flip can promote in a few hundred ms) there is no pinned
	// median to divide by: the ratio is skipped, ref_samples on the result
	// line says so, and the STATE assertions — which are the primary evidence
	// for celeris#592 and are unaffected by load — carry the arm.
	stall592MinSpeedup    = 5.0
	stall592QueuedNotice  = 0.05
	stall592MinRefSamples = 2

	// The CONTROL arms' latency observable, celeris#628. Both controls used
	// to end with `o.pingMed >= stall589StallBar` — the same absolute 5 ms of
	// wall clock celeris#622 removed from the settled arm, one level down.
	// Measured over 20 runs of this rig in CI's exact shape on a loaded box
	// (load average ~30), 3 failed and the two captured were this line:
	// io_uring negctrl_async at a /ping median of 8.56 ms and
	// negctrl_learning at 5.43 ms. Both arms had already proven the state
	// they exist to prove — route not adaptive/settled (or still learning),
	// every /kv conn async-dispatched — so the failures said nothing about
	// dispatch policy and everything about the runner. GitHub's hosted CI
	// skips the io_uring arms, so the bar never reddened a pull request; it
	// waits for the cluster.
	//
	// What a control claims is that the expected DIFFERENCE does not appear:
	// /ping did not queue behind a blocked Set. So it is now judged as a
	// comparison taken inside the run. The rig already has the pinned
	// magnitude to divide by — what a blocked Set costs a client on THIS box
	// under THIS load, measured by the /kv hammer over the very same window
	// the /ping samples come from, by the same kind of keep-alive client
	// against the same store. A worker that never waits for a Set answers
	// /ping in a small fraction of one; a pinned one answers it in about one
	// (the settled arm's pinned window measured 0.29-2.10 s of /ping against
	// a 300 ms D, because each worker serves its /kv conns serially before
	// the probe). stall592MinCtlSpeedup = 5 therefore says "/ping cost at
	// most a fifth of one blocked Set, so it cannot have waited for one",
	// and a loaded runner inflates both medians together.
	//
	// Measured, golang:1.27, --cpus 4, --ulimit memlock=128M, 28 spinners
	// plus two cold-cache `go build -a` loops in the same cgroup, load
	// average 1.3-39.7, 20 paired runs of CI's own command line alternating
	// this binary with main's: 80 control arms, ratio 161.1x-1981.3x, none
	// within an order of magnitude of the 5x bar. The same 80 arms put the
	// old bar's worst case at 1.87 ms against its 5 ms — 2.7x from failing,
	// where the ratio's worst case is 32.2x from failing. With async
	// dispatch disabled (the difference the control exists to exclude made
	// to appear) it collapses to 1.0-1.7x and all four arms fail.
	//
	// The legacy 5 ms median stays on the RESULT592 line (ping_med_ms) and
	// gets a CTL628 notice line, unasserted, so the historical series and
	// the old signal survive the change.
	//
	// stall592MinCtlRefSamples is the floor on the /kv window: below it there
	// is no measured cost-of-a-Set to divide by and the ratio is skipped, the
	// state assertions carrying the arm. Eight conns at ~D each over a 10 s
	// window normally give a few hundred.
	stall592MinCtlSpeedup    = 5.0
	stall592MinCtlRefSamples = 4
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

// kvSample589 is one completed /kv request: how long it took and when it
// finished. The instant is what lets the control's reference median be taken
// over exactly the /ping window rather than over the whole hammer run
// (celeris#628).
type kvSample589 struct {
	done time.Time
	lat  time.Duration
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
	depromotions       int  // learning control: jitter promotions undone during warm-up (celeris#620)
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
	stalled            int // /ping samples > stall589StallBar (REPORTED; see queued for the assertion)
	stalledFrac        float64
	stalledTime        time.Duration // sum of stalled sample latencies
	pingMin, pingMed   time.Duration
	pingMax            time.Duration

	// The pinned-world reference (settled mode only, celeris#622): the same
	// /ping sampler, running from the moment every /kv conn has completed one
	// slow call until the route is promoted — i.e. while the settled route is
	// still inline on the worker. It is the denominator the post-promotion
	// window is judged against, so both numbers carry the same runner.
	refSamples  int
	refPingMed  time.Duration
	refPingMax  time.Duration
	refStalled  int // reference samples > stall589StallBar (reported)
	refWindow   time.Duration
	speedup     float64       // refPingMed / pingMed; 0 when there is no reference
	queuedBar   time.Duration // "this /ping waited for a blocked Set" bar (stall589Delay/2)
	queued      int           // post-promotion samples > queuedBar
	queuedFrac  float64
	refQueued   int // reference samples > queuedBar (the pinned world's own rate, reported)
	refQueuedFr float64

	// The controls' in-run reference (celeris#628): what one blocked Set
	// costs a keep-alive client on this box under this load, measured by the
	// /kv hammer over the SAME window the /ping samples were taken in. Only
	// requests that COMPLETED inside the window are counted, so the two
	// medians carry the same runner. ctlSpeedup is kvMed/pingMed — a free
	// worker answers /ping in a small fraction of a blocked Set, a pinned one
	// in about one of them.
	kvSamples  int
	kvMed      time.Duration
	kvMax      time.Duration
	ctlSpeedup float64

	stacks *stackTally589 // CELERIS_589_STACK diagnostic, nil otherwise
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
// running on the engine worker. State is asserted on both engines; the latency
// observable is asserted on epoll only, because io_uring separately pinned its
// worker in the timeout sweep even when every conn is async-dispatched
// (celeris#593) — its numbers are printed instead. #593 is fixed on main by
// PR #604, which sits underneath this rig, and the printed io_uring numbers
// invert as epoll's do; the bar is left epoll-only so this stays one change,
// and the IOURING592 line carries what a tightening would be judged on.
//
// The latency observable is RELATIVE (celeris#622). What the settled arm
// claims is that a re-timed route stops costing the worker — not that a /ping
// completes within some number of milliseconds of wall clock, which on a
// shared runner is a statement about the runner. So the arm is judged on a
// ratio anchored inside the run: the post-promotion /ping median must be at
// least stall592MinSpeedup times faster than the SAME run's pre-promotion
// median, measured by the same sampler against the same blocked store while
// the route was still settled and inline. It is skipped only when promotion
// was so fast that the pinned window holds fewer than stall592MinRefSamples
// samples, and then the state assertions above carry the arm.
//
// The queued fraction (samples over queuedBar589) is computed and printed
// beside it but deliberately NOT asserted — the const block records the run
// that decided that.
func assertSettledRetimed592(o stall589Obs) error {
	switch {
	case !o.promotedInBound:
		return fmt.Errorf("/kv was not promoted within %v of the store turning slow (promote_latency=%v): the settled classification was never re-timed",
			stall592PromoteBound, o.promoteLatency)
	case !o.promotedAfter:
		return errors.New("/kv is not promoted (isPromoted=false) at the end of the window")
	case o.asyncPromotedConns < 1:
		return fmt.Errorf("AsyncPromotedConns=%d, want >= 1 (no conn was handed to the dispatch goroutine)", o.asyncPromotedConns)
	case o.engine == "epoll" && o.refSamples >= stall592MinRefSamples && o.speedup < stall592MinSpeedup:
		return fmt.Errorf("epoll: /ping median is only %.1fx faster after promotion (want >= %.0fx): %v over %d post-promotion samples "+
			"against %v over %d samples taken while the route was still settled — same sampler, same blocked store, same run "+
			"(queued %d/%d over %v)",
			o.speedup, stall592MinSpeedup, o.pingMed, o.samples, o.refPingMed, o.refSamples, o.queued, o.samples, o.queuedBar)
	}
	return nil
}

// assertCtlWorkerFree628 is the CONTROL arms' latency observable: with every
// /kv conn on a dispatch goroutine, the engine worker never waits for the
// blocked Set, so a /ping on a conn that shares those workers must cost a
// small fraction of what one blocked Set costs — not "under 5 ms", which is a
// statement about the runner (celeris#628).
//
// Both quantities come from the SAME window of the SAME run: o.pingMed is the
// median of the /ping samples taken over it, o.kvMed the median of the /kv
// requests that COMPLETED inside it, each measured client-side across a
// keep-alive conn to the same engine. A loaded box inflates both, so the ratio
// is a property of the dispatch policy rather than of the machine. It is
// skipped only when the window caught fewer than stall592MinCtlRefSamples /kv
// completions, in which case there is nothing measured to divide by and the
// control's state assertions carry the arm.
//
// The pinned world it must exclude is the rig's own #589 signature: a /ping
// that queues behind an in-flight Set costs ~D and usually several multiples
// of it (0.29-2.10 s measured against a 300 ms D in the settled arm's pinned
// window, because a worker serves its /kv conns serially before the probe), so
// there the ratio is at or below 1.
func assertCtlWorkerFree628(o stall589Obs) error {
	if o.kvSamples < stall592MinCtlRefSamples || o.pingMed <= 0 {
		return nil
	}
	if o.ctlSpeedup < stall592MinCtlSpeedup {
		return fmt.Errorf("%s: /ping median %v is only %.1fx cheaper than one blocked Set (want >= %.0fx): "+
			"%d /ping samples against a /kv median of %v over %d completions in the same window, same box, same run — "+
			"a worker whose /kv conns are ALL async-dispatched must not be paying for the Set "+
			"(async_promoted_conns=%d kv_conns=%d)",
			o.engine, o.pingMed, o.ctlSpeedup, stall592MinCtlSpeedup,
			o.samples, o.kvMed, o.kvSamples, o.asyncPromotedConns, o.kvConns)
	}
	return nil
}

// queuedBar589 is the latency at which a /ping sample is counted as having
// waited for the blocked Set rather than for the scheduler: half the delay the
// rig itself injects into the store (D/2 = 150 ms at the default D). It is
// derived from the run's own load, not from a guess about the machine, and it
// tracks the CELERIS_589_DELAY_MS override.
//
// A /ping that queues behind an in-flight Set costs ~D and often several
// multiples of it — the pinned window measured 0.29-2.10 s per sample, because
// each worker holds stall589KVConnsX /kv conns and serves them serially while
// the route is inline — so in that window EVERY sample is over the bar. After
// promotion the fraction over it stayed at or under 0.012 in 39 of 40 loaded
// runs; the fortieth put 0.145 over it while its median stayed at 18.95 ms,
// which is why this is a reported number and not an assertion: see
// stall592QueuedNotice.
func queuedBar589(delay time.Duration) time.Duration { return delay / 2 }

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
				default:
					ctlErr = assertCtlWorkerFree628(o)
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
				default:
					ctlErr = assertCtlWorkerFree628(o)
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
		"settled_after=%t promoted_after=%t promoted_in_bound=%t promote_ms=%.0f async_promoted_conns=%d workers=%d kv_conns=%d gomaxprocs=%d warm_reqs=%d warm_depromotions=%d slow_calls=%d kv_reqs=%d "+
		"pre_sample_ms=%.0f window_ms=%.0f samples=%d stalled=%d stalled_frac=%.3f stalled_time_ms=%.0f "+
		"ping_min_ms=%.3f ping_med_ms=%.3f ping_max_ms=%.3f "+
		"queued_bar_ms=%.0f queued=%d queued_frac=%.3f speedup=%.1f "+
		"kv_samples=%d kv_med_ms=%.3f kv_max_ms=%.3f ctl_speedup=%.1f "+
		"ref_samples=%d ref_window_ms=%.0f ref_ping_med_ms=%.3f ref_ping_max_ms=%.3f ref_stalled=%d ref_queued_frac=%.3f claim589_assert=%s",
		o.engine, o.mode, stall589RunSeq.Add(1), verdict, o.settledBefore, o.promotedBefore, o.adaptive, o.settledAtFlip,
		o.settledAfter, o.promotedAfter, o.promotedInBound, ms(o.promoteLatency), o.asyncPromotedConns, o.workers, o.kvConns, o.gomaxprocs, o.warmReqs, o.depromotions, o.slowCalls, o.kvReqs,
		ms(o.preSample), ms(o.window), o.samples, o.stalled, o.stalledFrac, ms(o.stalledTime),
		ms(o.pingMin), ms(o.pingMed), ms(o.pingMax),
		ms(o.queuedBar), o.queued, o.queuedFrac, o.speedup,
		o.kvSamples, ms(o.kvMed), ms(o.kvMax), o.ctlSpeedup,
		o.refSamples, ms(o.refWindow), ms(o.refPingMed), ms(o.refPingMax), o.refStalled, o.refQueuedFr, claim)
	if o.stacks != nil {
		t.Logf("STACKTALLY589 engine=%s mode=%s stalled=%d %s", o.engine, o.mode, o.stalled, o.stacks.String())
	}
	// A control whose /kv conns are ALL async-dispatched must leave the worker
	// free; a stalled fraction above 5 % there is not the item (4) dispatch
	// policy but a second defect (io_uring: checkTimeouts blocks on detachMu
	// held by runAsyncHandler across the slow ProcessH1). Name it on its own
	// line so the CONTROL_OK verdict (decided on the median) cannot hide it.
	// io_uring is not judged on the settled mode's latency: PRINT the two
	// numbers the epoll arm IS judged on (celeris#622 — the queued fraction
	// against the injected-delay bar, and the settled-vs-pinned median
	// speedup) so the unasserted side is on the record in the asserted form.
	// The celeris#593 sweep pin this allowance was written for is FIXED on
	// main (PR #604, the checkTimeouts/handleHeaderTimer TryLock) — so the
	// line is now a witness that the two fixes compose, and the numbers to
	// watch if the bar is ever tightened to cover both engines.
	if o.engine == "iouring" && o.mode == stall589Settled.String() {
		t.Logf("IOURING592 engine=iouring mode=settled speedup=%.1fx (epoll bar %.0fx, ref %v over %d pinned samples) "+
			"queued=%d/%d queued_frac=%.3f over %v stalled_frac=%.3f ping_med_ms=%.3f ping_max_ms=%.1f promote_ms=%.0f "+
			"async_promoted_conns=%d: reported, not asserted — the celeris#593 sweep pin (checkTimeouts blocking on detachMu "+
			"held by runAsyncHandler across the slow ProcessH1) is fixed by PR #604 underneath this rig",
			o.speedup, stall592MinSpeedup, o.refPingMed, o.refSamples,
			o.queued, o.samples, o.queuedFrac, o.queuedBar, o.stalledFrac, ms(o.pingMed), ms(o.pingMax), ms(o.promoteLatency),
			o.asyncPromotedConns)
	}
	// The queued fraction is the tail of the post-promotion window against the
	// rig's own injected delay. It is NOT asserted (celeris#622: a starved
	// runner owns that tail — 0.145 measured on a run whose median proved the
	// worker free), but a high value next to a healthy speedup is worth having
	// on the record, and a high value next to a poor speedup is the pin.
	if o.mode == stall589Settled.String() && o.queuedFrac > stall592QueuedNotice {
		t.Logf("QUEUED592 engine=%s mode=settled queued=%d/%d queued_frac=%.3f over %v (reported, not asserted) speedup=%.1fx "+
			"ping_med_ms=%.3f ping_max_ms=%.1f ref_ping_med_ms=%.3f: read the speedup — a pinned worker moves BOTH",
			o.engine, o.queued, o.samples, o.queuedFrac, o.queuedBar, o.speedup, ms(o.pingMed), ms(o.pingMax), ms(o.refPingMed))
	}
	// The control arms' legacy signal (celeris#628): the absolute 5 ms bar
	// they used to be judged on. It is REPORTED, never asserted — it is the
	// number that failed 3 runs in 20 on a loaded box while the arms' own
	// state proved the worker free — so the historical series survives and a
	// tally of old-bar-vs-new can be recomputed from these very lines.
	if o.mode != stall589Settled.String() && o.pingMed >= stall589StallBar {
		t.Logf("CTL628 engine=%s mode=%s ping_med_ms=%.3f >= legacy bar %v (reported, not asserted) "+
			"ctl_speedup=%.1fx against a %v /kv median over %d completions in the same window (bar %.0fx): "+
			"read the ratio — a worker paying for the blocked Set moves BOTH",
			o.engine, o.mode, ms(o.pingMed), stall589StallBar, o.ctlSpeedup, o.kvMed, o.kvSamples, stall592MinCtlSpeedup)
	}
	if o.mode != stall589Settled.String() && o.stalledFrac > 0.05 {
		t.Logf("ANOMALY589 engine=%s mode=%s stalled=%d/%d stalled_frac=%.3f ping_med_ms=%.3f ping_max_ms=%.1f: "+
			"worker pinned while every /kv conn is async-dispatched (async_promoted_conns=%d)",
			o.engine, o.mode, o.stalled, o.samples, o.stalledFrac, ms(o.pingMed), ms(o.pingMax), o.asyncPromotedConns)
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// skipIfMemlockCaps589 keeps this rig portable to a memlock-capped runner.
// io_uring locks ~12 MiB of ring + provided-buffer pages per worker, so a host
// with a low RLIMIT_MEMLOCK (GitHub Actions ships a soft limit of 8 MiB, which
// is one worker at most) makes the engine start with FEWER workers than the rig
// asks for. That is an environment fact, not a regression, and the rig needs
// >1 worker by construction: the /ping probe must be able to land on a worker
// other than the one that owns a sleeping /kv conn, which is exactly what the
// settled-route observable is measured against.
//
// The gate is the engine's OWN exported pre-flight (iouring.MaxWorkersForMemlock,
// engine/iouring/ring.go) — the same rlim.Cur/minMemlockPerWorker arithmetic
// capWorkersToMemlock applies at start — so the skip predicate and the cap that
// would trigger it cannot drift apart. It returns -1 for "no cap" (RLIM_INFINITY
// or an unreadable limit), in which case the rig runs.
//
// Nothing else is skipped: the epoll half of the matrix does not lock pages and
// always runs, and when the cap DOES allow the workers the later
// info.Metrics.Workers check still Fatals, because then a shortfall is the
// engine's fault.
func skipIfMemlockCaps589(t *testing.T, engType EngineType, workers int) {
	t.Helper()
	if engType != IOUring {
		return
	}
	maxW := iouring.MaxWorkersForMemlock()
	if maxW == -1 || maxW >= workers {
		return
	}
	// The byte figure in the hint mirrors engine/iouring's unexported
	// minMemlockPerWorker (12 MiB) and is advisory only — the GATE above is
	// the exported pre-flight, so a change to that constant cannot make the
	// rig skip or run wrongly, only make this hint generous or tight.
	skipOrFailIOUring592(t, "io_uring: RLIMIT_MEMLOCK allows %d worker(s), this rig needs %d "+
		"(raise it: `ulimit -l unlimited`, docker --ulimit memlock=%d, or systemd LimitMEMLOCK=infinity)",
		maxW, workers, workers*12*1024*1024)
}

// skipOrFailIOUring592 is the io_uring half's only way to skip. At a GitHub
// runner's 8 MiB the three io_uring subtests skip in every CI step
// (celeris#684). A step that raises memlock and sets
// CELERIS_REQUIRE_IOURING_WORKERS=1, as the `iouring` job does for
// engine/iouring's own worker tests, turns the skip into a failure, so that
// step cannot go green without running them.
func skipOrFailIOUring592(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatal(msg + " -- CELERIS_REQUIRE_IOURING_WORKERS=1 forbids skipping")
	}
	t.Skip(msg)
}

// memlockCeiling589 renders the pre-flight ceiling for the workers-shortfall
// Fatal, so the failure message says what the limit allowed rather than
// speculating about it.
func memlockCeiling589() string {
	if maxW := iouring.MaxWorkersForMemlock(); maxW != -1 {
		return strconv.Itoa(maxW) + " worker(s)"
	}
	return "unlimited workers (RLIM_INFINITY)"
}

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
	skipIfMemlockCaps589(t, engType, workers)

	// Freeze the adaptive promotion clock: a promotion made during the run
	// never expires, so the fixed/control worlds show exactly one inline run
	// per worker rather than one per TTL (review correction 1). The stub is an
	// atomic flip and is released by a t.Cleanup that runs after this frame's
	// defers, so releasing it can no longer race the engine's own clock reads
	// (celeris#620 fault 1); stopEngine below makes the ordering explicit as
	// well as safe.
	clock := stubNowNano(t)
	clock.Store(time.Now().UnixNano())

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

	// celeris#620 fault 1: every exit path from here on must JOIN the engine,
	// not merely cancel its context. The `defer cancel()` above only signals;
	// the event-loop goroutines keep serving — and keep reading the adaptive
	// clock and the router's sync.Maps — after the test frame is gone, which
	// is how a t.Fatalf anywhere below used to leave a live engine racing the
	// clock stub's restore and leak two workers into the next subtest.
	//
	// StartWithListenerAndContext returns only after Engine.Listen has
	// returned, and both native engines join their loops before that (epoll
	// and io_uring alike end Listen with `<-ctx.Done()` then `wg.Wait()`), so
	// receiving from startErr is a real barrier: no engine goroutine is left
	// running once it fires.
	//
	// Registered AFTER the clock stub, so the LIFO order on a t.Fatalf's
	// runtime.Goexit is: stop the engine, close the client conns, release the
	// clock (a t.Cleanup, which runs after every defer in this frame).
	engineStopped := false
	stopEngine := func() {
		if engineStopped {
			return
		}
		engineStopped = true
		cancel()
		select {
		case err := <-startErr:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Logf("Start returned after cancel: %v", err)
			}
		case <-time.After(30 * time.Second):
			// t.Errorf, not t.Fatalf: this also runs from a deferred call
			// during another failure's runtime.Goexit, where a second Goexit
			// would abandon the remaining defers and cleanups.
			t.Errorf("engine did not exit within 30 s of cancel")
		}
	}
	defer stopEngine()

	// Readiness on /ping over a throwaway conn; an early Start error (no
	// io_uring in this kernel/seccomp profile) is reported, not hidden.
	deadline := time.Now().Add(10 * time.Second)
	ready := false
	for time.Now().Before(deadline) && !ready {
		select {
		case err := <-startErr:
			// The only value startErr will ever carry has just been taken,
			// so stopEngine must not go on to wait 30 s for a second one.
			engineStopped = true
			if engType == IOUring {
				skipOrFailIOUring592(t, "io_uring engine failed to start (run with --security-opt seccomp=unconfined): %v", err)
			}
			t.Fatalf("engine failed to start: %v", err)
		default:
		}
		c, br, err := dial589(addr)
		if err == nil {
			if err = get589(c, br, "/ping"); err == nil {
				// A served /ping is NOT full readiness: the native engines
				// rebind per-worker SO_REUSEPORT sockets, so the FIRST worker
				// answers requests while EngineInfo().Metrics.Workers is still
				// 0 or 1. Measured once in 40 io_uring runs of the 20-run
				// celeris#592 campaign, where the worker-count precondition
				// below aborted a run on an engine that was in fact fine. Wait
				// for the whole worker set to be published before the run
				// starts; a genuine memlock cap still runs the deadline out
				// and fails the precondition.
				if info := s.EngineInfo(); info != nil && info.Metrics.Workers == workers {
					ready = true
				}
			}
			_ = c.Close()
		}
		if !ready {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !ready {
		t.Fatalf("server did not become ready (engine info %+v, want %d workers)", s.EngineInfo(), workers)
	}
	info := s.EngineInfo()
	if info == nil || info.Type != engType {
		t.Fatalf("engine type = %v, want %v (silent fallback would invalidate the run)", info, engType)
	}
	o.workers = info.Metrics.Workers
	if o.workers != workers {
		// skipIfMemlockCaps589 already cleared RLIMIT_MEMLOCK for this worker
		// count, so a shortfall here is the engine's own doing, not the
		// environment's: fail, do not skip.
		t.Fatalf("workers=%d, want %d (RLIMIT_MEMLOCK allows %s, so this is NOT the memlock cap)",
			o.workers, workers, memlockCeiling589())
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
				// Same jitter promotion as the learning control
				// (celeris#620 fault 2), and worse here: promoting also
				// CLEARS the fast streak and takes the route off the timed
				// path, so with the clock frozen the route could never
				// settle again and this loop would burn its whole bound
				// before failing the precondition. Undo it and let the
				// streak accumulate.
				if depromote589(s.router, "/kv") {
					o.depromotions++
				}
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
			// celeris#620 fault 2: the store is still FAST here, so a
			// promotion at this point is runner jitter (a warm-up run that
			// overran adaptiveBlockingThreshold), not a property of the tree
			// under test — and with the clock frozen it would never expire.
			// Undo it immediately so the remaining warm-up runs are timed
			// inline, which is what "still learning" means.
			if mode == stall589Learning && depromote589(s.router, "/kv") {
				o.depromotions++
			}
		}
	}
	_ = wc.Close()
	// Re-establish the learning control's precondition once more after the
	// last warm-up run, then assert it below: no /kv request runs between
	// here and the gate flip, so the state read next is the state the control
	// measures against.
	if mode == stall589Learning && depromote589(s.router, "/kv") {
		o.depromotions++
	}
	_, o.settledBefore = s.router.settled.Load("/kv")
	o.promotedBefore = s.router.isPromoted("/kv")
	switch mode {
	case stall589Settled:
		if !o.settledBefore || o.promotedBefore {
			t.Fatalf("precondition: /kv must be settled and not promoted after warm-up (settled=%t promoted=%t after %d runs)",
				o.settledBefore, o.promotedBefore, o.warmReqs)
		}
	case stall589Learning:
		// depromote589 has already undone any jitter promotion, so reaching
		// this Fatalf means the de-promotion itself did not take — a real
		// defect in the transition, not a loaded runner.
		if o.settledBefore || o.promotedBefore || !s.router.adaptiveLearning("/kv") {
			t.Fatalf("precondition: /kv must still be learning after %d runs (settled=%t promoted=%t, %d jitter promotions undone)",
				o.warmReqs, o.settledBefore, o.promotedBefore, o.depromotions)
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
	// celeris#628: every /kv request is timed client-side and kept with the
	// instant it completed, so the assertion can take the median over exactly
	// the /ping window. Each hammer fills a private slice and publishes it
	// under kvLatMu on the way out — no shared state on the hot path, and the
	// merge happens before wg.Wait() returns, so kvLat is final when read.
	var kvLatMu sync.Mutex
	var kvLat []kvSample589
	for i := 0; i < o.kvConns; i++ {
		c, br, err := dial589(addr)
		if err != nil {
			t.Fatalf("dial kv conn %d: %v", i, err)
		}
		defer func() { _ = c.Close() }()
		wg.Add(1)
		go func() {
			var mine []kvSample589
			defer func() {
				kvLatMu.Lock()
				kvLat = append(kvLat, mine...)
				kvLatMu.Unlock()
				wg.Done()
			}()
			for {
				select {
				case <-stop:
					return
				default:
				}
				t0 := time.Now()
				if err := get589(c, br, "/kv"); err != nil {
					hammerErr <- err
					return
				}
				done := time.Now()
				mine = append(mine, kvSample589{done: done, lat: done.Sub(t0)})
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
	// Diagnostics (test-only, env-gated): CELERIS_589_FRESH=1 samples /ping on
	// a FRESH conn per sample (random worker) instead of the pre-opened conn;
	// CELERIS_589_DUMP=1 logs the chronological latency series.
	fresh := os.Getenv("CELERIS_589_FRESH") != ""
	// One /ping sample, the same shape in both windows so the two medians are
	// comparable (celeris#622): the reference window and the post-promotion
	// window must differ only in the engine state they observe.
	pingOnce := func() (time.Duration, error) {
		t0 := time.Now()
		if fresh {
			fc, fbr, err := dial589(addr)
			if err != nil {
				return 0, err
			}
			err = get589(fc, fbr, "/ping")
			_ = fc.Close()
			if err != nil {
				return 0, err
			}
			return time.Since(t0), nil
		}
		if err := get589(pc, pbr, "/ping"); err != nil {
			return 0, err
		}
		return time.Since(t0), nil
	}

	// celeris#592: the settled classification is re-opened every
	// adaptiveSettleTTL, the re-timed run is ~D (far over
	// adaptiveBlockingThreshold) and the route promotes. Wait for that —
	// bounded by the design's bound — before the measured window opens, so the
	// numbers the assertion reads are the ones for the REST of the window,
	// after promotion. On a tree without the fix this loop runs out the bound
	// and the assertion fails on promoted_in_bound.
	//
	// celeris#622: SAMPLE /ping throughout that wait. Until the route is
	// promoted it is still settled and still inline on the worker, so this is
	// the same measurement the post-promotion window makes, taken seconds
	// earlier, by the same client loop, on the same box, against the same
	// blocked store — the pinned world. It is the denominator the
	// post-promotion median is judged against, which is what makes the arm's
	// latency observable a property of the engine rather than of the runner.
	// The sampler runs on its own goroutine so the 5 ms promotion poll keeps
	// its cadence; refLat is published to this goroutine by the refDone close.
	var refLat []time.Duration
	if mode == stall589Settled {
		stopRef := make(chan struct{})
		refDone := make(chan struct{})
		refErr := make(chan error, 1)
		go func() {
			defer close(refDone)
			for {
				select {
				case <-stopRef:
					return
				default:
				}
				d, err := pingOnce()
				if err != nil {
					select {
					case refErr <- err:
					default:
					}
					return
				}
				refLat = append(refLat, d)
				time.Sleep(stall589Spacing)
			}
		}()
		refStart := time.Now()
		promoteDeadline := flipAt.Add(stall592PromoteBound)
		// Raised only AFTER the reference sampler is joined: a t.Fatalf here
		// would otherwise leave that goroutine hammering the /ping conn this
		// frame's defers are about to close (celeris#620 fault 1's shape).
		var fatal error
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
				fatal = fmt.Errorf("kv hammer: %w", err)
			default:
			}
			if fatal != nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(stopRef)
		<-refDone
		o.refWindow = time.Since(refStart)
		if fatal == nil {
			select {
			case err := <-refErr:
				fatal = fmt.Errorf("GET /ping during the pre-promotion reference window: %w", err)
			default:
			}
		}
		if fatal != nil {
			t.Fatal(fatal)
		}
	}

	o.preSample = time.Since(flipAt)

	var lat []time.Duration
	winStart := time.Now()
	stackDiag := !fresh && os.Getenv("CELERIS_589_STACK") != ""
	for time.Since(winStart) < stall589Window {
		if stackDiag {
			// Diagnostic: 100 ms into EVERY stalled /ping, classify the
			// event-loop goroutines' blocking site (tallied on the STACK589
			// result line) and dump the first one in full.
			if o.stacks == nil {
				o.stacks = &stackTally589{}
			}
			t0 := time.Now()
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
			lat = append(lat, time.Since(t0))
		} else {
			// The same call the reference window made (celeris#622), so the
			// two medians differ only in the engine state they observed.
			d, err := pingOnce()
			if err != nil {
				t.Fatalf("GET /ping during window: %v", err)
			}
			lat = append(lat, d)
		}
		time.Sleep(stall589Spacing)
	}
	winEnd := time.Now()
	o.window = winEnd.Sub(winStart)
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
	o.queuedBar = queuedBar589(delay)
	for _, d := range lat {
		// The 5 ms tally is REPORTED, not asserted (celeris#622): it is the
		// series the #589 measurement branch published and the #592 campaign
		// was read on, so it stays on the result line for continuity and for
		// the defect-signature assertion, which needs 0.9 of the window and is
		// nowhere near a runner stall.
		if d > stall589StallBar {
			o.stalled++
			o.stalledTime += d
		}
		if d > o.queuedBar {
			o.queued++
		}
	}
	if o.samples > 0 {
		o.stalledFrac = float64(o.stalled) / float64(o.samples)
		o.queuedFrac = float64(o.queued) / float64(o.samples)
	}

	// The pinned-world reference (settled mode; empty in the controls, which
	// have no pre-promotion phase to sample).
	sort.Slice(refLat, func(i, j int) bool { return refLat[i] < refLat[j] })
	o.refSamples = len(refLat)
	if o.refSamples > 0 {
		o.refPingMed, o.refPingMax = refLat[o.refSamples/2], refLat[o.refSamples-1]
		for _, d := range refLat {
			if d > stall589StallBar {
				o.refStalled++
			}
			if d > o.queuedBar {
				o.refQueued++
			}
		}
		o.refQueuedFr = float64(o.refQueued) / float64(o.refSamples)
		if o.pingMed > 0 {
			o.speedup = float64(o.refPingMed) / float64(o.pingMed)
		}
	}

	// The controls' in-run reference (celeris#628): what one blocked Set cost
	// a client over the same window the /ping medians were taken in. Only
	// completions inside [winStart, winEnd] count, so a request that spanned
	// the pre-window wait (where the conns were still catching their first
	// slow run) cannot contribute. Computed for every mode — the settled arm
	// only prints it, its bar being the pinned /ping window.
	inWin := make([]time.Duration, 0, len(kvLat))
	for _, s := range kvLat {
		if !s.done.Before(winStart) && !s.done.After(winEnd) {
			inWin = append(inWin, s.lat)
		}
	}
	sort.Slice(inWin, func(i, j int) bool { return inWin[i] < inWin[j] })
	o.kvSamples = len(inWin)
	if o.kvSamples > 0 {
		o.kvMed, o.kvMax = inWin[o.kvSamples/2], inWin[o.kvSamples-1]
		if o.pingMed > 0 {
			o.ctlSpeedup = float64(o.kvMed) / float64(o.pingMed)
		}
	}

	// Orderly stop: client conns are closed by the deferred Close calls after
	// the engine has exited, so no handler is cut mid-frame by the rig.
	stopEngine()
	return o
}

// depromote589 returns an adaptive route from the promoted set to the timed
// learning path. It performs exactly the transition router.isPromoted performs
// when a promotion expires — drop the entry, clear the slow streak — and
// reports whether the route was promoted at all.
//
// The learning control (celeris#620 fault 2) needs it because the rig freezes
// the promotion clock, so the TTL that would normally undo a spurious
// promotion never fires: a single warm-up run over adaptiveBlockingThreshold
// on a loaded runner pins /kv to "promoted" for the whole run and the control
// aborts on its precondition before testing anything. Only the rig's own
// warm-up calls this, while the store is still fast and no other /kv request
// is in flight; the fast streak is deliberately left alone (the control's only
// requirement is that it stays below adaptiveSettleStreak, which 100 warm-up
// runs cannot reach).
func depromote589(rt *router, fullPath string) bool {
	if _, ok := rt.promoted.Load(fullPath); !ok {
		return false
	}
	rt.promoted.Delete(fullPath)
	if v, ok := rt.slowStreak.Load(fullPath); ok {
		v.(*atomic.Int32).Store(0)
	}
	return true
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
