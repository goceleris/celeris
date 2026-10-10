//go:build linux

package stallprobe

// E4: a minimal reproducer with no celeris in it.
//
//	go test -run '^TestProbeE4MinRepro$'    (celeris-stress: -f run='^TestProbeE4MinRepro$')
//
// A child process runs N goroutines, each LockOSThread + sched_setaffinity to one
// CPU, looping on unix.EpollWait(idle epfd, 1 ms) with a little pure-Go work after
// every wake-up (the work keeps the goroutine _Grunning part of the time: a
// goroutine that is only ever in a syscall is claimed by the GC's root scan
// without a signal, and the probe would test nothing), and M allocator goroutines
// that produce pointer-bearing garbage at a steady rate over a live heap so that
// GC cycles keep running. The process CPU mask is every allowed CPU in every case
// (the "slow4wide" shape that stalled 19/84 in the celeris probe), except the
// optional case slowconf.
//
// Cases (subtests, same names on every host): the loops are on the slow core type
// (slow: msr1's 4 Cortex-A520), the fast core type (fast: the 8 A720), or every
// allowed CPU (all: 12); on a host with one core type slow is the 4 lowest and
// fast the 8 highest CPU ids. Modes: default, and noasync (GODEBUG=asyncpreemptoff=1,
// the cure). Subtest names are <case>-<mode> (+ -rN for round N of several).
//
// The user work per wake-up (CELERIS_PROBE_E4_WORK_US, 20) is a fixed iteration
// count calibrated to take that long on the FASTEST core, so a slower core spends
// proportionally more time _Grunning per wake-up, as the A520 loops of the celeris
// probe did (49 us of CPU per wake-up against 4.2 us on an A720); WORK_MODE=time
// removes that factor (the same wall time on every core).
//
// Per loop the table prints the longest gap between two wake-ups that are not
// EINTR ("wake gap": the 1 ms timeout does not fire while signals keep
// restarting it) and between two returns of any kind ("any gap": the thread is
// not running at all), including the gap still open when the run ends, the gap
// counts, the time in state R, the thread's CPU split and its context switches,
// plus the process's GC cycles and stop-the-world pause counters (the same
// readSTW as the celeris probe).
//
// In a real episode the GC cannot finish its mark phase and every allocating
// goroutine is parked (on msr1 an allocation completed 41.8 s late), which includes
// this child's own reporting code. So the child also runs a heartbeat goroutine
// that never allocates (TestProbeE4HeartbeatAllocs holds it to 0 allocations): once
// a second it writes the per-loop open gap, longest gaps and EINTR counts to stderr.
// A child that does not finish within SECS+GRACE_S is killed, the subtest FAILS
// (default mode) and the parent prints the last heartbeat lines as the gap table.
// When a loop's open gap passes 200 ms the child also takes two /proc snapshots of
// every thread 300 ms apart and logs the threads that used CPU in between (the
// spinner of the suspendG theory is an R thread with sys+user CPU and no voluntary
// switches); the snapshot allocates, so it says when it actually ran and flags
// LATE or EPISODE ALREADY OVER. A default-mode loop with a wake gap above
// CELERIS_PROBE_E4_FAIL_MS (1000) fails the subtest. noasync results are logged,
// never failed (a gap there is logged as CONTROL VIOLATED). Proof knobs, off by
// default: INJECT_HANG_MS (loop 0 sleeps once, so a default run must FAIL on the
// gap) and INJECT_WEDGE=1 (the child never reports, so the parent must time it out
// and still print the heartbeat table).

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type e4node struct {
	next *e4node
	_    [56]byte
}

type e4LoopRes struct {
	CPU, Tid             int
	Wakes, Eintr, OthErr uint64
	GapWakeNs, GapAnyNs  int64
	Gap50, Gap500, Gap1s uint64
	OpenWakeNs           int64
	RSamples, Samples    int
	Ut, St, Vol, Invol   int64
	Exited               bool
	PinErr               string
}

