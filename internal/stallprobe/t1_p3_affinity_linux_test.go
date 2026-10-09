//go:build linux

package stallprobe

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris"
)

// P3 AffinityA720. The large-response check (P2's client, celeris's 5 s
// no-byte rule) against a celeris server, one fresh child process per
// (case, engine, round), to find out whether the stalls depend on the engine's
// loop threads being pinned and on which CPUs they are pinned to.
//
// How the engines pick CPUs (internal/engine/{epoll,iouring}, internal/platform/
// cpu_linux.go): Workers defaults to GOMAXPROCS, and loop (or ring worker) i is
// pinned to CPU number DistributeWorkers(i) = i % NumCPU, a CPU INDEX 0..n-1,
// not a member of the process's mask; each loop thread is locked
// (runtime.LockOSThread) and pinned to that one CPU. A mask alone is therefore
// NOT honest: a process started on cpus {0,1,6-11} still pins loops 2-5 onto
// the A520s. So every case works on the loop THREADS:
//
//  1. the child process starts with the case's CPU set as its mask (the parent
//     sets its forking thread's mask for the fork only), so NumCPU and
//     GOMAXPROCS follow it and every Go thread of the client is inside it;
//  2. Config.Workers is the set's size (cases all and nopin: the default);
//  3. after Start returns, the loop threads are the threads of the process
//     (io_uring kernel threads "iou-*" excepted) that are pinned to exactly one
//     CPU and were not before Start. Their number must EQUAL the loop count the
//     engine reports (EngineInfo().Metrics.Workers). A different number, or a
//     thread whose affinity cannot be set or read back as requested, FAILS the
//     case loudly (CASE MISLABELLED) and its workload is not run: it would be
//     measured under a false label;
//  4. each loop thread is moved with sched_setaffinity on its tid: onto the
//     k-th CPU of the set (fast8, fast4, slow4), or, for nopin, back to the
//     FULL allowed mask, which undoes the engine's pinning and needs no
//     product-code edit. After every repetition the probe re-reads the masks
//     and moves any loop thread that appeared later (the adaptive engine builds
//     its standby lazily); late ones are counted and reported;
//  5. the thread table is printed before and after the workload.
//
// Cases (same names on every host; see topo.caseCPUs):
//
//	all    nothing changed: the shape of the failing run. This is the POSITIVE
//	       CONTROL: if it does not fail in this fresh-child setup, no comparison
//	       with it means anything, and the output says "UNINFORMATIVE: all did
//	       not fail" before any comparison.
//	nopin  loops unpinned. A pinned-thread wake loss (H1/H3) predicts ~0 stalls;
//	       a celeris defect (H2) predicts the stalls continue.
//	fast8  loops one-to-one on the 8 fastest-core-type CPUs (msr1: the A720s).
//	fast4  on the 4 fastest of those.
//	slow4  on the slowest core type (msr1: the A520s). fast4 next to slow4
//	       separates "slow cores" from "only 4 CPUs"; compare those two.
//
// Cases added after run 37975655016 (19 of 19 stall snapshots had the stuck
// loop pinned on a Cortex-A520, 0 of 19 on an A720) are described at
// topo.extraCase: they vary how many loops sit on the slow cores, whether
// fast CPUs without a loop are left to the client, and GOMAXPROCS.
//
// Engines: epoll, io_uring, adaptive (the three that failed) and std (control:
// no loop threads, the same process shape). io_uring is where 13 of the 25
// failing leaves of run 37949658832 were. For std, nopin is the same as all.
//
// Every cell (case, engine) runs CELERIS_PROBE_ROUNDS times in a fresh child,
// the cells in ABBA order (round 2 runs them in reverse), so a drift in the
// host (heat, a noisy neighbour) falls on every cell alike; REPS are split
// between the rounds.
//
// What the stalls look like from inside is in each failure's snapshot (see
// snapshot_linux_test.go). P3 does NOT cover: a per-CPU 1-4 ms canary running
// next to the workload (a CPU-level freeze that does not even serve its own
// timer shows in the snapshot's schedstat only for the loop threads' CPUs), a
// runtime/trace, GODEBUG=gctrace (the stop-the-world pause histograms of
// runtime/metrics stand in for them), or a kernel/cpuidle A/B (cpuidle usage
// is read, not changed).

type p3Leaf struct {
	Shape string
	leafResult
}

type p3Result struct {
	Pid        int
	Engine     string
	Case       string
	Round      int
	GoMaxProc  int
	NumCPU     int
	Allowed    string
	Workers    int
	Leaves     []p3Leaf
	WD         wdResult
	Notes      []string
	CaseErrs   []string // the case was not what its name says
	LoopsFound int
	LoopsWant  int
	LateLoops  int
}

