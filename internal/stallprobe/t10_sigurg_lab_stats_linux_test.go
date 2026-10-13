//go:build linux

package stallprobe

// T10 (SIGURG lab), the pure part: parsing of the C program's output, statistics, the cell and arm plans, the
// fake class map and the /proc readers. Nothing here starts a process, so TestSigurgLabUnit can check it with
// fixed inputs. The experiments themselves are in t10_sigurg_lab_linux_test.go.

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ---- per-request records -----------------------------------------------------------------------------

// labReq is one request (one suspendG call) of the C program: the line
// "lat_ns sends handlers reentry zero_delta maxconsec timeout".
type labReq struct {
	LatNs                                          int64
	Sends, Handlers, Reentry, ZeroDelta, MaxConsec int64
	Timeout                                        bool
}

func labParseRecords(text string) ([]labReq, error) {
	var out []labReq
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		l := strings.TrimSpace(sc.Text())
		if l == "" {
			continue
		}
		f := strings.Fields(l)
		if len(f) != 7 {
			return nil, fmt.Errorf("record line %d: want 7 fields, got %d: %q", n, len(f), l)
		}
		var v [7]int64
		for i, s := range f {
			x, err := strconv.ParseInt(s, 10, 64)
			if err != nil || x < 0 {
				return nil, fmt.Errorf("record line %d field %d: %q", n, i+1, s)
			}
			v[i] = x
		}
		out = append(out, labReq{v[0], v[1], v[2], v[3], v[4], v[5], v[6] != 0})
	}
	return out, sc.Err()
}

// labStats summarises requests (one rep, or several reps pooled by concatenation: the percentiles are exact).
type labStats struct {
	Requests, Timeouts      int
	P50, P99, P999, Max     int64 // ns
	Over1ms, Over50ms       float64
	SendsMean               float64
	SendsMax                int64
	MultiSend               int // requests that needed more than one send
	Handlers, Reentry, Zero int64
	MaxConsec               int64 // longest run of consecutive re-entry handlers (max over requests)
	ReqWithReentry          int
	ReqOverConsec10         int // requests with a run of at least 10 consecutive re-entries
}

func labSummarize(reqs []labReq) labStats {
	var s labStats
	s.Requests = len(reqs)
	if len(reqs) == 0 {
		return s
	}
	lat := make([]int64, len(reqs))
	var sends int64
	var o1, o50 int
	for i, r := range reqs {
		lat[i] = r.LatNs
		sends += r.Sends
		if r.Timeout {
			s.Timeouts++
		}
		if r.LatNs > 1_000_000 {
			o1++
		}
		if r.LatNs > 50_000_000 {
			o50++
		}
		if r.Sends > s.SendsMax {
			s.SendsMax = r.Sends
		}
		if r.Sends > 1 {
			s.MultiSend++
		}
		s.Handlers += r.Handlers
		s.Reentry += r.Reentry
		s.Zero += r.ZeroDelta
		if r.MaxConsec > s.MaxConsec {
			s.MaxConsec = r.MaxConsec
		}
		if r.Reentry > 0 {
			s.ReqWithReentry++
		}
		if r.MaxConsec >= 10 {
			s.ReqOverConsec10++
		}
	}
	p := percentiles(lat)
	s.P50, s.P99, s.P999, s.Max = p.P50, p.P99, p.P999, p.Max
	s.Over1ms, s.Over50ms = float64(o1)/float64(len(reqs)), float64(o50)/float64(len(reqs))
	s.SendsMean = float64(sends) / float64(len(reqs))
	return s
}

// ---- histograms (the C program's idx:count lists) ------------------------------------------------------------

type labHist map[int]uint64

// labBucket is the C program's hidx: 0 for under 1 us, else 1+4*o+q with o = floor(log2(ns/1000)) and q the
// quarter octave.
func labBucket(ns int64) int {
	if ns < 1000 {
		return 0
	}
	o := 0
	for (int64(1000) << (o + 1)) <= ns {
		o++
	}
	r := float64(ns) / (1000.0 * float64(int64(1)<<o))
	q := 0
	switch {
	case r >= 1.681792830507429:
		q = 3
	case r >= 1.414213562373095:
		q = 2
	case r >= 1.189207115002721:
		q = 1
	}
	return min(1+4*o+q, 127)
}

