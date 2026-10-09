//go:build linux

package stallprobe

import (
	"fmt"
	"reflect"
	"testing"
)

// TestProbeCaseSpecs checks the CPU sets, loop plans and runtime knobs of the
// extra P3 cases (topo.extraCase) against a synthetic msr1 (4 Cortex-A520 on
// cpus 2-5, 8 Cortex-A720 elsewhere, capacities as measured in run
// 37975655016) and a synthetic 16-CPU homogeneous host. It needs no hardware.
func TestProbeCaseSpecs(t *testing.T) {
	msr1 := topo{Hetero: true, CPU: map[int]cpuInfo{}}
	capOf := map[int]int{0: 1024, 1: 1024, 2: 279, 3: 279, 4: 279, 5: 279, 6: 905, 7: 905, 8: 866, 9: 866, 10: 984, 11: 984}
	khz := map[int]int{0: 2600000, 1: 2600000, 2: 1800000, 3: 1800000, 4: 1800000, 5: 1800000, 6: 2300000, 7: 2300000, 8: 2200000, 9: 2200000, 10: 2500000, 11: 2500000}
	for c := 0; c < 12; c++ {
		part := "0xd81"
		if c >= 2 && c <= 5 {
			part = "0xd80"
		}
		msr1.Allowed = append(msr1.Allowed, c)
		msr1.CPU[c] = cpuInfo{ID: c, Part: part, PartName: armParts[part], Cap: capOf[c], MaxKHz: khz[c]}
	}
	msr1.Classes = [][]int{{0, 1, 6, 7, 8, 9, 10, 11}, {2, 3, 4, 5}}
	flat := topo{CPU: map[int]cpuInfo{}}
	for c := 0; c < 16; c++ {
		flat.Allowed = append(flat.Allowed, c)
		flat.CPU[c] = cpuInfo{ID: c, Cap: 1024}
	}
	flat.Classes = [][]int{flat.Allowed}

	type want struct {
		cpus, mask []int
		workers    int
		plan, keep []int
		gmp        int
	}
	all12 := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	fast8 := []int{0, 1, 10, 11, 6, 7, 8, 9}
	for _, tc := range []struct {
		host string
		tp   topo
		name string
		w    want
	}{
		{"msr1", msr1, "allperm", want{cpus: all12, plan: []int{11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0}}},
		{"msr1", msr1, "fast8s1", want{cpus: []int{0, 1, 2, 6, 7, 8, 9, 10, 11}, mask: []int{0, 1, 2, 6, 7, 8, 9, 10, 11}, workers: 9, plan: []int{0, 1, 2, 6, 7, 8, 9, 10, 11}}},
		{"msr1", msr1, "fast8s2", want{cpus: []int{0, 1, 2, 3, 6, 7, 8, 9, 10, 11}, mask: []int{0, 1, 2, 3, 6, 7, 8, 9, 10, 11}, workers: 10, plan: []int{0, 1, 2, 3, 6, 7, 8, 9, 10, 11}}},
		{"msr1", msr1, "fast6s4", want{cpus: []int{0, 1, 2, 3, 4, 5, 6, 7, 10, 11}, mask: []int{0, 1, 2, 3, 4, 5, 6, 7, 10, 11}, workers: 10, plan: []int{0, 1, 2, 3, 4, 5, 6, 7, 10, 11}}},
		{"msr1", msr1, "fast4s4", want{cpus: []int{0, 1, 2, 3, 4, 5, 10, 11}, mask: []int{0, 1, 2, 3, 4, 5, 10, 11}, workers: 8, plan: []int{0, 1, 2, 3, 4, 5, 10, 11}}},
		{"msr1", msr1, "slow4wide", want{cpus: []int{2, 3, 4, 5}, mask: all12, workers: 4, plan: []int{2, 3, 4, 5}}},
		{"msr1", msr1, "fast8wide", want{cpus: fast8, mask: all12, workers: 8, plan: fast8}},
		{"msr1", msr1, "fast8u1", want{cpus: fast8, mask: all12, workers: 9, plan: append(append([]int(nil), fast8...), -1)}},
		{"msr1", msr1, "pin720", want{cpus: all12, keep: fast8}},
		{"msr1", msr1, "pin520", want{cpus: all12, keep: []int{2, 3, 4, 5}}},
		{"msr1", msr1, "gmp8", want{cpus: all12, gmp: 8}},
		// a host with one core type: fast = highest ids, slow = lowest ids
		{"flat16", flat, "fast8s1", want{cpus: []int{0, 8, 9, 10, 11, 12, 13, 14, 15}, mask: []int{0, 8, 9, 10, 11, 12, 13, 14, 15}, workers: 9, plan: []int{0, 8, 9, 10, 11, 12, 13, 14, 15}}},
		{"flat16", flat, "slow4wide", want{cpus: []int{0, 1, 2, 3}, mask: flat.Allowed, workers: 4, plan: []int{0, 1, 2, 3}}},
		{"flat16", flat, "pin520", want{cpus: flat.Allowed, keep: []int{0, 1, 2, 3}}},
	} {
		sp, ok, err := tc.tp.extraCase(tc.name)
		if err != nil || !ok {
			t.Errorf("%s/%s: ok=%v err=%v", tc.host, tc.name, ok, err)
			continue
		}
		got := want{cpus: sp.CPUs, mask: sp.Mask, workers: sp.Workers, plan: sp.Plan, keep: sp.Keep, gmp: sp.GOMAXPROCS}
		if !reflect.DeepEqual(got, tc.w) {
			t.Errorf("%s/%s:\n got %s\nwant %s", tc.host, tc.name, fmt.Sprintf("%+v", got), fmt.Sprintf("%+v", tc.w))
		}
	}
	if _, ok, _ := msr1.extraCase("all"); ok {
		t.Errorf("all is not an extra case")
	}
	// the loop plans as the child applies them
	ls := &loopSet{mode: caseMode{HasPlan: true, Plan: []int{3, -1, 5}}, full: []int{0, 1, 2, 3}}
	for k, w := range [][]int{{3}, {0, 1, 2, 3}, {5}, {0, 1, 2, 3}} {
		if got := ls.wanted(k, k); !reflect.DeepEqual(got, w) {
			t.Errorf("plan: loop %d wanted %v, got %v", k, w, got)
		}
	}
	ls = &loopSet{mode: caseMode{HasKeep: true, Keep: []int{2, 3}}, full: []int{0, 1, 2, 3}}
	for from, w := range [][]int{{0, 1, 2, 3}, {0, 1, 2, 3}, {2}, {3}} {
		if got := ls.wanted(from, from); !reflect.DeepEqual(got, w) {
			t.Errorf("keep: loop from %d wanted %v, got %v", from, w, got)
		}
	}
}
