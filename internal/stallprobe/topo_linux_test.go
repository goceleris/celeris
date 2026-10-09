//go:build linux

package stallprobe

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// cpuInfo is what the probes know about one CPU. Every field is best effort:
// a missing file leaves the zero or -1 value, and nothing fails on it.
type cpuInfo struct {
	ID       int
	Part     string // arm64 "CPU part" ("0xd81"), "" elsewhere
	PartName string // "A720", "" when unknown
	Cap      int    // cpu_capacity, -1 when absent
	MaxKHz   int    // cpuinfo_max_freq, -1 when absent
	Domain   string // cpufreq related_cpus, "" when absent
}

func (c cpuInfo) class() string {
	switch {
	case c.PartName != "":
		return c.PartName
	case c.Part != "":
		return c.Part
	}
	return "-"
}

type topo struct {
	Allowed []int // sched_getaffinity of the calling thread
	Online  []int
	CPU     map[int]cpuInfo
	Classes [][]int // groups of allowed CPUs by core type, fastest first
	Hetero  bool    // two or more core types among the allowed CPUs
}

var armParts = map[string]string{
	"0xd03": "A53", "0xd04": "A35", "0xd05": "A55", "0xd07": "A57", "0xd08": "A72",
	"0xd09": "A73", "0xd0a": "A75", "0xd0b": "A76", "0xd0d": "A77", "0xd41": "A78",
	"0xd44": "X1", "0xd46": "A510", "0xd47": "A710", "0xd48": "X2", "0xd4d": "A715",
	"0xd4e": "X3", "0xd80": "A520", "0xd81": "A720", "0xd82": "X4", "0xd85": "X925",
	"0xd87": "A725",
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readInt(path string, def int) int {
	if n, err := strconv.Atoi(readTrim(path)); err == nil {
		return n
	}
	return def
}

// parseCPUList parses "0-3,8,10-11".
func parseCPUList(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(strings.TrimSpace(s), ",") {
		if p == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(p, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("cpu list %q: %v", s, err)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				return nil, fmt.Errorf("cpu list %q: %v", s, err)
			}
		}
		for i := a; i <= b; i++ {
			out = append(out, i)
		}
	}
	return out, nil
}

func cpuList(set *unix.CPUSet) []int {
	var out []int
	for i := 0; i < 1024; i++ {
		if set.IsSet(i) {
			out = append(out, i)
		}
	}
	return out
}

func maskOf(cpus []int) unix.CPUSet {
	var s unix.CPUSet
	s.Zero()
	for _, c := range cpus {
		s.Set(c)
	}
	return s
}

func fmtCPUs(cpus []int) string {
	if len(cpus) == 0 {
		return "-"
	}
	var parts []string
	for i := 0; i < len(cpus); {
		j := i
		for j+1 < len(cpus) && cpus[j+1] == cpus[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d-%d", cpus[i], cpus[j]))
		} else {
			parts = append(parts, strconv.Itoa(cpus[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// cpuinfoParts reads "CPU part" per processor from /proc/cpuinfo, which any
// user may read (the MIDR register file under sysfs is root only).
func cpuinfoParts() map[int]string {
	out := map[int]string{}
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return out
	}
	cur := -1
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "processor":
			if n, err := strconv.Atoi(v); err == nil {
				cur = n
			}
		case "CPU part":
			if cur >= 0 {
				if n, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(v), "0x"), 16, 64); err == nil && n != 0 {
					v = strings.ToLower(v)
					out[cur] = v
				}
			}
		}
	}
	return out
}

func loadTopo() topo {
	var t topo
	t.CPU = map[int]cpuInfo{}
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err == nil {
		t.Allowed = cpuList(&set)
	}
	if on, err := parseCPUList(readTrim("/sys/devices/system/cpu/online")); err == nil && len(on) > 0 {
		t.Online = on
	} else {
		t.Online = t.Allowed
	}
	if len(t.Allowed) == 0 {
		t.Allowed = t.Online
	}
	parts := cpuinfoParts()
	for _, id := range t.Allowed {
		d := fmt.Sprintf("/sys/devices/system/cpu/cpu%d", id)
		ci := cpuInfo{ID: id, Part: parts[id], Cap: readInt(d+"/cpu_capacity", -1), MaxKHz: readInt(d+"/cpufreq/cpuinfo_max_freq", -1)}
		if ci.Part == "" {
			// midr_el1 is root only; try it anyway.
			if v := readTrim(d + "/regs/identification/midr_el1"); v != "" {
				if n, err := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 64); err == nil && (n>>4)&0xfff != 0 {
					ci.Part = fmt.Sprintf("0x%x", (n>>4)&0xfff)
				}
			}
		}
		ci.PartName = armParts[ci.Part]
		ci.Domain = strings.ReplaceAll(readTrim(d+"/cpufreq/related_cpus"), " ", ",")
		t.CPU[id] = ci
	}
	// Core types: by CPU part when every allowed CPU reports one (the SoC's
	// frequency domains and cpu_capacity values differ inside one core type:
	// msr1's A720s are four domains), else one type.
	byPart := map[string][]int{}
	allParts := true
	for _, id := range t.Allowed {
		if t.CPU[id].Part == "" {
			allParts = false
		}
		byPart[t.CPU[id].Part] = append(byPart[t.CPU[id].Part], id)
	}
	if allParts && len(byPart) > 1 {
		for _, g := range byPart {
			t.Classes = append(t.Classes, g)
		}
		rank := func(g []int) (int, int) {
			mk, cp := 0, 0
			for _, id := range g {
				mk = max(mk, t.CPU[id].MaxKHz)
				cp = max(cp, t.CPU[id].Cap)
			}
			return mk, cp
		}
		sort.Slice(t.Classes, func(i, j int) bool {
			ai, bi := rank(t.Classes[i])
			aj, bj := rank(t.Classes[j])
			if ai != aj {
				return ai > aj
			}
			if bi != bj {
				return bi > bj
			}
			return t.Classes[i][0] < t.Classes[j][0]
		})
		t.Hetero = true
	} else {
		t.Classes = [][]int{append([]int(nil), t.Allowed...)}
	}
	return t
}

