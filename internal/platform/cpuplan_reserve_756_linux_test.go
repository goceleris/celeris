//go:build linux

package platform

import (
	"fmt"
	"slices"
	"testing"
)

// celeris#756, #757: a DEFAULT loop count (Config.Workers not set) on a host of
// alike CPUs leaves a reserve of Ps to the goroutines that are not loops: the
// plan holds n - max(1, n/8) loops, none for n <= 2. A user-set count is
// honoured, and so is the big.LITTLE plan. These tests pin the POLICY (the
// loop counts), not any latency: the figures are written out and the tests do
// not call defaultLoopReserve, so they compile and fail by assertion on a tree
// without the reserve.

// A request for n workers, default, on a host whose n CPUs are alike.
func TestPlanDefaultLoopReserve756(t *testing.T) {
	for _, tc := range []struct{ n, want int }{
		{0, 0}, {1, 1}, {2, 2}, // no reserve: a count that small would drop to one loop or none
		{3, 2}, {4, 3}, {7, 6},
		{8, 7},
		{9, 8}, {15, 14},
		{16, 14},
		{17, 15}, {24, 21}, {31, 28},
		{32, 28},
		{48, 42}, {64, 56},
	} {
		t.Run(fmt.Sprintf("n=%d", tc.n), func(t *testing.T) {
			h := newFakeHost(t, rng(0, max(tc.n, 1)-1))
			p := planWorkerCPUs(tc.n, false, h.src())
			if p.Heterogeneous {
				t.Fatalf("a host of alike CPUs planned as heterogeneous: %+v", p)
			}
			if len(p.CPUs) != tc.want {
				t.Fatalf("default %d workers: %d loops, want %d (plan %v)", tc.n, len(p.CPUs), tc.want, p.CPUs)
			}
			if p.Requested != tc.n {
				t.Errorf("Requested = %d, want %d (what the plan was asked for)", p.Requested, tc.n)
			}
			if p.Note != "" {
				t.Errorf("Note = %q: a homogeneous plan carries none", p.Note)
			}
			// Every loop on its own CPU, in order; the CPUs left are the last ones.
			for i, c := range p.CPUs {
				if c != i {
					t.Fatalf("loop %d on CPU %d, want CPU %d (plan %v)", i, c, i, p.CPUs)
				}
			}
		})
	}
}

// The headline case, alone: 16 identical CPUs, default request 16 -> 14 loops
// (rc1 gave 16). The same request with the count set by the user -> 16.
func TestPlanSixteenAlikeCPUs756(t *testing.T) {
	h := newFakeHost(t, rng(0, 15))
	got := planWorkerCPUs(16, false, h.src()).CPUs
	eq(t, "default 16 -> 14 loops", got, rng(0, 13))
	got = planWorkerCPUs(16, true, h.src()).CPUs
	eq(t, "explicit 16 -> 16 loops", got, rng(0, 15))
}

// A user-set count is planned exactly as asked, for every count a host can
// ask: this is the opt-out (Config.Workers = runtime.GOMAXPROCS(0) restores
// one loop per P).
func TestPlanExplicitCountIsHonoured756(t *testing.T) {
	h := newFakeHost(t, rng(0, 15))
	for n := 1; n <= 40; n++ {
		p := planWorkerCPUs(n, true, h.src())
		if len(p.CPUs) != n {
			t.Fatalf("explicit %d workers: %d loops", n, len(p.CPUs))
		}
		for i, c := range p.CPUs {
			if c != i%16 {
				t.Fatalf("explicit %d workers: loop %d on CPU %d, want %d", n, i, c, i%16)
			}
		}
	}
}

// 1..64 default workers on a host of that many CPUs, against the rule written
// out inline: loops = n - max(1, n/8) for n > 2, n otherwise.
func TestPlanDefaultReserveTable756(t *testing.T) {
	for n := 1; n <= 64; n++ {
		h := newFakeHost(t, rng(0, n-1))
		want := n
		if n > 2 {
			want = n - max(1, n/8)
		}
		if got := len(planWorkerCPUs(n, false, h.src()).CPUs); got != want {
			t.Errorf("default %d workers: %d loops, want %d", n, got, want)
		}
	}
}

// The reserve counts the workers asked for, not the CPUs allowed: a default
// request of 8 (GOMAXPROCS lowered under a CPU limit) on a 16-CPU host runs 7
// loops on the first 7 CPUs; a request above the CPUs (GOMAXPROCS set above
// them) wraps.
func TestPlanDefaultReserveCountsWorkersNotCPUs756(t *testing.T) {
	h := newFakeHost(t, rng(0, 15))
	eq(t, "default 8 of 16 CPUs", planWorkerCPUs(8, false, h.src()).CPUs, rng(0, 6))
	h = newFakeHost(t, rng(0, 3))
	eq(t, "default 8 of 4 CPUs wraps", planWorkerCPUs(8, false, h.src()).CPUs, []int{0, 1, 2, 3, 0, 1, 2})
}

// A read error means "homogeneous" (no allowed mask, no capacity files): the
// default count gets the reserve there too, an explicit one does not.
func TestPlanDefaultReserveWithoutAMask756(t *testing.T) {
	h := newFakeHost(t, nil)
	src := h.src()
	src.allowed = func() ([]int, error) { return nil, fmt.Errorf("boom") }
	if p := planWorkerCPUs(16, false, src); len(p.CPUs) != 14 || p.Heterogeneous {
		t.Errorf("default 16 without a mask: %d loops (%+v), want 14", len(p.CPUs), p)
	}
	if p := planWorkerCPUs(16, true, src); len(p.CPUs) != 16 {
		t.Errorf("explicit 16 without a mask: %d loops, want 16", len(p.CPUs))
	}
}

// The big.LITTLE plan (msr1: 12 CPUs, CPUs 2-5 little) is untouched: no reserve
// on top of "one loop per non-little CPU", whatever the default count is, so
// 12 asked -> 8 loops (workers=8 requested=12 pinned=8), and a count below the
// big CPUs is kept whole: 7 asked -> 7 loops (a reserve that leaked in would
// make it 6).
func TestPlanBigLittleIgnoresTheReserve756(t *testing.T) {
	h := msr1(t, rng(0, 11))
	for _, tc := range []struct {
		n        int
		explicit bool
		want     int
	}{
		{12, false, 8}, // msr1: workers=8 requested=12 pinned=8
		{8, false, 8},  // exactly the big CPUs: a reserve would make it 7
		{7, false, 7},  // below the big CPUs: kept whole
		{4, false, 4},
		{3, false, 3},
		{16, false, 8},
		{12, true, 12}, // a user-set count wraps over the big CPUs
		{8, true, 8},
		{16, true, 16},
	} {
		p := planWorkerCPUs(tc.n, tc.explicit, h.src())
		if !p.Heterogeneous {
			t.Fatalf("msr1-like host planned as homogeneous: %+v", p)
		}
		if len(p.CPUs) != tc.want || p.Pinned() != tc.want {
			t.Errorf("msr1-like, %d workers explicit=%v: %d loops (%d pinned), want %d",
				tc.n, tc.explicit, len(p.CPUs), p.Pinned(), tc.want)
		}
		if tc.n == 12 && !tc.explicit && !slices.Equal(p.CPUs, bigCPUs1036) {
			t.Errorf("msr1-like default 12: CPUs %v, want %v", p.CPUs, bigCPUs1036)
		}
	}
}