type e4Result struct {
	Facts      string
	Work       string
	Secs       float64
	GOGC       string
	GoDebug    string
	GoMaxProcs int
	Loops      []e4LoopRes
	Cycles     uint64
	STW        string
	AllocMiB   float64
	Snapshots  []string
	Unfinished int
}

func e4Child() (any, error) {
	c := newClk()
	cpus, err := parseCPUList(os.Getenv("CELERIS_PROBE_ARG_E4_CPUS"))
	if err != nil || len(cpus) == 0 {
		return nil, fmt.Errorf("CELERIS_PROBE_ARG_E4_CPUS: %v", err)
	}
	secs := envInt("CELERIS_PROBE_E4_SECS", 30)
	workNs := int64(envInt("CELERIS_PROBE_E4_WORK_US", 20)) * 1000
	nAlloc := max(envInt("CELERIS_PROBE_E4_ALLOCS", 4), 1)
	allocBps := float64(envInt("CELERIS_PROBE_E4_ALLOC_MBS", 256)) * (1 << 20)
	liveBytes := envInt("CELERIS_PROBE_E4_LIVE_MB", 32) << 20
	res := &e4Result{Facts: hostFacts(), GOGC: os.Getenv("GOGC"), GoDebug: os.Getenv("GODEBUG"), GoMaxProcs: runtime.GOMAXPROCS(0)}

	// The live heap: pointer-bearing, partitioned between the allocators.
	liveN := liveBytes / 64
	live := make([]*e4node, liveN)
	for i := range live {
		live[i] = new(e4node)
	}

	var stop atomic.Bool
	base := readSTW()
	workIters := 0
	if os.Getenv("CELERIS_PROBE_E4_WORK_MODE") != "time" {
		fast := envInt("CELERIS_PROBE_ARG_E4_FASTCPU", cpus[0])
		type cal struct {
			n  int
			ns float64
		}
		out := make(chan cal, 1)
		go func() {
			_ = pinThis(fast)
			n, ns := calibrateIters(c, workNs)
			out <- cal{n, ns}
		}()
		cl := <-out
		workIters = cl.n
		res.Work = fmt.Sprintf("user work per wake-up: %d iterations = %d us on cpu %d (the fastest core, %.2f ns/iteration); a slower core takes proportionally longer", cl.n, workNs/1000, fast, cl.ns)
	} else {
		res.Work = fmt.Sprintf("user work per wake-up: %d us of wall time on every core", workNs/1000)
	}
	loops, err := startLoops(c, cpus, workNs, workIters, &stop)
	if err != nil {
		stop.Store(true)
		return nil, err
	}
	hb := &e4Heartbeat{c: c, loops: loops, t0: c.now()}
	go hb.run()

	var allocated atomic.Uint64
	var allocDone atomic.Int32
	for a := 0; a < nAlloc; a++ {
		lo, hi := a*liveN/nAlloc, (a+1)*liveN/nAlloc
		go func() {
			defer allocDone.Add(1)
			perSec := allocBps / float64(nAlloc)
			t0 := c.now()
			var did float64
			j := 0
			for !stop.Load() {
				allowed := perSec * float64(c.now()-t0) / 1e9
				if allowed-did > 8<<20 {
					did = allowed - 8<<20 // no catch-up burst after a stall
				}
				if did >= allowed {
					time.Sleep(200 * time.Microsecond)
					continue
				}
				var head *e4node
				for range 1024 { // 64 KiB per batch
					head = &e4node{next: head}
				}
				did += 1024 * 64
				allocated.Add(1024 * 64)
				j++
				if j%16 == 0 && hi > lo { // churn the live set: the write barrier is busy during mark
					live[lo+(j/16)%(hi-lo)] = &e4node{next: head.next.next} // a short chain: the old one becomes garbage
				}
			}
		}()
	}

	// Proof knob, off by default: loop 0 stops for INJECT_HANG_MS right after INJECT_AT_S seconds
	// (a plain sleep in the loop, no signal). A default-mode run with it must FAIL on the wake gap:
	// the detection, the snapshot and the failure path are exercised without the real stall.
	if hang := envInt("CELERIS_PROBE_E4_INJECT_HANG_MS", 0); hang > 0 {
		at := time.Duration(envInt("CELERIS_PROBE_E4_INJECT_AT_S", 2)) * time.Second
		go func() {
			time.Sleep(at)
			loops[0].Hang.Store(int64(hang) * 1_000_000)
		}()
	}

	// monitor: R-state samples, stall snapshots
	rs := make([]int, len(loops))
	ns := 0
	statA := make([]thrStat, len(loops))
	for i, l := range loops {
		statA[i] = readThr(int(l.Tid.Load()))
	}
	t0 := c.now()
	end := t0 + int64(secs)*1_000_000_000
	var snapped bool
	nextSample := t0
	for c.now() < end {
		time.Sleep(50 * time.Millisecond)
		now := c.now()
		if now >= nextSample {
			nextSample += 250_000_000
			ns++
			for i, l := range loops {
				if readThr(int(l.Tid.Load())).State == 'R' {
					rs[i]++
				}
			}
		}
		if !snapped && len(res.Snapshots) < 2 {
			for i, l := range loops {
				if now-l.LastWake.Load() > 200_000_000 {
					snap := e4Snapshot(c, i, l, l.LastWake.Load(), t0)
					res.Snapshots = append(res.Snapshots, snap)
					fmt.Fprintln(os.Stderr, snap) // the parent keeps stderr, so a child that never finishes still leaves it
					snapped = true
					break
				}
			}
		} else if snapped {
			// allow one more snapshot later in the run, once the stall is over
			over := true
			for _, l := range loops {
				if now-l.LastWake.Load() > 100_000_000 {
					over = false
				}
			}
			if over {
				snapped = false
			}
		}
	}
	if os.Getenv("CELERIS_PROBE_E4_INJECT_WEDGE") == "1" {
		// proof knob: the main goroutine never reports, so the parent must time the child out
		// and still print the heartbeat table
		for {
			time.Sleep(time.Hour)
		}
	}
	stop.Store(true)
	tEnd := c.now()
	// Read the stats now; do not wait for stuck loops.
	for i, l := range loops {
		openWake := tEnd - l.LastWake.Load()
		r := e4LoopRes{
			CPU: l.CPU, Tid: int(l.Tid.Load()), Wakes: l.Wakes.Load(), Eintr: l.Eintr.Load(), OthErr: l.OtherErr.Load(),
			GapWakeNs: max(l.MaxGapWake.Load(), openWake), GapAnyNs: max(l.MaxGapAny.Load(), tEnd-l.LastAny.Load()),
			Gap50: l.Gap50.Load(), Gap500: l.Gap500.Load(), Gap1s: l.Gap1s.Load(), OpenWakeNs: openWake,
			RSamples: rs[i], Samples: ns,
		}
		if openWake > 1_000_000_000 {
			r.Gap1s++ // the gap still open at the end counts
		}
		b := readThr(r.Tid)
		r.Ut, r.St, r.Vol, r.Invol = b.Ut-statA[i].Ut, b.St-statA[i].St, b.Vol-statA[i].Vol, b.Invol-statA[i].Invol
		res.Loops = append(res.Loops, r)
	}
	res.Secs = float64(tEnd-t0) / 1e9
	res.AllocMiB = float64(allocated.Load()) / (1 << 20)
	cur := readSTW()
	res.Cycles = cur.cycles - base.cycles
	res.STW = cur.since(base)
	// grace for the loops to see stop; whatever is stuck is reported as unfinished
	for dl := time.Now().Add(3 * time.Second); time.Now().Before(dl); time.Sleep(10 * time.Millisecond) {
		n := 0
		for _, l := range loops {
			if !l.Exited.Load() {
				n++
			}
		}
		if n == 0 {
			break
		}
	}
	for _, l := range loops {
		if !l.Exited.Load() {
			res.Unfinished++
		}
	}
	return res, nil
}