func labParseHist(s string) (labHist, error) {
	h := labHist{}
	if s == "" || s == "-" {
		return h, nil
	}
	for _, p := range strings.Split(s, ",") {
		a, b, ok := strings.Cut(p, ":")
		if !ok {
			return nil, fmt.Errorf("histogram item %q", p)
		}
		i, err1 := strconv.Atoi(a)
		c, err2 := strconv.ParseUint(b, 10, 64)
		if err1 != nil || err2 != nil || i < 0 || i > 127 {
			return nil, fmt.Errorf("histogram item %q", p)
		}
		h[i] += c
	}
	return h, nil
}

func (h labHist) merge(o labHist) {
	for i, c := range o {
		h[i] += c
	}
}

func (h labHist) total() (n uint64) {
	for _, c := range h {
		n += c
	}
	return n
}

// quantile returns the geometric middle of the bucket holding the q-quantile, in ns (-1: empty).
func (h labHist) quantile(q float64) float64 {
	n := h.total()
	if n == 0 {
		return -1
	}
	idx := make([]int, 0, len(h))
	for i := range h {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	want := uint64(math.Ceil(q * float64(n)))
	if want < 1 {
		want = 1
	}
	var acc uint64
	for _, i := range idx {
		acc += h[i]
		if acc >= want {
			if i == 0 {
				return 500
			}
			lo := 1000 * math.Pow(2, float64(i-1)/4)
			hi := 1000 * math.Pow(2, float64(i)/4)
			return math.Sqrt(lo * hi)
		}
	}
	return -1
}

func labUs(ns float64) string {
	if ns < 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f", ns/1e3)
}

// ---- the LABCELL line -----------------------------------------------------------------------------------------

// labCell is a parsed LABCELL line: counters from the C program plus its histograms.
type labCell struct {
	KV                 map[string]string
	Ack, Gap, A2R      labHist
	Resends, Waited    uint64
	Handlers, Reentry  uint64
	ZeroDelta          uint64
	Completes, Timeout uint64
	Requests, Sends    uint64
}

func labParseCell(out string) (labCell, error) {
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "LABCELL ") {
			continue
		}
		c := labCell{KV: parseKV(l)}
		var err error
		for name, dst := range map[string]*labHist{"ack_hist": &c.Ack, "gap_hist": &c.Gap, "a2r_hist": &c.A2R} {
			if *dst, err = labParseHist(c.KV[name]); err != nil {
				return c, fmt.Errorf("%s: %w", name, err)
			}
		}
		for name, dst := range map[string]*uint64{"resends": &c.Resends, "resends_waited": &c.Waited, "handlers": &c.Handlers, "reentry": &c.Reentry,
			"zero_delta": &c.ZeroDelta, "completes": &c.Completes, "timeouts": &c.Timeout, "requests": &c.Requests, "sends": &c.Sends} {
			v, err := strconv.ParseUint(c.KV[name], 10, 64)
			if err != nil {
				return c, fmt.Errorf("LABCELL field %s=%q", name, c.KV[name])
			}
			*dst = v
		}
		return c, nil
	}
	return labCell{}, fmt.Errorf("no LABCELL line in %q", tail(out, 200))
}

// labPool is every rep of one (pair, D) cell.
type labPool struct {
	reps               int
	reqs               []labReq
	ack, gap, a2r      labHist
	resends, waited    uint64
	handlers, reentry  uint64
	timeouts, requests uint64
}

func newLabPool() *labPool { return &labPool{ack: labHist{}, gap: labHist{}, a2r: labHist{}} }

func (p *labPool) add(c labCell, reqs []labReq) {
	p.reps++
	p.reqs = append(p.reqs, reqs...)
	p.ack.merge(c.Ack)
	p.gap.merge(c.Gap)
	p.a2r.merge(c.A2R)
	p.resends += c.Resends
	p.waited += c.Waited
	p.handlers += c.Handlers
	p.reentry += c.Reentry
	p.timeouts += c.Timeout
	p.requests += c.Requests
}