func (t topo) describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "allowed CPUs %s (%d), online %s (%d), heterogeneous=%v\n", fmtCPUs(t.Allowed), len(t.Allowed), fmtCPUs(t.Online), len(t.Online), t.Hetero)
	for i, g := range t.Classes {
		ci := t.CPU[g[0]]
		fmt.Fprintf(&b, "  class %d (fastest first): cpus %s, part %s %s\n", i, fmtCPUs(g), orDash(ci.Part), orDash(ci.PartName))
	}
	fmt.Fprintf(&b, "  cpu  class  part   cap   max_MHz  freq_domain")
	for _, id := range t.Allowed {
		c := t.CPU[id]
		fmt.Fprintf(&b, "\n  %-4d %-6s %-6s %-5d %-8s %s", id, c.class(), orDash(c.Part), c.Cap, mhz(int64(c.MaxKHz)), orDash(c.Domain))
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// byFastness orders CPUs of one group fastest first: capacity, then max
// frequency, then id.
func (t topo) byFastness(cpus []int) []int {
	out := append([]int(nil), cpus...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := t.CPU[out[i]], t.CPU[out[j]]
		if a.Cap != b.Cap {
			return a.Cap > b.Cap
		}
		if a.MaxKHz != b.MaxKHz {
			return a.MaxKHz > b.MaxKHz
		}
		return a.ID < b.ID
	})
	return out
}

// caseCPUs picks the CPUs of a P3 case. The case names are the same on every
// host (the stress tally fails a run whose two arches did not run the same
// leaves); what they select differs:
//
//	all    every allowed CPU; the engine's own loop pinning is left alone (the failing run's shape)
//	nopin  every allowed CPU; after Start every engine loop thread is given the FULL allowed
//	       mask again (sched_setaffinity on its tid), so no loop is pinned at all
//	fast8  the 8 fastest-core-type CPUs      (heterogeneous: the A720s on msr1)
//	fast4  the 4 fastest of those            (msr1: cpus 0,1,10,11 at 2.5-2.6 GHz)
//	slow4  the 4 slowest-core-type CPUs      (msr1: the A520s, cpus 2-5)
//
// fast4 next to slow4 separates "slow cores" from "only 4 CPUs". On a
// homogeneous host the same counts are taken by id: fast8 the highest 8, fast4
// the highest 4, slow4 the lowest 4.
func (t topo) caseCPUs(name string) (cpus []int, note string, err error) {
	n := len(t.Allowed)
	take := func(src []int, k int) []int {
		if k > len(src) {
			k = len(src)
		}
		return append([]int(nil), src[:k]...)
	}
	asc := func(c []int) []int { c = append([]int(nil), c...); sort.Ints(c); return c }
	switch name {
	case "all":
		return asc(t.Allowed), "every allowed CPU, the engine's loop pinning left alone", nil
	case "nopin":
		return asc(t.Allowed), "every allowed CPU, every loop thread unpinned (full mask) after Start", nil
	case "fast8", "fast4":
		k := 8
		if name == "fast4" {
			k = 4
		}
		if t.Hetero {
			cpus = asc(take(t.byFastness(t.Classes[0]), k))
			note = "the fastest core type, fastest CPUs first"
		} else {
			hi := asc(t.Allowed)
			cpus = asc(take(reverse(hi), k))
			note = "homogeneous host: the highest ids"
		}
	case "slow4":
		if t.Hetero {
			cpus = asc(take(asc(t.Classes[len(t.Classes)-1]), 4))
			note = "the slowest core type"
		} else {
			cpus = asc(take(asc(t.Allowed), 4))
			note = "homogeneous host: the lowest ids"
		}
	default:
		return nil, "", fmt.Errorf("unknown case %q (all, nopin, fast8, fast4, slow4; extra: allperm, fast8s1, fast8s2, fast6s4, fast4s4, slow4wide, fast8wide, fast8u1, pin720, pin520, gmp8)", name)
	}
	if len(cpus) < 2 {
		return nil, "", fmt.Errorf("case %s: %d allowed CPUs on this host (%d allowed in all), the engine needs at least 2 workers", name, len(cpus), n)
	}
	if len(cpus) < map[string]int{"fast8": 8, "fast4": 4, "slow4": 4}[name] {
		note += fmt.Sprintf(" (only %d CPUs available)", len(cpus))
	}
	return cpus, note, nil
}