// e4Snapshot logs which threads of the process used CPU over 300 ms while loop i
// had been without a wake-up for 200+ ms. It allocates, so in a real stall it can
// itself be held up (the GC cannot finish a cycle): it stamps when the stall was
// seen to start (stalledWake = the loop's last wake-up) and when it really ran,
// and says whether the episode was still going on.
func e4Snapshot(c clk, i int, l *loopStats, stalledWake, t0 int64) string {
	a := map[int]thrStat{}
	comm := map[int]string{}
	for _, tid := range taskIDs() {
		a[tid] = readThr(tid)
		comm[tid] = threadComm(tid)
	}
	ranAt := c.now()
	openAtRun := ranAt - l.LastWake.Load()
	over := l.LastWake.Load() != stalledWake
	time.Sleep(300 * time.Millisecond)
	var rows []string
	for _, tid := range taskIDs() {
		b, ok := a[tid]
		if !ok || !b.OK {
			continue
		}
		n := readThr(tid)
		du, ds := n.Ut-b.Ut, n.St-b.St
		if du+ds < 3 && tid != int(l.Tid.Load()) {
			continue
		}
		role := ""
		if tid == int(l.Tid.Load()) {
			role = " <- the stalled loop"
		}
		rows = append(rows, fmt.Sprintf("    tid %d %s state %c cpu %d: +%d usr +%d sys ticks, +%d vol +%d invol switches%s", tid, comm[tid], n.State, n.LastCPU, du, ds, n.Vol-b.Vol, n.Invol-b.Invol, role))
	}
	late := ranAt - (stalledWake + 200_000_000)
	flag := ""
	if over {
		flag = " EPISODE ALREADY OVER when the snapshot started (the loop woke)"
	} else if late > 100_000_000 {
		flag = fmt.Sprintf(" LATE: ran %.0f ms after the stall passed 200 ms", float64(late)/1e6)
	}
	if l.LastWake.Load() != stalledWake && !over {
		flag += " (the loop woke during the 300 ms window)"
	}
	return fmt.Sprintf("loop %d (cpu %d): no wake-up since t=%.2f s; snapshot ran at t=%.2f s, loop open gap then %.0f ms%s; threads that used CPU in the next 300 ms (10 ms ticks):\n%s",
		i, l.CPU, float64(stalledWake-t0)/1e9, float64(ranAt-t0)/1e9, float64(openAtRun)/1e6, flag, strings.Join(rows, "\n"))
}