// labStatsLine renders statistics as space separated k=v, the same fields for a rep (LAB_CELL) and a pool (LAB_TABLE).
func labStatsLine(st labStats, ack, gap, a2r labHist, resends, waited uint64) string {
	wf := "-"
	if resends > 0 {
		wf = fmt.Sprintf("%.3f", float64(waited)/float64(resends))
	}
	return fmt.Sprintf("requests=%d timeouts=%d p50_us=%s p99_us=%s p999_us=%s max_us=%s gt1ms=%.4f gt50ms=%.4f sends_mean=%.2f sends_max=%d multi_send=%d handlers=%d reentry=%d zero_delta=%d max_consec_reentry=%d req_with_reentry=%d req_consec10=%d resends=%d resends_waited_frac=%s ack_p50_us=%s ack_p99_us=%s gap_p50_us=%s gap_p99_us=%s ack_to_resend_p50_us=%s",
		st.Requests, st.Timeouts, labUs(float64(st.P50)), labUs(float64(st.P99)), labUs(float64(st.P999)), labUs(float64(st.Max)), st.Over1ms, st.Over50ms,
		st.SendsMean, st.SendsMax, st.MultiSend, st.Handlers, st.Reentry, st.Zero, st.MaxConsec, st.ReqWithReentry, st.ReqOverConsec10, resends, wf,
		labUs(ack.quantile(0.5)), labUs(ack.quantile(0.99)), labUs(gap.quantile(0.5)), labUs(gap.quantile(0.99)), labUs(a2r.quantile(0.5)))
}

// ---- CPU classes, pairs, plans --------------------------------------------------------------------------------

// labTopo is the lab's view of the host: which CPUs are the slow and fast core types.
type labTopo struct {
	Hetero, Fake  bool
	Littles, Bigs []int // ascending ids; nil on a homogeneous host
	All           []int // the allowed CPUs, ascending
	Fast          int   // the calibration core: the fastest of Bigs (the highest capacity), or All[0]
	Note          string
}

// labClasses derives the lab's classes from the topology, or from a fake map "BIGS/LITTLES" ("4-7/0-3") that
// makes a homogeneous host (the Docker VM) walk the heterogeneous code path. A fake map changes nothing about
// the hardware: every line the lab logs then says fake=true.
func labClasses(tp topo, fake string) (labTopo, error) {
	lt := labTopo{All: sortedCopy(tp.Allowed)}
	if len(lt.All) > 0 {
		lt.Fast = lt.All[0]
	}
	if fake != "" {
		b, l, ok := strings.Cut(fake, "/")
		if !ok {
			return lt, fmt.Errorf("fake class map %q: want BIGS/LITTLES, e.g. 4-7/0-3", fake)
		}
		bigs, err1 := parseCPUList(b)
		lits, err2 := parseCPUList(l)
		if err1 != nil || err2 != nil || len(bigs) == 0 || len(lits) == 0 {
			return lt, fmt.Errorf("fake class map %q: %v %v", fake, err1, err2)
		}
		allowed := map[int]bool{}
		for _, c := range lt.All {
			allowed[c] = true
		}
		seen := map[int]bool{}
		for _, c := range append(append([]int(nil), bigs...), lits...) {
			if !allowed[c] || seen[c] {
				return lt, fmt.Errorf("fake class map %q: cpu %d is not allowed or listed twice", fake, c)
			}
			seen[c] = true
		}
		lt.Hetero, lt.Fake = true, true
		lt.Bigs, lt.Littles = sortedCopy(bigs), sortedCopy(lits)
		lt.Fast = lt.Bigs[0]
		lt.Note = "FAKE class map " + fake + " on a host without core types"
		return lt, nil
	}
	if !tp.Hetero || len(tp.Classes) < 2 {
		lt.Note = "homogeneous host: same-class cells only"
		return lt, nil
	}
	lt.Hetero = true
	lt.Bigs = sortedCopy(tp.Classes[0])
	lt.Littles = sortedCopy(tp.Classes[len(tp.Classes)-1])
	lt.Fast = tp.byFastness(lt.Bigs)[0]
	lt.Note = fmt.Sprintf("big = class 0 (cpus %s), little = class %d (cpus %s)", fmtCPUs(lt.Bigs), len(tp.Classes)-1, fmtCPUs(lt.Littles))
	if len(tp.Classes) > 2 {
		lt.Note += fmt.Sprintf("; %d middle classes unused", len(tp.Classes)-2)
	}
	return lt, nil
}

type labPair struct {
	Name        string
	T, S        int
	TCls, SCls  string
	ControlOnly bool // homogeneous host: the same-class control
	SameClass   bool
}