func engineType(name string) (celeris.EngineType, error) {
	switch name {
	case "epoll":
		return celeris.Epoll, nil
	case "std":
		return celeris.Std, nil
	case "io_uring":
		return celeris.IOUring, nil
	case "adaptive":
		return celeris.Adaptive, nil
	}
	return 0, fmt.Errorf("unknown engine %q (epoll, io_uring, adaptive, std)", name)
}

// caseMode says what is done to the loop threads after Start.
type caseMode struct {
	Move  []int // loop k goes to Move[k % len]
	Unpin bool  // every loop gets the full allowed mask
	// Plan (the extra cases, topo.extraCase): loop k, the loops in the order of
	// the CPU the engine pinned them to, goes to Plan[k]; a negative entry
	// leaves that loop unpinned (full mask). Keep: a loop the engine pinned to
	// one of these CPUs stays; every other loop is unpinned.
	Plan    []int
	Keep    []int
	HasPlan bool
	HasKeep bool
}

func (m caseMode) moves() bool { return m.Unpin || len(m.Move) > 0 || m.HasPlan || m.HasKeep }

type loopThread struct {
	Tid  int
	From int // the CPU the engine pinned it to
	To   int // the CPU it was moved to, -1 when not moved to one CPU
	Want []int
	Late bool
}

func threadMask(tid int) []int {
	var s unix.CPUSet
	if err := unix.SchedGetaffinity(tid, &s); err != nil {
		return nil
	}
	return cpuList(&s)
}