// ---- the heartbeat -------------------------------------------------------------

// e4Heartbeat writes one line a second to stderr from a goroutine that never
// allocates (atomics, strconv.AppendInt into a fixed buffer, one write(2)), so it
// can go on while the GC is stuck in a mark phase and every allocating goroutine
// is parked: the parent keeps the child's stderr, and when the child never finishes
// it prints the last lines as the gap table.
type e4Heartbeat struct {
	c     clk
	loops []*loopStats
	t0    int64
	buf   [4096]byte
}

// line: "HB t=12 | 2:3,17,17,0 | 3:..." = cpu:open wake gap, longest wake gap, longest any gap (ms), EINTRs
func (h *e4Heartbeat) format(now int64) []byte {
	b := h.buf[:0]
	b = append(b, "HB t="...)
	b = strconv.AppendInt(b, (now-h.t0)/1_000_000_000, 10)
	for _, l := range h.loops {
		b = append(b, " | "...)
		b = strconv.AppendInt(b, int64(l.CPU), 10)
		b = append(b, ':')
		b = strconv.AppendInt(b, (now-l.LastWake.Load())/1_000_000, 10)
		b = append(b, ',')
		b = strconv.AppendInt(b, l.MaxGapWake.Load()/1_000_000, 10)
		b = append(b, ',')
		b = strconv.AppendInt(b, l.MaxGapAny.Load()/1_000_000, 10)
		b = append(b, ',')
		b = strconv.AppendUint(b, l.Eintr.Load(), 10)
	}
	return append(b, '\n')
}

func (h *e4Heartbeat) run() {
	for {
		time.Sleep(time.Second)
		_, _ = unix.Write(2, h.format(h.c.now()))
	}
}