// labPairs lists the (target, sender) CPU pairs. Heterogeneous: the sender class "same as the target's other
// core" is the same cell as "sender of the target's own class", so there are 2 targets x 2 sender classes.
// sibs says which CPUs share a physical core (SMT) with a CPU, domain which share a frequency domain.
func labPairs(lt labTopo, domain func(int) string, sibs func(int) []int) []labPair {
	other := func(t int, from []int, wantDomain string) int { // another core of the list, same domain preferred
		best := -1
		for _, c := range from {
			if c == t {
				continue
			}
			skip := false
			for _, s := range sibs(t) {
				if s == c {
					skip = true
				}
			}
			if skip {
				continue
			}
			if best < 0 {
				best = c
			}
			if wantDomain != "" && domain(c) == wantDomain {
				return c
			}
		}
		return best
	}
	if !lt.Hetero {
		if len(lt.All) < 2 {
			return nil
		}
		t := lt.All[0]
		s := other(t, lt.All, "")
		if s < 0 {
			return nil
		}
		return []labPair{{Name: "same<-same", T: t, S: s, TCls: "all", SCls: "all", ControlOnly: true}}
	}
	var ps []labPair
	lt0, bg0 := lt.Littles[0], lt.Bigs[0]
	ps = append(ps, labPair{Name: "little<-big", T: lt0, S: bg0, TCls: "little", SCls: "big"})
	if s := other(lt0, lt.Littles, domain(lt0)); s >= 0 {
		ps = append(ps, labPair{Name: "little<-little", T: lt0, S: s, TCls: "little", SCls: "little", SameClass: true})
	}
	if s := other(bg0, lt.Bigs, domain(bg0)); s >= 0 {
		ps = append(ps, labPair{Name: "big<-big", T: bg0, S: s, TCls: "big", SCls: "big", SameClass: true})
	}
	ps = append(ps, labPair{Name: "big<-little", T: bg0, S: lt0, TCls: "big", SCls: "little"})
	return ps
}

type labCellSpec struct {
	Rep  int
	Pair labPair
	D    int // us
}

// labPlan: reps outermost (so a budget stop leaves complete reps), then pairs, then D rotated by 4 per rep.
func labPlan(pairs []labPair, ds []int, reps int) []labCellSpec {
	var out []labCellSpec
	for r := 0; r < reps; r++ {
		for _, p := range pairs {
			for i := range ds {
				out = append(out, labCellSpec{Rep: r + 1, Pair: p, D: ds[(i+4*r)%len(ds)]})
			}
		}
	}
	return out
}

func labParseDs(list []string) ([]int, error) {
	var ds []int
	for _, s := range list {
		d, err := strconv.Atoi(s)
		if err != nil || d < 0 || d > 1_000_000 {
			return nil, fmt.Errorf("D %q: want 0..1000000 (us)", s)
		}
		ds = append(ds, d)
	}
	if len(ds) == 0 {
		return nil, fmt.Errorf("no D values")
	}
	return ds, nil
}

// labCellSeconds is the wall time one cell takes: its run time, a process start and the stop of the sender.
func labCellSeconds(secs int) float64 { return float64(secs) + 0.3 }

// ---- experiment B: the Go-level arms --------------------------------------------------------------------------

// labArm is one process-affinity arm of the Go reproducer.
type labArm struct {
	Name    string
	Pin     []int // the whole process starts with this mask (nil: the host's)
	Cpus    []int // the loops' CPUs (nil: -class=little)
	NoAsync bool
	Lab     bool // needs the calibration hook (stallrepro_lab binary)
	Why     string
}

func labArms(lt labTopo, tp topo) []labArm {
	four := func(c []int) []int { return takeN(c, 4) }
	fast4 := four(lt.Bigs)
	if !lt.Fake {
		fast4 = sortedCopy(takeN(tp.byFastness(lt.Bigs), 4))
	}
	return []labArm{
		{Name: "little-default", Why: "control, as TestUpstreamMatrix: loops on the little CPUs, the process may use every CPU"},
		{Name: "little-all-little", Pin: lt.Littles, Cpus: four(lt.Littles), Lab: true, Why: "slow sender: every thread restricted to the little CPUs before the runtime starts"},
		{Name: "little-loops-rest-big", Pin: lt.Bigs, Cpus: four(lt.Littles), Lab: true, Why: "fast sender: loops pinned to little CPUs, every other thread restricted to the big CPUs"},
		{Name: "big-loops-rest-little", Pin: lt.Littles, Cpus: fast4, Lab: true, Why: "reverse control: 4 loops on big CPUs, every other thread restricted to the little CPUs"},
		{Name: "little-all-little-noasync", Pin: lt.Littles, Cpus: four(lt.Littles), Lab: true, NoAsync: true, Why: "starvation control for little-all-little: asyncpreemptoff=1"},
	}
}

type labArmRow struct {
	Arm     string
	Rep     int
	GapMs   float64
	Verdict string // OK | STALL | HANG | ERROR
}