func sameCPUs(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// loopSet is the probe's view of the engine's loop threads.
type loopSet struct {
	mode   caseMode
	full   []int
	loops  []loopThread
	known  map[int]bool // tids that are not loop candidates: present before Start, or a loop already
	errs   []string
	notes  []string
	late   int
	hard   bool // a loop that is not where the case says is an error (a moving case)
	posted map[string]bool
}

func (ls *loopSet) report(msg string, hard bool) {
	if ls.posted[msg] {
		return
	}
	ls.posted[msg] = true
	if hard && ls.hard {
		ls.errs = append(ls.errs, msg)
	} else {
		ls.notes = append(ls.notes, msg)
	}
}

// wanted is the mask loop number k must have.
func (ls *loopSet) wanted(k int, from int) []int {
	switch {
	case ls.mode.Unpin:
		return ls.full
	case ls.mode.HasPlan:
		if k < len(ls.mode.Plan) && ls.mode.Plan[k] >= 0 {
			return []int{ls.mode.Plan[k]}
		}
		return ls.full
	case ls.mode.HasKeep:
		for _, c := range ls.mode.Keep {
			if c == from {
				return []int{from}
			}
		}
		return ls.full
	case len(ls.mode.Move) > 0:
		return []int{ls.mode.Move[k%len(ls.mode.Move)]}
	}
	return []int{from}
}

// place applies the case to loop k and reads the mask back.
func (ls *loopSet) place(k int) {
	l := &ls.loops[k]
	l.Want = ls.wanted(k, l.From)
	if !ls.mode.moves() {
		return
	}
	m := maskOf(l.Want)
	if err := unix.SchedSetaffinity(l.Tid, &m); err != nil {
		ls.report(fmt.Sprintf("tid %d could not be moved to %s: %v", l.Tid, fmtCPUs(l.Want), err), true)
		return
	}
	if got := threadMask(l.Tid); !sameCPUs(got, l.Want) {
		ls.report(fmt.Sprintf("tid %d: asked for %s, the kernel reports %s (a cpuset narrows it?)", l.Tid, fmtCPUs(l.Want), fmtCPUs(got)), true)
	}
	if len(l.Want) == 1 {
		l.To = l.Want[0]
	}
}

// discover finds pinned threads that are not known yet and returns them.
func (ls *loopSet) discover() []loopThread {
	var fresh []loopThread
	for tid, cpu := range pinnedThreads() {
		if !ls.known[tid] {
			fresh = append(fresh, loopThread{Tid: tid, From: cpu, To: -1})
		}
	}
	sort.Slice(fresh, func(i, j int) bool {
		if fresh[i].From != fresh[j].From {
			return fresh[i].From < fresh[j].From
		}
		return fresh[i].Tid < fresh[j].Tid
	})
	return fresh
}

// check runs after every repetition: it re-reads the loop threads' masks and
// adopts (moves, in a moving case) the loop threads that appeared since.
func (ls *loopSet) check() {
	for k := range ls.loops {
		l := &ls.loops[k]
		got := threadMask(l.Tid)
		if got == nil {
			ls.report(fmt.Sprintf("loop thread %d is gone (the engine switched or shut a loop down)", l.Tid), false)
			continue
		}
		if !sameCPUs(got, l.Want) {
			ls.report(fmt.Sprintf("loop thread %d has mask %s, the case wants %s (something re-pinned it)", l.Tid, fmtCPUs(got), fmtCPUs(l.Want)), true)
		}
	}
	for _, f := range ls.discover() {
		ls.known[f.Tid] = true
		f.Late = true
		ls.loops = append(ls.loops, f)
		ls.late++
		ls.place(len(ls.loops) - 1)
		ls.report(fmt.Sprintf("LATE loop thread %d appeared after Start, pinned by the engine to cpu %d until the next check (adaptive builds its standby lazily); case applied to it", f.Tid, f.From), false)
		snapSetLoops(ls.loops)
	}
}

func (ls *loopSet) table() string {
	if len(ls.loops) == 0 {
		return "none found (no pinned loop threads)"
	}
	var parts []string
	for _, l := range ls.loops {
		parts = append(parts, fmt.Sprintf("tid %d engine-pinned cpu %d, now allowed %s%s", l.Tid, l.From, threadAllowed(l.Tid), map[bool]string{true: " (late)", false: ""}[l.Late]))
	}
	return strings.Join(parts, "; ")
}

type p3Started struct {
	srv      *p3Server
	addr     string
	ls       *loopSet
	notes    []string
	found    int
	want     int
	caseErrs []string
}

func p3Child() (any, error) {
	c := newClk()
	engName := os.Getenv("CELERIS_PROBE_ARG_ENGINE")
	et, err := engineType(engName)
	if err != nil {
		return nil, err
	}
	var mode caseMode
	if v := os.Getenv("CELERIS_PROBE_ARG_TARGET"); v != "" {
		if mode.Move, err = parseCPUList(v); err != nil {
			return nil, err
		}
	}
	mode.Unpin = os.Getenv("CELERIS_PROBE_ARG_UNPIN") == "1"
	if v := os.Getenv("CELERIS_PROBE_ARG_PLAN"); v != "" {
		mode.HasPlan = true
		for _, f := range strings.Split(v, ",") {
			if f == "u" {
				mode.Plan = append(mode.Plan, -1)
				continue
			}
			n, perr := strconv.Atoi(f)
			if perr != nil {
				return nil, fmt.Errorf("CELERIS_PROBE_ARG_PLAN %q: %v", v, perr)
			}
			mode.Plan = append(mode.Plan, n)
		}
	}
	if v, ok := os.LookupEnv("CELERIS_PROBE_ARG_KEEP"); ok {
		mode.HasKeep = true
		if mode.Keep, err = parseCPUList(v); err != nil {
			return nil, err
		}
	}
	workers := envInt("CELERIS_PROBE_ARG_WORKERS", 0)
	rounds := envInt("CELERIS_PROBE_ARG_ROUNDS", 1)
	sizes, err := sizesMiB()
	if err != nil {
		return nil, err
	}
	rule := ruleFromEnv(rounds)
	idle := time.Duration(envInt("CELERIS_PROBE_IDLE_MS", 5000)) * time.Millisecond
	tp := loadTopo()
	res := &p3Result{Pid: os.Getpid(), Engine: engName, Case: os.Getenv("CELERIS_PROBE_ARG_CASE"), Round: envInt("CELERIS_PROBE_ARG_ROUND", 1), GoMaxProc: runtime.GOMAXPROCS(0), NumCPU: runtime.NumCPU(), Allowed: fmtCPUs(tp.Allowed), Workers: workers}
	bodies := map[int][]byte{}
	maxMiB := 0
	for _, m := range sizes {
		bodies[m] = patterned(m << 20)
		maxMiB = max(maxMiB, m)
	}
	buf := make([]byte, maxMiB<<20)
	wd := startWatchdog(c)

	for _, shape := range envList("CELERIS_PROBE_SHAPES", "sync,async-route") {
		asyncServer, asyncRoute := false, false
		switch shape {
		case "sync":
		case "async-loop":
			asyncServer = true
		case "async-route":
			asyncRoute = true
		default:
			return nil, fmt.Errorf("unknown shape %q (sync, async-loop, async-route)", shape)
		}
		st, err := p3Start(et, engName, workers, asyncServer, asyncRoute, bodies, mode, tp)
		if st != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("[%s] %s", shape, strings.Join(st.notes, "\n    ")))
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", shape, err)
		}
		res.LoopsFound, res.LoopsWant = st.found, st.want
		if len(st.caseErrs) > 0 {
			for _, e := range st.caseErrs {
				res.CaseErrs = append(res.CaseErrs, fmt.Sprintf("[%s] %s", shape, e))
			}
			st.srv.stop()
			continue // never measure under a false label
		}
		snapSetLoops(st.ls.loops)
		for _, m := range sizes {
			l := runLeaf(c, fmt.Sprintf("%dMiB", m), m, st.addr, "/big?n="+strconv.Itoa(m), bodies[m], buf, rule, idle, func(int) { st.ls.check() })
			res.Leaves = append(res.Leaves, p3Leaf{Shape: shape, leafResult: l})
		}
		st.ls.check()
		res.Notes = append(res.Notes, fmt.Sprintf("[%s] loop threads after the workload: %s\n    io_uring kernel threads: %s", shape, st.ls.table(), ioThreads()))
		for _, n := range st.ls.notes {
			res.Notes = append(res.Notes, fmt.Sprintf("[%s] %s", shape, n))
		}
		for _, e := range st.ls.errs {
			res.CaseErrs = append(res.CaseErrs, fmt.Sprintf("[%s] %s", shape, e))
		}
		res.LateLoops += st.ls.late
		st.srv.stop()
	}
	res.WD = wd.Stop()
	return res, nil
}