// parseHeartbeat reads the loops of the last HB line: cpu -> (open, longest wake gap, longest any gap) in ms.
func parseHeartbeat(stderr []byte) (lines []string, last map[int][3]int64) {
	for _, l := range strings.Split(string(stderr), "\n") {
		if strings.HasPrefix(l, "HB ") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return nil, nil
	}
	last = map[int][3]int64{}
	for _, f := range strings.Split(lines[len(lines)-1], " | ")[1:] {
		cpu, rest, ok := strings.Cut(f, ":")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(cpu)
		p := strings.Split(rest, ",")
		if len(p) < 3 {
			continue
		}
		var v [3]int64
		for i := range v {
			v[i], _ = strconv.ParseInt(p[i], 10, 64)
		}
		last[n] = v
	}
	return lines, last
}

// e4CPUs returns the loop CPUs of a case and whether the process mask is confined to them.
func (t topo) e4CPUs(name string) (cpus []int, confined bool, note string, err error) {
	asc := sortedCopy(t.Allowed)
	switch name {
	case "all":
		return asc, false, "every allowed CPU", nil
	case "slow", "slowconf":
		if t.Hetero {
			cpus, note = sortedCopy(t.Classes[len(t.Classes)-1]), "the slowest core type"
		} else {
			cpus, note = takeN(asc, 4), "homogeneous host: the 4 lowest ids"
		}
		return cpus, name == "slowconf", note, nil
	case "fast":
		if t.Hetero {
			return sortedCopy(t.Classes[0]), false, "the fastest core type", nil
		}
		return sortedCopy(takeN(reverse(asc), 8)), false, "homogeneous host: the 8 highest ids", nil
	}
	return nil, false, "", fmt.Errorf("unknown E4 case %q (slow, fast, all, slowconf)", name)
}