type labArmAgg struct {
	Arm                         string
	Runs, Stalled               int
	MedianMs, MaxMs, MedStallMs float64
}

func labArmSummary(rows []labArmRow) []labArmAgg {
	var order []string
	by := map[string][]labArmRow{}
	for _, r := range rows {
		if _, ok := by[r.Arm]; !ok {
			order = append(order, r.Arm)
		}
		by[r.Arm] = append(by[r.Arm], r)
	}
	med := func(v []float64) float64 {
		if len(v) == 0 {
			return -1
		}
		s := append([]float64(nil), v...)
		sort.Float64s(s)
		if len(s)%2 == 1 {
			return s[len(s)/2]
		}
		return (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	var out []labArmAgg
	for _, a := range order {
		g := labArmAgg{Arm: a}
		var all, st []float64
		for _, r := range by[a] {
			g.Runs++
			if r.Verdict == "STALL" || r.Verdict == "HANG" {
				g.Stalled++
				st = append(st, r.GapMs)
			}
			if r.Verdict == "OK" || r.Verdict == "STALL" {
				all = append(all, r.GapMs)
				g.MaxMs = max(g.MaxMs, r.GapMs)
			}
		}
		g.MedianMs, g.MedStallMs = med(all), med(st)
		out = append(out, g)
	}
	return out
}

// ---- /proc readers (mid-run proof that an arm did what it claims) -----------------------------------------------

// labStatLastCPU returns field 39 ("processor") of a /proc/PID/task/TID/stat line, -1 when it cannot be read.
// comm (field 2) may hold spaces and parentheses, so fields are counted after the last ')'.
func labStatLastCPU(stat string) int {
	i := strings.LastIndex(stat, ")")
	if i < 0 {
		return -1
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 37 { // f[0] is field 3
		return -1
	}
	n, err := strconv.Atoi(f[36])
	if err != nil {
		return -1
	}
	return n
}

func labStatusMask(status string) string {
	for _, l := range strings.Split(status, "\n") {
		if v, ok := strings.CutPrefix(l, "Cpus_allowed_list:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return "?"
}

// labProcSummary describes the threads of a process: how many have each Cpus_allowed_list, and where each last
// ran. littles/bigs classify the last CPU.
func labProcSummary(pid int, littles, bigs []int) string {
	ents, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return "proc=unreadable"
	}
	masks := map[string]int{}
	var inL, inB, other, n int
	isIn := func(c int, s []int) bool {
		for _, x := range s {
			if x == c {
				return true
			}
		}
		return false
	}
	for _, e := range ents {
		base := fmt.Sprintf("/proc/%d/task/%s/", pid, e.Name())
		st, err1 := os.ReadFile(base + "status")
		stat, err2 := os.ReadFile(base + "stat")
		if err1 != nil || err2 != nil {
			continue
		}
		n++
		masks[labStatusMask(string(st))]++
		switch c := labStatLastCPU(string(stat)); {
		case isIn(c, littles):
			inL++
		case isIn(c, bigs):
			inB++
		default:
			other++
		}
	}
	keys := make([]string, 0, len(masks))
	for k := range masks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", k, masks[k]))
	}
	return fmt.Sprintf("threads=%d masks=%q last_cpu_little=%d last_cpu_big=%d last_cpu_other=%d", n, strings.Join(parts, ";"), inL, inB, other)
}

// labPatchRepro makes the lab copy of the reproducer: the calibration core becomes settable
// (STALLREPRO_CALIB_CPU) so that an arm whose mask has no big CPU still calibrates its 20 us of work on the same
// core as the control arm. It fails loudly when the anchor is not found exactly once.
func labPatchRepro(src []byte) (patched []byte, hook []byte, err error) {
	const anchor = "affinity(fast)"
	if n := strings.Count(string(src), anchor); n != 1 {
		return nil, nil, fmt.Errorf("calibration anchor %q found %d times, want 1", anchor, n)
	}
	patched = []byte(strings.Replace(string(src), anchor, "affinity(labCalib(fast))", 1))
	hook = []byte(`package main

import (
	"os"
	"strconv"
)

// labCalib lets the lab choose the CPU the 20 us of work per wake-up is calibrated on.
func labCalib(def int) int {
	if v, err := strconv.Atoi(os.Getenv("STALLREPRO_CALIB_CPU")); err == nil && v >= 0 {
		return v
	}
	return def
}
`)
	return patched, hook, nil
}
