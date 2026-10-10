//go:build linux

package platform

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// fakeHost builds a sysfs/procfs tree in a temp dir and a planSource over it.
type fakeHost struct {
	t       *testing.T
	dir     string
	allowed []int
	nodes   [][]int
}

func newFakeHost(t *testing.T, allowed []int) *fakeHost {
	t.Helper()
	return &fakeHost{t: t, dir: t.TempDir(), allowed: allowed}
}

// capacity writes cpuN/cpu_capacity.
func (h *fakeHost) capacity(cpu int, val string) {
	h.t.Helper()
	d := filepath.Join(h.dir, "cpu", "cpu"+strconv.Itoa(cpu))
	if err := os.MkdirAll(d, 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "cpu_capacity"), []byte(val+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// cpuinfo writes /proc/cpuinfo blocks, one "implementer:part" per CPU index.
func (h *fakeHost) cpuinfo(parts []string) {
	h.t.Helper()
	var b strings.Builder
	for i, ip := range parts {
		impl, part, _ := strings.Cut(ip, ":")
		b.WriteString("processor\t: " + strconv.Itoa(i) + "\nBogoMIPS\t: 2000.00\nFeatures\t: fp asimd\n")
		b.WriteString("CPU implementer\t: " + impl + "\nCPU architecture: 8\nCPU variant\t: 0x1\nCPU part\t: " + part + "\nCPU revision\t: 1\n\n")
	}
	if err := os.WriteFile(filepath.Join(h.dir, "cpuinfo"), []byte(b.String()), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) src() planSource {
	return planSource{
		allowed:  func() ([]int, error) { return h.allowed, nil },
		cpuDir:   filepath.Join(h.dir, "cpu"),
		cpuinfo:  filepath.Join(h.dir, "cpuinfo"),
		nodeCPUs: h.nodes,
	}
}

func rng(lo, hi int) []int {
	var out []int
	for c := lo; c <= hi; c++ {
		out = append(out, c)
	}
	return out
}

// msr1 is the shape of the arm64 host the stalls were seen on: CPUs 0-1 and
// 6-11 are Cortex-A720s (capacity 866 to 1024, not one value), CPUs 2-5 are
// Cortex-A520s (279).
func msr1(t *testing.T, allowed []int) *fakeHost {
	t.Helper()
	h := newFakeHost(t, allowed)
	big := map[int]string{0: "1024", 1: "1024", 6: "866", 7: "866", 8: "909", 9: "909", 10: "960", 11: "1024"}
	for c := 0; c < 12; c++ {
		if v, ok := big[c]; ok {
			h.capacity(c, v)
		} else {
			h.capacity(c, "279")
		}
	}
	return h
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

func TestPlanMSR1LikeAvoidsLittleCPUs(t *testing.T) {
	h := msr1(t, rng(0, 11))
	// 12 loops: the 8 big CPUs are pinned in order, the other 4 loops run unpinned.
	p := planWorkerCPUs(12, h.src())
	eq(t, "cpus(12)", p.CPUs, []int{0, 1, 6, 7, 8, 9, 10, 11, -1, -1, -1, -1})
	eq(t, "little", p.Little, []int{2, 3, 4, 5})
	if !p.Heterogeneous || p.Note == "" {
		t.Errorf("heterogeneous=%v note=%q, want true and a reason", p.Heterogeneous, p.Note)
	}
	if p.Pinned() != 8 {
		t.Errorf("Pinned() = %d, want 8", p.Pinned())
	}
	// Fewer loops than big CPUs: all pinned, none on an A520.
	eq(t, "cpus(4)", planWorkerCPUs(4, h.src()).CPUs, []int{0, 1, 6, 7})
	eq(t, "cpus(8)", planWorkerCPUs(8, h.src()).CPUs, []int{0, 1, 6, 7, 8, 9, 10, 11})
	// One more loop than big CPUs does not wrap onto them.
	eq(t, "cpus(9)", planWorkerCPUs(9, h.src()).CPUs, []int{0, 1, 6, 7, 8, 9, 10, 11, -1})
	eq(t, "cpus(0)", planWorkerCPUs(0, h.src()).CPUs, []int{})
}

// The A720s of msr1 read 866 to 1024. A planner that took "little" to mean
// "not equal to the maximum" would call 866 and 909 little.
func TestPlanCapacityVariationAmongBigCPUsIsNotLittle(t *testing.T) {
	h := newFakeHost(t, rng(0, 7))
	for c, v := range []string{"1024", "866", "909", "960", "1024", "866", "880", "1000"} {
		h.capacity(c, v)
	}
	p := planWorkerCPUs(8, h.src())
	if p.Heterogeneous {
		t.Fatalf("capacities 866..1024 planned as heterogeneous: little %v", p.Little)
	}
	eq(t, "cpus", p.CPUs, rng(0, 7))
}

func TestPlanLittleThresholdIsBelowHalf(t *testing.T) {
	for _, tc := range []struct {
		small      string
		wantLittle bool
	}{
		{"511", true},
		{"512", false}, // exactly 50% is not below 50%
		{"600", false},
	} {
		h := newFakeHost(t, rng(0, 3))
		h.capacity(0, "1024")
		h.capacity(1, "1024")
		h.capacity(2, tc.small)
		h.capacity(3, tc.small)
		p := planWorkerCPUs(4, h.src())
		if p.Heterogeneous != tc.wantLittle {
			t.Errorf("capacity %s next to 1024: heterogeneous=%v, want %v", tc.small, p.Heterogeneous, tc.wantLittle)
		}
	}
}

func TestPlanMaskedAllowedSetStaysInsideTheMask(t *testing.T) {
	// The probe's fast8 child: mask 0-1,6-11 of msr1. Only A720s are allowed, so
	// the set is homogeneous and the 8 loops take the 8 members in order, never
	// CPUs 2-5 (celeris#909: the old code pinned CPUs 0-7).
	allowed := append(rng(0, 1), rng(6, 11)...)
	h := msr1(t, allowed)
	p := planWorkerCPUs(8, h.src())
	eq(t, "cpus", p.CPUs, allowed)
	if p.Heterogeneous {
		t.Errorf("heterogeneous = true for an all-A720 mask")
	}
	// More loops than members wrap inside the set.
	eq(t, "cpus(10)", planWorkerCPUs(10, h.src()).CPUs, []int{0, 1, 6, 7, 8, 9, 10, 11, 0, 1})
}

func TestPlanTasksetOnAHomogeneousHost(t *testing.T) {
	h := newFakeHost(t, rng(4, 7)) // taskset -c 4-7 on an 8-CPU x86 host, no cpu_capacity files
	eq(t, "cpus(4)", planWorkerCPUs(4, h.src()).CPUs, []int{4, 5, 6, 7})
	eq(t, "cpus(6)", planWorkerCPUs(6, h.src()).CPUs, []int{4, 5, 6, 7, 4, 5})
	h = newFakeHost(t, []int{1, 2, 3})
	eq(t, "cpus(3)", planWorkerCPUs(3, h.src()).CPUs, []int{1, 2, 3})
}

func TestPlanX86LikeHomogeneousIsTheOldRule(t *testing.T) {
	h := newFakeHost(t, rng(0, 7))
	p := planWorkerCPUs(10, h.src())
	eq(t, "cpus", p.CPUs, []int{0, 1, 2, 3, 4, 5, 6, 7, 0, 1})
	if p.Heterogeneous || p.Note != "" || len(p.Little) != 0 {
		t.Errorf("homogeneous host: %+v", p)
	}
	// Equal capacities (arm64 host with one core type) are homogeneous too.
	for c := 0; c < 8; c++ {
		h.capacity(c, "1024")
	}
	eq(t, "equal caps", planWorkerCPUs(10, h.src()).CPUs, p.CPUs)
}

func TestPlanSingleCPU(t *testing.T) {
	h := newFakeHost(t, []int{5})
	eq(t, "one worker", planWorkerCPUs(1, h.src()).CPUs, []int{5})
	eq(t, "three workers", planWorkerCPUs(3, h.src()).CPUs, []int{5, 5, 5})
}

func TestPlanMissingOrBrokenCapacityIsHomogeneous(t *testing.T) {
	// No cpu_capacity files at all.
	h := newFakeHost(t, rng(0, 3))
	eq(t, "missing", planWorkerCPUs(4, h.src()).CPUs, rng(0, 3))
	// One allowed CPU has no file: not enough to classify, assume homogeneous.
	h = newFakeHost(t, rng(0, 3))
	h.capacity(0, "1024")
	h.capacity(1, "1024")
	h.capacity(2, "100")
	eq(t, "one file missing", planWorkerCPUs(4, h.src()).CPUs, rng(0, 3))
	// Garbage, zero and negative values.
	for _, bad := range []string{"", "abc", "0", "-5"} {
		h = newFakeHost(t, rng(0, 3))
		h.capacity(0, "1024")
		h.capacity(1, "1024")
		h.capacity(2, bad)
		h.capacity(3, "100")
		if p := planWorkerCPUs(4, h.src()); p.Heterogeneous {
			t.Errorf("capacity %q: planned as heterogeneous: %+v", bad, p)
		}
	}
	// Capacity files of CPUs outside the allowed set are not read.
	h = newFakeHost(t, rng(0, 1))
	h.capacity(0, "1024")
	h.capacity(1, "1024")
	h.capacity(2, "100")
	if p := planWorkerCPUs(2, h.src()); p.Heterogeneous {
		t.Errorf("a little CPU outside the mask made the plan heterogeneous: %+v", p)
	}
}

func TestPlanCPUInfoFallback(t *testing.T) {
	const a720, a520 = "0x41:0xd81", "0x41:0xd80"
	parts := []string{a720, a720, a520, a520, a520, a520, a720, a720, a720, a720, a720, a720}
	// No cpu_capacity files: the CPU parts of /proc/cpuinfo decide.
	h := newFakeHost(t, rng(0, 11))
	h.cpuinfo(parts)
	p := planWorkerCPUs(12, h.src())
	eq(t, "cpus", p.CPUs, []int{0, 1, 6, 7, 8, 9, 10, 11, -1, -1, -1, -1})
	eq(t, "little", p.Little, []int{2, 3, 4, 5})
	// A mask of the A520s only is homogeneous.
	h = newFakeHost(t, rng(2, 5))
	h.cpuinfo(parts)
	if p := planWorkerCPUs(4, h.src()); p.Heterogeneous {
		t.Errorf("all-A520 mask: %+v", p)
	}
	// A mask without them is homogeneous.
	h = newFakeHost(t, append(rng(0, 1), rng(6, 11)...))
	h.cpuinfo(parts)
	if p := planWorkerCPUs(8, h.src()); p.Heterogeneous {
		t.Errorf("all-A720 mask: %+v", p)
	}
	// A host with one known-little part only (an all-A55 board) is homogeneous.
	h = newFakeHost(t, rng(0, 3))
	h.cpuinfo([]string{"0x41:0xd05", "0x41:0xd05", "0x41:0xd05", "0x41:0xd05"})
	if p := planWorkerCPUs(4, h.src()); p.Heterogeneous {
		t.Errorf("all-A55: %+v", p)
	}
	// Two unknown parts: no table entry, so no guess.
	h = newFakeHost(t, rng(0, 1))
	h.cpuinfo([]string{"0x41:0xd81", "0x41:0xd82"})
	if p := planWorkerCPUs(2, h.src()); p.Heterogeneous {
		t.Errorf("two big parts: %+v", p)
	}
	// An allowed CPU the file does not describe: homogeneous.
	h = newFakeHost(t, rng(0, 5))
	h.cpuinfo(parts[:4])
	if p := planWorkerCPUs(4, h.src()); p.Heterogeneous {
		t.Errorf("short cpuinfo: %+v", p)
	}
	// A capacity file wins over cpuinfo: all capacities equal, parts mixed.
	h = newFakeHost(t, rng(0, 3))
	h.cpuinfo([]string{a720, a720, a520, a520})
	for c := 0; c < 4; c++ {
		h.capacity(c, "1024")
	}
	if p := planWorkerCPUs(4, h.src()); p.Heterogeneous {
		t.Errorf("equal capacities with mixed parts: %+v", p)
	}
}

func TestPlanAllowedErrorFallsBackToTheCPUCount(t *testing.T) {
	h := newFakeHost(t, nil)
	src := h.src()
	src.allowed = func() ([]int, error) { return nil, errors.New("boom") }
	p := planWorkerCPUs(3, src)
	if len(p.CPUs) != 3 || p.CPUs[0] != 0 || p.Heterogeneous {
		t.Errorf("fallback plan: %+v", p)
	}
}

func TestPlanNUMAIntersectsTheMask(t *testing.T) {
	// Two nodes, 0-3 and 4-7. The mask 2-3,4-6 keeps two CPUs of node 0 and
	// three of node 1. Workers alternate between the nodes, each taking its
	// next allowed CPU, and the node that runs out is skipped: every allowed
	// CPU gets a loop before one gets a second, and none leaves the mask.
	h := newFakeHost(t, []int{2, 3, 4, 5, 6})
	h.nodes = [][]int{rng(0, 3), rng(4, 7)}
	eq(t, "interleave", planWorkerCPUs(5, h.src()).CPUs, []int{2, 4, 3, 5, 6})
	eq(t, "wrap", planWorkerCPUs(7, h.src()).CPUs, []int{2, 4, 3, 5, 6, 2, 4})
	// A node the mask excludes entirely contributes nothing: its turns do not
	// fall back to CPUs the other node already used.
	h = newFakeHost(t, []int{4, 5, 6, 7})
	h.nodes = [][]int{rng(0, 3), rng(4, 7)}
	eq(t, "one node of two", planWorkerCPUs(4, h.src()).CPUs, []int{4, 5, 6, 7})
	// No node list intersects the mask (stale sysfs): plain round-robin on the set.
	h = newFakeHost(t, []int{8, 9})
	h.nodes = [][]int{rng(0, 3), rng(4, 7)}
	eq(t, "no intersection", planWorkerCPUs(3, h.src()).CPUs, []int{8, 9, 8})
	// A member no node lists (stale sysfs) comes after the nodes' CPUs.
	h = newFakeHost(t, []int{2, 3, 9})
	h.nodes = [][]int{rng(0, 3), rng(4, 7)}
	eq(t, "unlisted member", planWorkerCPUs(4, h.src()).CPUs, []int{2, 3, 9, 2})
}

// celeris#909 (review of #973): a cpuset or taskset that covers one NUMA node,
// or covers the nodes unevenly, must give every allowed CPU a loop before any
// CPU gets a second, whatever the number of loops. The first version of the
// interleave alternated over every node, and the turns of a node the mask left
// short or empty went to CPUs the other node had already used.
func TestPlanNUMARestrictedShapesBalanceTheLoops(t *testing.T) {
	cases := []struct {
		name    string
		allowed []int
		nodes   [][]int
	}{
		{"one socket of two, 4 CPUs", rng(4, 7), [][]int{rng(0, 3), rng(4, 7)}},
		{"one socket of two, 32 CPUs", rng(32, 63), [][]int{rng(0, 31), rng(32, 63)}},
		{"the other socket", rng(0, 31), [][]int{rng(0, 31), rng(32, 63)}},
		{"uneven cover 2+3", []int{2, 3, 4, 5, 6}, [][]int{rng(0, 3), rng(4, 7)}},
		{"kubernetes-like 2-7,8-9", append(rng(2, 7), 8, 9), [][]int{rng(0, 7), rng(8, 15)}},
		{"three nodes, one empty", append(rng(0, 1), rng(8, 11)...), [][]int{rng(0, 3), rng(4, 7), rng(8, 11)}},
		{"unrestricted, nodes of 2 and 14", rng(0, 15), [][]int{rng(0, 1), rng(2, 15)}},
		{"unrestricted, an empty node", rng(0, 15), [][]int{rng(0, 15), nil}},
		{"unrestricted, equal nodes", rng(0, 7), [][]int{rng(0, 3), rng(4, 7)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t, tc.allowed)
			h.nodes = tc.nodes
			for workers := 0; workers <= 2*len(tc.allowed)+3; workers++ {
				plan := planWorkerCPUs(workers, h.src()).CPUs
				count := map[int]int{}
				for _, c := range plan {
					count[c]++
				}
				lo, hi := workers, 0 // loops per allowed CPU
				for _, c := range tc.allowed {
					lo, hi = min(lo, count[c]), max(hi, count[c])
					delete(count, c)
				}
				if len(count) > 0 {
					t.Errorf("%d workers: plan %v leaves the mask %s", workers, plan, FormatCPUs(tc.allowed))
				}
				if hi-lo > 1 {
					t.Errorf("%d workers on %d allowed CPUs: plan %v puts %d loops on one CPU and %d on another",
						workers, len(tc.allowed), plan, hi, lo)
				}
			}
		})
	}
}

func TestFormatCPUs(t *testing.T) {
	eq(t, "ranges", FormatCPUs([]int{11, 0, 1, 6, 7, 8, 9, 10}), "0-1,6-11")
	eq(t, "single", FormatCPUs([]int{3}), "3")
	eq(t, "empty", FormatCPUs(nil), "")
}

func TestPinToCPURefusesANegativeCPU(t *testing.T) {
	// Not locked, nothing pinned: the call must fail before it touches the mask.
	if err := PinToCPU(-1); !errors.Is(err, unix.EINVAL) {
		t.Errorf("PinToCPU(-1) = %v, want EINVAL", err)
	}
}

// The real host, in whatever container the test runs: the plan stays inside
// the thread's allowed set.
func TestPlanWorkerCPUsOnTheRealHost(t *testing.T) {
	allowed, err := schedAllowedCPUs()
	if err != nil {
		t.Fatalf("sched_getaffinity: %v", err)
	}
	p := PlanWorkerCPUs(len(allowed) + 3)
	if len(p.CPUs) != len(allowed)+3 {
		t.Fatalf("plan has %d entries", len(p.CPUs))
	}
	eq(t, "allowed", p.Allowed, allowed)
	for i, c := range p.CPUs {
		if c < 0 {
			continue
		}
		found := false
		for _, a := range allowed {
			found = found || a == c
		}
		if !found {
			t.Errorf("worker %d planned on CPU %d, outside the allowed set %v", i, c, allowed)
		}
	}
	t.Logf("real host: allowed %s, heterogeneous=%v, plan %v", FormatCPUs(allowed), p.Heterogeneous, p.CPUs)
}

// oldDistributeWorkers is DistributeWorkers as it was before celeris#909, kept
// here as the oracle for "a homogeneous host that restricts nothing gets
// today's plan".
func oldDistributeWorkers(numWorkers, numCPU int, nodeCPUs [][]int) []int {
	numaNodes := len(nodeCPUs)
	if numCPU <= 0 {
		numCPU = 1
	}
	cpus := make([]int, numWorkers)
	if numaNodes <= 1 || nodeCPUs == nil {
		for i := range numWorkers {
			cpus[i] = i % numCPU
		}
		return cpus
	}
	nodeIdx := make([]int, numaNodes)
	for i := range numWorkers {
		node := i % numaNodes
		list := nodeCPUs[node]
		if len(list) == 0 {
			cpus[i] = i % numCPU
			continue
		}
		cpus[i] = list[nodeIdx[node]%len(list)]
		nodeIdx[node]++
	}
	return cpus
}

// Nodes of equal size. A node list that is short, or empty, got the old plan's
// imbalance (the short node's CPUs reused while the long node's went unused):
// that is a deliberate change, covered by
// TestPlanNUMARestrictedShapesBalanceTheLoops.
func TestPlanUnrestrictedHomogeneousEqualsTheOldPlan(t *testing.T) {
	topologies := map[string][][]int{
		"one node":               nil,
		"two nodes, blocks":      {rng(0, 7), rng(8, 15)},
		"two nodes, interleaved": {{0, 2, 4, 6, 8, 10, 12, 14}, {1, 3, 5, 7, 9, 11, 13, 15}},
		"four nodes":             {rng(0, 3), rng(4, 7), rng(8, 11), rng(12, 15)},
	}
	for name, nodes := range topologies {
		for _, ncpu := range []int{1, 2, 4, 8, 16} {
			for _, workers := range []int{0, 1, 3, 4, 8, 16, 20, 33} {
				h := newFakeHost(t, rng(0, ncpu-1))
				if nodes != nil {
					// The node lists describe the 16-CPU machine; the mask is the
					// first ncpu of it, so only the 16-CPU case is "unrestricted".
					if ncpu != 16 {
						continue
					}
					h.nodes = nodes
				}
				got := planWorkerCPUs(workers, h.src()).CPUs
				want := oldDistributeWorkers(workers, ncpu, nodes)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s, %d CPUs, %d workers: got %v, old plan %v", name, ncpu, workers, got, want)
				}
			}
		}
	}
}
