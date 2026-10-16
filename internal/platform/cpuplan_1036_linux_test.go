//go:build linux

package platform

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// celeris#1036. On a host whose allowed CPUs differ in capacity, the plan has
// no unpinned entry and no little CPU in it, whatever the allowed set and the
// worker count. The expected values below come from the fixture's own table of
// which CPU is big, never from the planner.
//
// Before the fix the plan held `numWorkers` entries and left the ones past the
// big CPUs at -1 ("unpinned"); an arm64 A/B on msr1 put the throughput loss on
// those floating loops.

// fixture is a 12-CPU msr1-like machine: CPUs 0-1 and 6-11 big, 2-5 little.
type fixture struct {
	name  string
	build func(t *testing.T, allowed []int) *fakeHost
}

var fixtures1036 = []fixture{
	{"cpu_capacity files", msr1},
	{"cpuinfo fallback, no capacity files", func(t *testing.T, allowed []int) *fakeHost {
		const a720, a520 = "0x41:0xd81", "0x41:0xd80"
		h := newFakeHost(t, allowed)
		h.cpuinfo([]string{a720, a720, a520, a520, a520, a520, a720, a720, a720, a720, a720, a720})
		return h
	}},
}

var bigCPUs1036 = []int{0, 1, 6, 7, 8, 9, 10, 11}

func cpusIn(set, of []int) []int {
	var out []int
	for _, c := range set {
		if slices.Contains(of, c) {
			out = append(out, c)
		}
	}
	return out
}

func TestPlanHeterogeneousNeverUnpinnedNeverLittle1036(t *testing.T) {
	cases := []struct {
		name    string
		allowed []int
	}{
		{"unrestricted 0-11", rng(0, 11)},
		{"cpuset leaving 2 big and 4 little", rng(0, 5)},
		{"taskset 0-1,6-7 and 2-3 (4 big, 2 little)", []int{0, 1, 2, 3, 6, 7}},
		{"only one big CPU allowed (7) with the 4 little", []int{2, 3, 4, 5, 7}},
		{"only one big CPU allowed (11) with 1 little", []int{4, 11}},
		{"only little CPUs allowed", rng(2, 5)},
		{"only big CPUs allowed", bigCPUs1036},
		{"one big CPU alone", []int{9}},
		{"one little CPU alone", []int{3}},
		{"big CPUs 6-11 and little 5", rng(5, 11)},
	}
	for _, fx := range fixtures1036 {
		for _, tc := range cases {
			t.Run(fx.name+"/"+tc.name, func(t *testing.T) {
				h := fx.build(t, tc.allowed)
				wantBig := cpusIn(bigCPUs1036, tc.allowed)
				wantLittle := cpusIn(rng(2, 5), tc.allowed)
				// Heterogeneous only when the allowed set mixes both kinds.
				het := len(wantBig) > 0 && len(wantLittle) > 0
				for workers := 0; workers <= 2*len(tc.allowed)+4; workers++ {
					for _, explicit := range []bool{false, true} {
						p := planWorkerCPUs(workers, explicit, h.src())
						tag := fmt.Sprintf("%d workers explicit=%v plan %v", workers, explicit, p.CPUs)
						if p.Heterogeneous != het {
							t.Fatalf("%s: Heterogeneous=%v, want %v", tag, p.Heterogeneous, het)
						}
						for i, c := range p.CPUs {
							if c < 0 {
								t.Fatalf("%s: entry %d is %d, an unpinned loop", tag, i, c)
							}
							if !slices.Contains(tc.allowed, c) {
								t.Fatalf("%s: entry %d is CPU %d, outside the allowed set %v", tag, i, c, tc.allowed)
							}
						}
						if p.Pinned() != len(p.CPUs) {
							t.Fatalf("%s: %d of %d entries pinned", tag, p.Pinned(), len(p.CPUs))
						}
						if !het {
							// Alike CPUs: one entry per worker, whatever explicit says.
							if len(p.CPUs) != workers {
								t.Fatalf("%s: %d entries, want %d (a host of alike CPUs keeps every worker)", tag, len(p.CPUs), workers)
							}
							continue
						}
						eq(t, tag+": Little", p.Little, wantLittle)
						for i, c := range p.CPUs {
							if slices.Contains(wantLittle, c) {
								t.Fatalf("%s: entry %d is CPU %d, a little CPU", tag, i, c)
							}
						}
						wantLoops := min(workers, len(wantBig))
						if explicit {
							wantLoops = workers
						}
						if len(p.CPUs) != wantLoops {
							t.Fatalf("%s: %d loops, want %d", tag, len(p.CPUs), wantLoops)
						}
						if p.Requested != workers {
							t.Fatalf("%s: Requested=%d", tag, p.Requested)
						}
						// Balance: every big CPU holds a loop before one holds a second.
						count := map[int]int{}
						for _, c := range p.CPUs {
							count[c]++
						}
						lo, hi := len(p.CPUs), 0
						for _, c := range wantBig {
							lo, hi = min(lo, count[c]), max(hi, count[c])
						}
						if hi-lo > 1 {
							t.Fatalf("%s: %d loops on one big CPU and %d on another", tag, hi, lo)
						}
					}
				}
			})
		}
	}
}