// caseSpec is a P3 case that needs more than a CPU list: a child mask that is
// wider than the loops' CPUs, a Workers count, a GOMAXPROCS, or loops that are
// left unpinned. The first five cases (all, nopin, fast8, fast4, slow4) keep
// the code path of caseCPUs; every later case is a spec (extraCase).
type caseSpec struct {
	CPUs       []int // CPUs the case names, for the log (the loops' CPUs, or the child's mask)
	Mask       []int // the child's CPU mask at fork (nil: the parent's)
	Workers    int   // Config.Workers (0: the default, GOMAXPROCS)
	Plan       []int // loop k (loops in the order of the CPU the engine pinned them to) is moved to Plan[k]; -1 = unpinned (full mask)
	Keep       []int // loops the engine pinned to one of these CPUs stay where the engine put them; every other loop is unpinned
	GOMAXPROCS int   // the child's GOMAXPROCS (0: default)
	Note       string
}

// extraCase returns the spec of the cases added after the first run (37975655016,
// where the stalled loop was a loop pinned on a Cortex-A520 in 19 of 19
// snapshots). Same names on every host. On a host with one core type "fast" is
// the highest CPU ids and "slow" the lowest, as in caseCPUs, so the shapes
// exist on both arches (the stress tally wants the same leaves everywhere).
//
//	allperm    12 loops, one per CPU, but loop k is moved to CPU n-1-k: the same CPU set
//	           as all, a different loop-to-CPU map and the probe (not the engine) does the pinning
//	fast8s1    one loop on each of the 8 fastest CPUs and one on the slowest CPU (9 loops, mask = those 9)
//	fast8s2    ... and two slow CPUs (10)
//	fast6s4    6 fastest + the 4 slowest (10)
//	fast4s4    4 fastest + the 4 slowest (8)
//	slow4wide  4 loops on the 4 slowest CPUs, the process mask is every CPU (the fast CPUs are free for the client)
//	fast8wide  8 loops on the 8 fastest CPUs, the process mask is every CPU (the slow CPUs are free)
//	fast8u1    8 loops on the 8 fastest CPUs and a ninth loop left unpinned, mask = every CPU
//	pin720     every CPU has a loop; the loops on the fastest core type stay pinned, the others are unpinned
//	pin520     every CPU has a loop; the loops on the slowest core type stay pinned, the others are unpinned
//	gmp8       the engine's own pinning with GOMAXPROCS=8 on all CPUs (8 loops pinned to CPU 0-7, CPUs 8+ without a loop)
func (t topo) extraCase(name string) (caseSpec, bool, error) {
	var fast, slow []int
	if t.Hetero {
		fast = t.byFastness(t.Classes[0])
		s := append([]int(nil), t.Classes[len(t.Classes)-1]...)
		sort.Ints(s)
		slow = s
	} else {
		a := append([]int(nil), t.Allowed...)
		sort.Ints(a)
		fast = reverse(a)
		slow = a
	}
	take := func(src []int, k int) []int {
		if k > len(src) {
			k = len(src)
		}
		return append([]int(nil), src[:k]...)
	}
	union := func(a, b []int) []int {
		seen := map[int]bool{}
		var out []int
		for _, c := range append(append([]int(nil), a...), b...) {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
		sort.Ints(out)
		return out
	}
	all := append([]int(nil), t.Allowed...)
	sort.Ints(all)
	var sp caseSpec
	switch name {
	case "allperm":
		sp = caseSpec{CPUs: all, Workers: 0, Plan: reverse(all), Note: "every CPU has a loop, loop k moved to CPU n-1-k (all's CPU set, another map)"}
	case "fast8s1":
		sp.CPUs = union(take(fast, 8), take(slow, 1))
		sp.Note = "the 8 fastest CPUs and the slowest one, a loop on each"
	case "fast8s2":
		sp.CPUs = union(take(fast, 8), take(slow, 2))
		sp.Note = "the 8 fastest CPUs and the 2 slowest, a loop on each"
	case "fast6s4":
		sp.CPUs = union(take(fast, 6), take(slow, 4))
		sp.Note = "the 6 fastest CPUs and the 4 slowest, a loop on each"
	case "fast4s4":
		sp.CPUs = union(take(fast, 4), take(slow, 4))
		sp.Note = "the 4 fastest CPUs and the 4 slowest, a loop on each"
	case "slow4wide":
		sp = caseSpec{CPUs: take(slow, 4), Mask: all, Workers: len(take(slow, 4)), Plan: take(slow, 4), Note: "4 loops on the slowest CPUs, every CPU in the process mask"}
	case "fast8wide":
		sp = caseSpec{CPUs: take(fast, 8), Mask: all, Workers: len(take(fast, 8)), Plan: take(fast, 8), Note: "8 loops on the fastest CPUs, every CPU in the process mask"}
	case "fast8u1":
		f := take(fast, 8)
		sp = caseSpec{CPUs: f, Mask: all, Workers: len(f) + 1, Plan: append(append([]int(nil), f...), -1), Note: "8 loops on the fastest CPUs, a ninth loop unpinned, every CPU in the process mask"}
	case "pin720":
		sp = caseSpec{CPUs: all, Keep: take(fast, 8), Note: "every CPU has a loop; only the loops the engine put on the 8 fastest CPUs stay pinned"}
	case "pin520":
		sp = caseSpec{CPUs: all, Keep: take(slow, 4), Note: "every CPU has a loop; only the loops the engine put on the 4 slowest CPUs stay pinned"}
	case "gmp8":
		sp = caseSpec{CPUs: all, GOMAXPROCS: 8, Note: "the engine's own pinning, GOMAXPROCS=8 (8 loops on CPU 0-7)"}
	default:
		return caseSpec{}, false, nil
	}
	switch name {
	case "fast8s1", "fast8s2", "fast6s4", "fast4s4":
		sp.Mask, sp.Workers, sp.Plan = sp.CPUs, len(sp.CPUs), sp.CPUs
	}
	if len(sp.CPUs) < 2 {
		return caseSpec{}, true, fmt.Errorf("case %s: %d usable CPUs on this host, the engine needs at least 2 workers", name, len(sp.CPUs))
	}
	if name == "gmp8" && len(all) <= 8 {
		sp.Note += fmt.Sprintf(" (only %d CPUs here: GOMAXPROCS=8 changes nothing)", len(all))
	}
	return sp, true, nil
}

func reverse(s []int) []int {
	out := make([]int, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

// ---- per-thread affinity ---------------------------------------------------

func taskIDs() []int {
	ents, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range ents {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// isIOThread reports whether a thread is a kernel-created io_uring thread (an
// io-wq worker "iou-wrk-*" or an SQPOLL thread "iou-sqp-*"). They belong to a
// ring and not to an engine loop: they appear lazily during a workload and can
// inherit the pin of the thread that created them, so they are never counted
// as loop threads (and their affinity is not settable from here).
func isIOThread(tid int) bool {
	return strings.HasPrefix(threadComm(tid), "iou-")
}

// ioThreads describes the io_uring kernel threads of the process, for notes.
func ioThreads() string {
	var parts []string
	for _, tid := range taskIDs() {
		if isIOThread(tid) {
			parts = append(parts, fmt.Sprintf("%d %s allowed %s", tid, threadComm(tid), threadAllowed(tid)))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

// pinnedThreads maps tid -> cpu for every thread of this process, io_uring
// kernel threads excepted, whose affinity is exactly one CPU.
func pinnedThreads() map[int]int {
	out := map[int]int{}
	for _, tid := range taskIDs() {
		if isIOThread(tid) {
			continue
		}
		var s unix.CPUSet
		if err := unix.SchedGetaffinity(tid, &s); err != nil {
			continue
		}
		if s.Count() == 1 {
			out[tid] = cpuList(&s)[0]
		}
	}
	return out
}

func threadComm(tid int) string {
	return readTrim(fmt.Sprintf("/proc/self/task/%d/comm", tid))
}

func threadAllowed(tid int) string {
	var s unix.CPUSet
	if err := unix.SchedGetaffinity(tid, &s); err != nil {
		return "?"
	}
	return fmtCPUs(cpuList(&s))
}