func TestProbeE4MinRepro(t *testing.T) {
	tp := loadTopo()
	logf(t, "E4 minimal reproducer (no celeris)\nhost: %s\ntopology:\n%s", hostFacts(), tp.describe())
	cases := envList("CELERIS_PROBE_E4_CASES", "slow,fast,all")
	modes := envList("CELERIS_PROBE_E4_MODES", "default,noasync")
	rounds := max(envInt("CELERIS_PROBE_E4_ROUNDS", 1), 1)
	secs := envInt("CELERIS_PROBE_E4_SECS", 30)
	failMs := int64(envInt("CELERIS_PROBE_E4_FAIL_MS", 1000))
	gogc := envInt("CELERIS_PROBE_E4_GOGC", 100)
	grace := envInt("CELERIS_PROBE_E4_GRACE_S", 120)
	fastCPU := tp.byFastness(tp.Classes[0])[0]
	for _, m := range modes {
		if m != "default" && m != "noasync" {
			t.Fatalf("CELERIS_PROBE_E4_MODES: %q is not default or noasync", m)
		}
	}
	type row struct {
		name       string
		maxWakeMs  float64
		maxAnyMs   float64
		cycles     uint64
		unfinished int
		verdict    string
	}
	var rows []row
	for _, cs := range cases {
		cpus, confined, note, err := tp.e4CPUs(cs)
		if err != nil {
			t.Fatal(err)
		}
		var cl []string
		for _, c := range cpus {
			cl = append(cl, fmt.Sprintf("%d(%s)", c, classLabel(tp, c)))
		}
		logf(t, "case %s: %d loops, %s: %s; process mask %s", cs, len(cpus), note, strings.Join(cl, " "), map[bool]string{true: "CONFINED to those CPUs", false: "every allowed CPU"}[confined])
		for _, mode := range modes {
			for r := 1; r <= rounds; r++ {
				name := cs + "-" + mode
				if rounds > 1 {
					name += "-r" + strconv.Itoa(r)
				}
				t.Run(name, func(t *testing.T) {
					gd := "GODEBUG=asyncpreemptoff=0"
					if mode == "noasync" {
						gd = "GODEBUG=asyncpreemptoff=1"
					}
					if os.Getenv("CELERIS_PROBE_E4_GCTRACE") == "1" {
						gd += ",gctrace=1"
					}
					spec := childSpec{kind: "e4", timeout: time.Duration(secs+grace) * time.Second, env: []string{
						gd, "GOGC=" + strconv.Itoa(gogc), "CELERIS_PROBE_ARG_E4_CPUS=" + fmtCPUs(cpus), "CELERIS_PROBE_ARG_E4_FASTCPU=" + strconv.Itoa(fastCPU),
					}}
					if confined {
						spec.mask = cpus
					}
					so, se, err := runChild(spec)
					rw := row{name: name}
					if err != nil {
						rw.verdict = "CHILD DID NOT FINISH"
						hbLines, hbLast := parseHeartbeat(se)
						var tb strings.Builder
						fmt.Fprintf(&tb, "%s: the child did not finish within %d s (%v): a stall in progress, a crash, or a wedged child.\n", name, secs+grace, err)
						if len(hbLines) > 0 {
							fmt.Fprintf(&tb, "  heartbeat of the child (one line a second from a goroutine that never allocates), last %d of %d lines; per loop cpu:open_wake_gap,longest_wake_gap,longest_any_gap (ms),eintr:\n", min(len(hbLines), 12), len(hbLines))
							for _, l := range hbLines[max(len(hbLines)-12, 0):] {
								fmt.Fprintf(&tb, "    %s\n", l)
							}
							var bad []string
							for _, c := range cpus {
								if v, ok := hbLast[c]; ok {
									rw.maxWakeMs = max(rw.maxWakeMs, float64(max(v[0], v[1])))
									rw.maxAnyMs = max(rw.maxAnyMs, float64(v[2]))
									if max(v[0], v[1]) > failMs {
										bad = append(bad, fmt.Sprintf("cpu %d (%s) %d ms", c, classLabel(tp, c), max(v[0], v[1])))
									}
								}
							}
							fmt.Fprintf(&tb, "  loops above %d ms at the last heartbeat: %s\n", failMs, orDash(strings.Join(bad, ", ")))
						} else {
							tb.WriteString("  no heartbeat line was received\n")
						}
						fmt.Fprintf(&tb, "  stderr tail (heartbeat lines left out):\n%s", tailLines(withoutHB(se), 25))
						rows = append(rows, rw)
						if mode == "default" {
							t.Errorf("%s", tb.String())
						} else {
							logf(t, "%s", tb.String())
						}
						return
					}
					var res e4Result
					if err := decodeChild(so, &res); err != nil {
						t.Fatalf("%s: %v\n%s", name, err, tailLines(se, 20))
					}
					rw.cycles, rw.unfinished = res.Cycles, res.Unfinished
					bad := reportE4(t, tp, name, mode, res, failMs)
					for _, l := range res.Loops {
						rw.maxWakeMs = max(rw.maxWakeMs, float64(l.GapWakeNs)/1e6)
						rw.maxAnyMs = max(rw.maxAnyMs, float64(l.GapAnyNs)/1e6)
					}
					rw.verdict = "ok"
					if len(bad) > 0 {
						rw.verdict = fmt.Sprintf("GAP>%dms on %d loop(s)", failMs, len(bad))
					}
					rows = append(rows, rw)
					if os.Getenv("CELERIS_PROBE_E4_GCTRACE") == "1" {
						logf(t, "child stderr (tail):\n%s", tailLines(se, 30))
					}
					if len(bad) > 0 {
						msg := fmt.Sprintf("%s: loops with a wake gap above %d ms: %s", name, failMs, strings.Join(bad, ", "))
						if mode == "default" {
							t.Errorf("%s", msg)
						} else {
							logf(t, "CONTROL VIOLATED (asyncpreemptoff=1) %s", msg)
						}
					}
				})
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "E4 SUMMARY %s (secs %d, GOGC %d)\n  case-mode          max_wake_gap_ms  max_any_gap_ms  gc_cycles  unfinished  verdict", runtime.GOARCH, secs, gogc)
	for _, r := range rows {
		fmt.Fprintf(&b, "\n  %-18s %-16.1f %-15.1f %-10d %-11d %s", r.name, r.maxWakeMs, r.maxAnyMs, r.cycles, r.unfinished, r.verdict)
	}
	logf(t, "%s", b.String())
}

// reportE4 logs one child's result and returns the loops above the failure gap.
func reportE4(t *testing.T, tp topo, name, mode string, res e4Result, failMs int64) (bad []string) {
	var b strings.Builder
	fmt.Fprintf(&b, "E4 %s: %.1f s, GOMAXPROCS %d, GOGC %s, GODEBUG %q; %s\n", name, res.Secs, res.GoMaxProcs, res.GOGC, res.GoDebug, res.Facts)
	fmt.Fprintf(&b, "  %s\n", res.Work)
	fmt.Fprintf(&b, "  GC: %d cycles (%.1f/s), garbage %.0f MiB (%.0f MiB/s); %s\n", res.Cycles, float64(res.Cycles)/res.Secs, res.AllocMiB, res.AllocMiB/res.Secs, res.STW)
	fmt.Fprintf(&b, "  loop cpu  class    tid      wakes    eintr   wake_gap_ms any_gap_ms  open_ms  gaps>50ms >500ms >1s  R%%   usr/sys ticks  vol invol")
	for i, l := range res.Loops {
		cls := classLabel(tp, l.CPU)
		note := ""
		if l.PinErr != "" {
			note = " PINERR " + l.PinErr
		}
		fmt.Fprintf(&b, "\n  %-4d %-4d %-8s %-8d %-8d %-7d %-11.1f %-10.1f  %-8.1f %-9d %-5d %-4d %-4.0f %d/%d  %d %d%s",
			i, l.CPU, cls, l.Tid, l.Wakes, l.Eintr, float64(l.GapWakeNs)/1e6, float64(l.GapAnyNs)/1e6, float64(l.OpenWakeNs)/1e6,
			l.Gap50, l.Gap500, l.Gap1s, 100*float64(l.RSamples)/float64(max(l.Samples, 1)), l.Ut, l.St, l.Vol, l.Invol, note)
		if l.GapWakeNs > failMs*1_000_000 {
			bad = append(bad, fmt.Sprintf("loop %d cpu %d (%s) %.1f ms", i, l.CPU, cls, float64(l.GapWakeNs)/1e6))
		}
	}
	if res.Unfinished > 0 {
		fmt.Fprintf(&b, "\n  %d loop thread(s) had not seen stop 3 s after the run ended", res.Unfinished)
	}
	for _, s := range res.Snapshots {
		fmt.Fprintf(&b, "\n  %s", s)
	}
	logf(t, "%s", b.String())
	return bad
}

// TestProbeE4HeartbeatAllocs holds the heartbeat formatter to zero allocations:
// it must keep running while the GC is stuck. It is not in any dispatch regexp.
func TestProbeE4HeartbeatAllocs(t *testing.T) {
	c := newClk()
	h := &e4Heartbeat{c: c, t0: c.now()}
	for _, cpu := range []int{2, 3, 11} {
		l := &loopStats{CPU: cpu}
		l.LastWake.Store(c.now() - 123_000_000)
		l.MaxGapWake.Store(7_000_000)
		l.MaxGapAny.Store(9_000_000)
		l.Eintr.Store(12345)
		h.loops = append(h.loops, l)
	}
	var out []byte
	if n := testing.AllocsPerRun(1000, func() { out = h.format(c.now()) }); n != 0 {
		t.Fatalf("the heartbeat formatter allocates %.0f times per line", n)
	}
	lines, last := parseHeartbeat(out)
	if len(lines) != 1 || last[2][0] < 120 || last[2][1] != 7 || last[2][2] != 9 || len(last) != 3 {
		t.Fatalf("heartbeat line %q parsed as %v / %v", out, lines, last)
	}
}

func withoutHB(b []byte) []byte {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(l, "HB ") {
			out = append(out, l)
		}
	}
	return []byte(strings.Join(out, "\n"))
}