// The three regimes the decision names: fewer workers than big CPUs, exactly
// as many, and more (a user-set count, which wraps and stays pinned).
func TestPlanHeterogeneousWorkerCountRegimes1036(t *testing.T) {
	for _, fx := range fixtures1036 {
		t.Run(fx.name, func(t *testing.T) {
			h := fx.build(t, rng(0, 11))
			for _, explicit := range []bool{false, true} {
				eq(t, fmt.Sprintf("explicit=%v 5 < 8 big", explicit),
					planWorkerCPUs(5, explicit, h.src()).CPUs, []int{0, 1, 6, 7, 8})
				eq(t, fmt.Sprintf("explicit=%v 8 == 8 big", explicit),
					planWorkerCPUs(8, explicit, h.src()).CPUs, bigCPUs1036)
			}
			eq(t, "default 12 > 8 big is 8 loops", planWorkerCPUs(12, false, h.src()).CPUs, bigCPUs1036)
			eq(t, "explicit 12 > 8 big wraps", planWorkerCPUs(12, true, h.src()).CPUs,
				[]int{0, 1, 6, 7, 8, 9, 10, 11, 0, 1, 6, 7})
			eq(t, "explicit 17 wraps twice", planWorkerCPUs(17, true, h.src()).CPUs,
				[]int{0, 1, 6, 7, 8, 9, 10, 11, 0, 1, 6, 7, 8, 9, 10, 11, 0})
		})
	}
}

// One big CPU in the allowed set is one loop by default, below
// resource.MinWorkers: deliberate (a second loop could only share that CPU or
// run on a little one). io_uring already runs on one worker under a low
// RLIMIT_MEMLOCK; epoll never did by default, its engine test serves requests
// on one loop (TestEpollServesOnOneLoop1036). A user-set count is honoured on
// that CPU.
func TestPlanOneBigCPUAllowed1036(t *testing.T) {
	h := msr1(t, []int{2, 3, 4, 5, 7})
	eq(t, "default", planWorkerCPUs(12, false, h.src()).CPUs, []int{7})
	eq(t, "explicit 3", planWorkerCPUs(3, true, h.src()).CPUs, []int{7, 7, 7})
}

// With NUMA nodes the big CPUs are taken from the nodes in turn, as any
// allowed set is, and the wrap starts only after every big CPU holds a loop.
func TestPlanHeterogeneousUsesTheNodesInTurn1036(t *testing.T) {
	h := msr1(t, rng(0, 11))
	h.nodes = [][]int{rng(0, 5), rng(6, 11)}
	eq(t, "default", planWorkerCPUs(12, false, h.src()).CPUs, []int{0, 6, 1, 7, 8, 9, 10, 11})
	eq(t, "explicit 10", planWorkerCPUs(10, true, h.src()).CPUs, []int{0, 6, 1, 7, 8, 9, 10, 11, 0, 6})
}

// The plan's note says which CPUs the loops use, not only the set they were
// drawn from: 5 loops of 8 non-little CPUs do not read as if all 8 were used.
func TestPlanNoteNamesTheCPUsTheLoopsUse1036(t *testing.T) {
	for _, fx := range fixtures1036 {
		t.Run(fx.name, func(t *testing.T) {
			h := fx.build(t, rng(0, 11))
			p := planWorkerCPUs(5, true, h.src())
			if want := "5 loops pinned to the CPUs 0-1,6-8 of the other allowed CPUs 0-1,6-11"; !strings.Contains(p.Note, want) {
				t.Errorf("note %q lacks %q", p.Note, want)
			}
			p = planWorkerCPUs(8, false, h.src())
			if want := "8 loops pinned to the CPUs 0-1,6-11 of the other allowed CPUs 0-1,6-11"; !strings.Contains(p.Note, want) {
				t.Errorf("note %q lacks %q", p.Note, want)
			}
		})
	}
}

// Truncate cuts a plan to the loops an engine can fund. The first n entries
// of a plan are the plan for n workers, and the note says how many start.
func TestPlanTruncate1036(t *testing.T) {
	h := msr1(t, rng(0, 11))
	full := planWorkerCPUs(12, true, h.src())
	for n := 1; n < len(full.CPUs); n++ {
		cut := full.Truncate(n, "a test")
		eq(t, fmt.Sprintf("truncate to %d", n), cut.CPUs, planWorkerCPUs(n, true, h.src()).CPUs)
		if want := fmt.Sprintf("only %d of the 12 planned loops start (a test)", n); !strings.Contains(cut.Note, want) {
			t.Errorf("truncate to %d: note %q lacks %q", n, cut.Note, want)
		}
		if cut.Pinned() != n || cut.Requested != 12 {
			t.Errorf("truncate to %d: pinned %d requested %d", n, cut.Pinned(), cut.Requested)
		}
	}
	if len(full.CPUs) != 12 {
		t.Errorf("Truncate changed the plan it was called on: %v", full.CPUs)
	}
	for _, n := range []int{0, -1, 12, 40} {
		if got := full.Truncate(n, "x"); !slices.Equal(got.CPUs, full.CPUs) || got.Note != full.Note {
			t.Errorf("Truncate(%d) changed the plan: %v / %q", n, got.CPUs, got.Note)
		}
	}
	alike := planWorkerCPUs(4, false, planSource{allowed: func() ([]int, error) { return []int{0, 1, 2, 3}, nil }})
	if cut := alike.Truncate(2, "x"); cut.Note != "" || len(cut.CPUs) != 2 {
		t.Errorf("alike CPUs: %v note %q (a plan without a note gets none)", cut.CPUs, cut.Note)
	}
}