type p3Server struct {
	s         *celeris.Server
	startDone chan error
}

func (p *p3Server) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = p.s.Shutdown(ctx)
	select {
	case <-p.startDone:
	case <-time.After(15 * time.Second):
	}
}

// p3Start starts one celeris server, finds its loop threads, checks their
// number against the engine's own count and applies the case to them.
func p3Start(et celeris.EngineType, engName string, workers int, asyncServer, asyncRoute bool, bodies map[int][]byte, mode caseMode, tp topo) (*p3Started, error) {
	st := &p3Started{}
	ls := &loopSet{mode: mode, full: append([]int(nil), tp.Allowed...), known: map[int]bool{}, hard: mode.moves(), posted: map[string]bool{}}
	st.ls = ls
	before := pinnedThreads()
	for tid := range before {
		ls.known[tid] = true
	}
	cfg := celeris.Config{Engine: et, AsyncHandlers: asyncServer, Workers: workers, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	retryUntil := time.Now().Add(30 * time.Second)
	for {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return st, err
		}
		st.addr = ln.Addr().String()
		_ = ln.Close()
		cfg.Addr = st.addr
		s := celeris.New(cfg)
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		big := s.GET("/big", func(c *celeris.Context) error {
			maybeInject()
			n, err := strconv.Atoi(c.Query("n"))
			if err != nil {
				return err
			}
			body, ok := bodies[n]
			if !ok {
				return fmt.Errorf("no body of %d MiB", n)
			}
			return c.Blob(http.StatusOK, "application/octet-stream", body)
		})
		if asyncRoute {
			big.Async()
		}
		st.srv = &p3Server{s: s, startDone: make(chan error, 1)}
		go func() { st.srv.startDone <- s.Start() }()
		err = waitPing(st.addr, st.srv.startDone)
		if err == nil {
			break
		}
		st.srv.stop()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(retryUntil) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		return st, fmt.Errorf("server did not start: %w", err)
	}

	// How many loops does the engine say it runs, and how many did we configure?
	cfgWant := workers
	if cfgWant == 0 {
		cfgWant = runtime.GOMAXPROCS(0)
	}
	st.want = -1
	if info := st.srv.s.EngineInfo(); info != nil {
		st.want = info.Metrics.Workers
		st.notes = append(st.notes, fmt.Sprintf("engine requested %v, EngineInfo reports %v", et, info.Type))
		if info.Type != et {
			// e.g. io_uring unavailable and the server fell back: every label of this cell would be false.
			st.caseErrs = append(st.caseErrs, fmt.Sprintf("ENGINE MISLABELLED: %v was requested and %v is running", et, info.Type))
		}
	}
	switch {
	case engName == "std":
		st.want = 0 // net/http has no loop threads
	case st.want < 0:
		st.notes = append(st.notes, "EngineInfo() is nil: the loop count is taken from the configuration")
		st.want = cfgWant
	case st.want != cfgWant:
		st.notes = append(st.notes, fmt.Sprintf("the engine runs %d loops, %d were configured (io_uring caps its workers by RLIMIT_MEMLOCK; adaptive counts only the built engine)", st.want, cfgWant))
	}

	// The loop threads: pinned to exactly one CPU now, not before Start, not an
	// io_uring kernel thread. Wait for the engine's count, no longer than 5 s.
	if engName != "std" {
		deadline := time.Now().Add(5 * time.Second)
		for {
			fresh := ls.discover()
			if len(fresh) >= st.want || time.Now().After(deadline) {
				ls.loops = fresh
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if envInt("CELERIS_PROBE_INJECT_LOOPMISS", 0) == 1 && len(ls.loops) > 0 {
			// Proof knob, off by default: pretend discovery missed a loop thread, to show that a moving case fails loudly.
			ls.loops = ls.loops[1:]
		}
		for _, l := range ls.loops {
			ls.known[l.Tid] = true
		}
	}
	st.found = len(ls.loops)
	st.notes = append(st.notes, fmt.Sprintf("loops: the engine reports %d, %d pinned loop threads found; io_uring kernel threads: %s", st.want, st.found, ioThreads()))
	st.notes = append(st.notes, "loop threads as the engine pinned them: "+ls.table())
	if st.found != st.want && engName != "std" {
		msg := fmt.Sprintf("CASE MISLABELLED: %d loop threads found, the engine reports %d", st.found, st.want)
		if mode.moves() {
			st.caseErrs = append(st.caseErrs, msg)
		} else {
			st.notes = append(st.notes, "NOTE (case all moves nothing, so the label stays true): "+msg)
		}
	}
	if engName == "std" && len(pinnedThreads()) > len(before) {
		st.notes = append(st.notes, "NOTE: std started pinned threads, which are not located")
	}
	for k := range ls.loops {
		ls.place(k)
	}
	if mode.moves() && len(ls.loops) > 0 {
		var mv []string
		for _, l := range ls.loops {
			mv = append(mv, fmt.Sprintf("tid %d cpu %d -> %s", l.Tid, l.From, fmtCPUs(l.Want)))
		}
		st.notes = append(st.notes, "loop threads placed by the case: "+strings.Join(mv, "; "))
		st.notes = append(st.notes, fmt.Sprintf("full allowed mask is %s, online CPUs are %s", fmtCPUs(tp.Allowed), fmtCPUs(tp.Online)))
	}
	st.caseErrs = append(st.caseErrs, ls.errs...)
	ls.errs = nil
	if mode.moves() && len(st.caseErrs) == 0 {
		// The ping was served before the move; make sure the server answers after it.
		if err := waitPing(st.addr, nil); err != nil {
			st.caseErrs = append(st.caseErrs, fmt.Sprintf("no answer after the move: %v", err))
		}
	}
	return st, nil
}

func waitPing(addr string, startDone <-chan error) error {
	probe := &http.Client{Timeout: 300 * time.Millisecond}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if startDone != nil {
			select {
			case err := <-startDone:
				if err == nil {
					err = fmt.Errorf("Start returned nil before the server was ready")
				}
				return err
			default:
			}
		}
		if resp, err := probe.Get("http://" + addr + "/ping"); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("no answer on /ping at %s within 15s", addr)
}

// ---- the parent -----------------------------------------------------------------------

type p3Cell struct {
	Case, Engine string
	Round        int
	Res          *p3Result
	Err          error
	Stderr       string
	Took         time.Duration
}

// cellKey names a (case, engine) pair.
type cellKey struct{ Case, Engine string }

type cellTotals struct {
	leafStats
	Children, Broken int
	CaseErrs         []string
}

func (c *p3Cell) totals() leafStats {
	var s leafStats
	if c.Res == nil {
		return s
	}
	for _, l := range c.Res.Leaves {
		x := l.stats()
		s.Transfers += x.Transfers
		s.Failed += x.Failed
		s.Stalled += x.Stalled
		s.Snapped += x.Snapped
	}
	return s
}

// fisher is the two-sided Fisher exact p value for a of n1 versus b of n2.
func fisher(a, n1, b, n2 int) float64 {
	k, n := a+b, n1+n2
	if n1 == 0 || n2 == 0 || k == 0 || k == n {
		return 1
	}
	lc := func(n, r int) float64 {
		x, _ := math.Lgamma(float64(n + 1))
		y, _ := math.Lgamma(float64(r + 1))
		z, _ := math.Lgamma(float64(n - r + 1))
		return x - y - z
	}
	pr := func(x int) float64 { return math.Exp(lc(k, x) + lc(n-k, n1-x) - lc(n, n1)) }
	p0 := pr(a)
	var p float64
	for x := max(0, k-n2); x <= min(k, n1); x++ {
		if q := pr(x); q <= p0*(1+1e-7) {
			p += q
		}
	}
	return math.Min(p, 1)
}

func isNative(eng string) bool { return eng == "epoll" || eng == "io_uring" || eng == "adaptive" }

// controlReport builds the positive-control and power block. It is the first
// thing to read, and it is printed again after each all/<engine> cell.
func controlReport(cells []*p3Cell, cases, engines []string) string {
	return controlBlock(cells, cases, engines, false)
}

// controlBlock builds the block; short gives only the verdict lines (used
// while the children are still running, when "so far" is all that is known).
func controlBlock(cells []*p3Cell, cases, engines []string, short bool) string {
	tot := map[cellKey]*cellTotals{}
	for _, c := range cells {
		k := cellKey{c.Case, c.Engine}
		t := tot[k]
		if t == nil {
			t = &cellTotals{}
			tot[k] = t
		}
		s := c.totals()
		t.Transfers += s.Transfers
		t.Failed += s.Failed
		t.Stalled += s.Stalled
		t.Snapped += s.Snapped
		t.Children++
		if c.Err != nil || c.Res == nil {
			t.Broken++
		} else {
			t.CaseErrs = append(t.CaseErrs, c.Res.CaseErrs...)
		}
	}
	var b strings.Builder
	if short {
		b.WriteString("P3 POSITIVE CONTROL SO FAR (the final block comes after the last child)\n")
	} else {
		b.WriteString("P3 POSITIVE CONTROL AND POWER (read this before any comparison)\n")
		fmt.Fprintf(&b, "  %-9s %-6s %9s %7s %12s %9s  %s\n", "engine", "case", "transfers", "failed", "stalled>=1s", "snapshots", "state")
	}
	for _, e := range engines {
		if short {
			break
		}
		for _, cs := range cases {
			t := tot[cellKey{cs, e}]
			if t == nil {
				continue
			}
			state := "ok"
			switch {
			case t.Broken > 0:
				state = fmt.Sprintf("%d child(ren) FAILED to run", t.Broken)
			case len(t.CaseErrs) > 0:
				state = "CASE MISLABELLED: no workload was run"
			case t.Transfers == 0:
				state = "no transfers"
			}
			fmt.Fprintf(&b, "  %-9s %-6s %9d %7d %12d %9d  %s\n", e, cs, t.Transfers, t.Failed, t.Stalled, t.Snapped, state)
		}
	}
	anyAll, anyAllFail, anyAllStall := false, false, false
	var perEngine []string
	for _, e := range engines {
		if !isNative(e) {
			continue
		}
		t := tot[cellKey{"all", e}]
		if t == nil {
			continue
		}
		anyAll = true
		switch {
		case t.Failed > 0:
			anyAllFail = true
			perEngine = append(perEngine, fmt.Sprintf("  %s: CONTROL OK, case all failed %d of %d transfers (%d stalled >= %v)", e, t.Failed, t.Transfers, t.Stalled, pcfg.Slow))
		case t.Stalled > 0:
			anyAllStall = true
			perEngine = append(perEngine, fmt.Sprintf("  %s: UNINFORMATIVE: all did not fail (0 of %d transfers; %d stalled >= %v but passed: a weak control)", e, t.Transfers, t.Stalled, pcfg.Slow))
		default:
			perEngine = append(perEngine, fmt.Sprintf("  %s: UNINFORMATIVE: all did not fail (0 of %d transfers, none stalled >= %v)", e, t.Transfers, pcfg.Slow))
		}
	}
	switch {
	case !anyAll:
		b.WriteString("NO POSITIVE CONTROL: case all was not run for a native engine (epoll, io_uring, adaptive); no comparison below can be read.\n")
	case !anyAllFail:
		b.WriteString("UNINFORMATIVE: all did not fail\n")
		b.WriteString("  case all, the failing run's shape, did not reproduce a failure in this fresh-child setup for any native engine; a masked or unpinned case that also passes says nothing about the cause.\n")
		if anyAllStall {
			b.WriteString("  (some transfers stalled for 1 s or more and passed; they are the only signal left to compare.)\n")
		}
	default:
		b.WriteString("CONTROL OK: case all reproduced failures for at least one native engine (per engine below); comparisons are read per engine, against that engine's own all.\n")
	}
	for _, l := range perEngine {
		b.WriteString(l + "\n")
	}
	if short {
		return b.String()
	}
	b.WriteString("COMPARISONS against case all of the same engine, in this run (two-sided Fisher exact test on failed transfers; small counts give no power: a p above 0.05 means the data cannot tell the cases apart, not that they are equal):\n")
	for _, e := range engines {
		if !isNative(e) {
			continue
		}
		base := tot[cellKey{"all", e}]
		for _, cs := range cases {
			if cs == "all" {
				continue
			}
			t := tot[cellKey{cs, e}]
			if t == nil || base == nil || t.Transfers == 0 || base.Transfers == 0 {
				continue
			}
			p := fisher(t.Failed, t.Transfers, base.Failed, base.Transfers)
			pst := fisher(t.Stalled, t.Transfers, base.Stalled, base.Transfers)
			note := ""
			switch {
			case base.Failed == 0:
				note = "  [UNINFORMATIVE: all did not fail]"
			case base.Failed < 5:
				note = "  [WEAK: all failed fewer than 5 transfers]"
			case p < 0.05 && t.Failed*base.Transfers < base.Failed*t.Transfers:
				note = "  [fewer failures than all]"
			case p < 0.05:
				note = "  [MORE failures than all]"
			default:
				note = "  [not distinguishable from all]"
			}
			fmt.Fprintf(&b, "  %-9s %-6s failed %d/%d vs all %d/%d  p=%.3g; stalled>=1s %d/%d vs %d/%d p=%.3g%s\n", e, cs, t.Failed, t.Transfers, base.Failed, base.Transfers, p, t.Stalled, t.Transfers, base.Stalled, base.Transfers, pst, note)
		}
	}
	return b.String()
}

func cellOrder(cases, engines []string, rounds int) []*p3Cell {
	var pairs []cellKey
	for _, cs := range cases {
		for _, e := range engines {
			pairs = append(pairs, cellKey{cs, e})
		}
	}
	var out []*p3Cell
	for r := 1; r <= rounds; r++ {
		for i := range pairs {
			p := pairs[i]
			if r%2 == 0 {
				p = pairs[len(pairs)-1-i] // ABBA
			}
			out = append(out, &p3Cell{Case: p.Case, Engine: p.Engine, Round: r})
		}
	}
	return out
}

func TestProbeP3Affinity(t *testing.T) {
	tp := loadTopo()
	logf(t, "host topology:\n%s", tp.describe())
	cases := envList("CELERIS_PROBE_CASES", "all,nopin,fast8,fast4,slow4")
	engines := envList("CELERIS_PROBE_ENGINES", "epoll,io_uring,adaptive,std")
	shapes := envList("CELERIS_PROBE_SHAPES", "sync,async-route")
	sizes, err := sizesMiB()
	if err != nil {
		t.Fatal(err)
	}
	rounds := max(envInt("CELERIS_PROBE_ROUNDS", 2), 1)
	rule := ruleFromEnv(rounds)
	idleMs := envInt("CELERIS_PROBE_IDLE_MS", 5000)
	for _, e := range engines {
		if _, err := engineType(e); err != nil {
			t.Fatal(err)
		}
	}
	caseCPUs := map[string][]int{}
	caseSpecs := map[string]caseSpec{}
	for _, cs := range cases {
		var cpus []int
		var note string
		if sp, ok, err := tp.extraCase(cs); err != nil {
			t.Fatalf("%v", err)
		} else if ok {
			caseSpecs[cs] = sp
			cpus, note = sp.CPUs, sp.Note
		} else if cpus, note, err = tp.caseCPUs(cs); err != nil {
			t.Fatalf("%v", err)
		}
		caseCPUs[cs] = cpus
		var cl []string
		for _, c := range cpus {
			cl = append(cl, fmt.Sprintf("%d(%s,cap %d)", c, tp.CPU[c].class(), tp.CPU[c].Cap))
		}
		logf(t, "case %s: %d CPUs, %s: %s", cs, len(cpus), note, strings.Join(cl, " "))
	}
	// A child's budget: every leaf at most 2*maxStalls stalled reps of idle+2 s, the rest of the reps at 1 s.
	budget := time.Duration(len(shapes)*len(sizes)*(rule.Reps+2*rule.MaxStalls*(idleMs/1000+3))+240) * time.Second
	logf(t, "plan: %d cases x %d engines x %d rounds = %d children; per child %d shapes x %d sizes x up to %d reps (stall budget %d after %d reps, hard stop at %d); a child may take up to %v",
		len(cases), len(engines), rounds, len(cases)*len(engines)*rounds, len(shapes), len(sizes), rule.Reps, rule.MaxStalls, rule.MinReps, 2*rule.MaxStalls, budget)

	// Phase 1: run every child. Nothing is judged yet; the control line is
	// printed as soon as a case-all cell returns, so a later timeout does not
	// take it with it.
	cells := cellOrder(cases, engines, rounds)
	for i, cell := range cells {
		cpus := caseCPUs[cell.Case]
		env := []string{
			"CELERIS_PROBE_ARG_CASE=" + cell.Case,
			"CELERIS_PROBE_ARG_ENGINE=" + cell.Engine,
			"CELERIS_PROBE_ARG_ROUND=" + strconv.Itoa(cell.Round),
			"CELERIS_PROBE_ARG_ROUNDS=" + strconv.Itoa(rounds),
		}
		var mask []int
		sp, isSpec := caseSpecs[cell.Case]
		switch {
		case isSpec:
			mask = sp.Mask
			if sp.Workers > 0 {
				env = append(env, "CELERIS_PROBE_ARG_WORKERS="+strconv.Itoa(sp.Workers))
			}
			if sp.GOMAXPROCS > 0 {
				env = append(env, "GOMAXPROCS="+strconv.Itoa(sp.GOMAXPROCS))
			}
			if sp.Plan != nil {
				var ps []string
				for _, c := range sp.Plan {
					if c < 0 {
						ps = append(ps, "u")
					} else {
						ps = append(ps, strconv.Itoa(c))
					}
				}
				env = append(env, "CELERIS_PROBE_ARG_PLAN="+strings.Join(ps, ","))
			}
			if sp.Keep != nil {
				env = append(env, "CELERIS_PROBE_ARG_KEEP="+fmtCPUs(sp.Keep))
			}
		case cell.Case == "all":
		case cell.Case == "nopin":
			env = append(env, "CELERIS_PROBE_ARG_UNPIN=1")
		default:
			mask = cpus
			env = append(env, "CELERIS_PROBE_ARG_TARGET="+fmtCPUs(cpus), "CELERIS_PROBE_ARG_WORKERS="+strconv.Itoa(len(cpus)))
		}
		// Runtime knobs of the child, for A/B runs of the same case (run 37975655016
		// could not exclude the Go garbage collector's stack scans or async
		// preemption as the other party of the stall):
		//   CELERIS_PROBE_CHILD_GOGC=<n|off>   GOGC of the child
		//   CELERIS_PROBE_CHILD_NOASYNC=1      GODEBUG=asyncpreemptoff=1 in the child
		if v := os.Getenv("CELERIS_PROBE_CHILD_GOGC"); v != "" {
			env = append(env, "GOGC="+v)
		}
		if os.Getenv("CELERIS_PROBE_CHILD_NOASYNC") == "1" {
			env = append(env, "GODEBUG=asyncpreemptoff=1")
		}
		t0 := time.Now()
		so, se, err := runChild(childSpec{kind: "p3", env: env, mask: mask, timeout: budget})
		cell.Took = time.Since(t0)
		cell.Stderr = tailLines(se, 25)
		if err != nil {
			cell.Err = err
		} else {
			var res p3Result
			if derr := decodeChild(so, &res); derr != nil {
				cell.Err = derr
			} else {
				cell.Res = &res
			}
		}
		s := cell.totals()
		logf(t, "child %d/%d: %s/%s round %d took %v: %d transfers, %d failed, %d stalled >= %v, %d snapshots%s",
			i+1, len(cells), cell.Case, cell.Engine, cell.Round, cell.Took.Round(time.Second), s.Transfers, s.Failed, s.Stalled, pcfg.Slow, s.Snapped,
			map[bool]string{true: fmt.Sprintf("; CHILD FAILED: %v", cell.Err), false: ""}[cell.Err != nil])
		if cell.Case == "all" && isNative(cell.Engine) {
			var done []*p3Cell
			for _, c := range cells[:i+1] {
				if c.Case == "all" {
					done = append(done, c)
				}
			}
			logf(t, "%s", controlBlock(done, []string{"all"}, engines, true))
		}
	}

	// Phase 2: the control block, before any leaf is judged.
	report := controlReport(cells, cases, engines)
	logf(t, "%s", report)
	t.Run("control", func(t *testing.T) { logf(t, "%s", report) })

	// Phase 3: the leaves.
	for _, cs := range cases {
		t.Run(cs, func(t *testing.T) {
			for _, eng := range engines {
				t.Run(eng, func(t *testing.T) {
					var mine []*p3Cell
					for _, c := range cells {
						if c.Case == cs && c.Engine == eng {
							mine = append(mine, c)
						}
					}
					reportP3Cell(t, mine)
				})
			}
		})
	}
}

// reportP3Cell logs one (case, engine) cell, all its rounds, and reports its
// leaves merged over the rounds.
func reportP3Cell(t *testing.T, rounds []*p3Cell) {
	t.Helper()
	type key struct{ Shape, Name string }
	merged := map[key]*leafResult{}
	var order []key
	for _, c := range rounds {
		if c.Err != nil {
			t.Errorf("round %d: the child failed: %v\n%s", c.Round, c.Err, c.Stderr)
			continue
		}
		res := c.Res
		logf(t, "round %d: child pid %d: GOMAXPROCS %d, NumCPU %d, allowed CPUs %s, Workers %d (0 = GOMAXPROCS), loops found %d of %d the engine reports, late loops %d\n%s\nwatchdog over the whole child: max oversleep %s ms (%d sleeps)",
			c.Round, res.Pid, res.GoMaxProc, res.NumCPU, res.Allowed, res.Workers, res.LoopsFound, res.LoopsWant, res.LateLoops, strings.Join(res.Notes, "\n"), ms(res.WD.MaxNs), res.WD.Sleeps)
		for _, e := range res.CaseErrs {
			t.Errorf("round %d: CASE MISLABELLED (leaves not run, or run while a loop was not where the case says): %s", c.Round, e)
		}
		for _, l := range res.Leaves {
			l.leafResult.applyWD(res.WD)
			k := key{l.Shape, l.Name}
			m := merged[k]
			if m == nil {
				cp := l.leafResult
				cp.Reps, cp.STW, cp.WDMax, cp.WD50, cp.WD500 = nil, "", 0, 0, 0
				m = &cp
				merged[k] = m
				order = append(order, k)
			}
			for _, r := range l.Reps {
				r.Rep = len(m.Reps) + 1
				m.Reps = append(m.Reps, r)
			}
			m.Stopped = m.Stopped || l.Stopped
			m.StartNs, m.EndNs = min(m.StartNs, l.StartNs), max(m.EndNs, l.EndNs)
			if k := fmt.Sprintf("round %d: %s", c.Round, l.STW); m.STW == "" {
				m.STW = k
			} else {
				m.STW += " | " + k
			}
			m.WDMax = max(m.WDMax, l.WDMax)
			m.WD50 += l.WD50
			m.WD500 += l.WD500
		}
	}
	byShape := map[string][]key{}
	var shapes []string
	for _, k := range order {
		if _, ok := byShape[k.Shape]; !ok {
			shapes = append(shapes, k.Shape)
		}
		byShape[k.Shape] = append(byShape[k.Shape], k)
	}
	for _, sh := range shapes {
		t.Run(sh, func(t *testing.T) {
			for _, k := range byShape[sh] {
				t.Run(k.Name, func(t *testing.T) { reportLeaf(t, *merged[k]) })
			}
		})
	}
}
