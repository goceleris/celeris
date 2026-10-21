//go:build linux

package iouring

import (
	"bytes"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/platform/pintest"
)

// celeris#1036, NIC interrupt report: the loop count is the length of the plan
// the planner made, whatever the NIC's interrupts do. The planner and the
// platform's interrupt finder are stood in for (the test host's CPUs are alike,
// and it has no big.LITTLE NIC). The finder reports a vector serviced by a
// little CPU of the allowed set that runs no loop; the engine must still start
// exactly the planner's loops, each pinned to its planned CPU, hand the finder
// the concrete listen address and the plan the loops were started from, and
// log the Warn and the serviced CPUs, with no extra loop.
func TestNICIRQReportAddsNoLoop1036(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 2*time.Minute)
		return
	}
	cpus, err := pintest.AllowedCPUs()
	if err != nil || len(cpus) < 1 {
		t.Fatalf("premise: the process has no allowed CPU: %v (%v)", cpus, err)
	}
	base, _ := pintest.StartupMask()
	const little = 1000 // a CPU of the stand-in plan's allowed set that no loop uses
	for _, tc := range []struct {
		name    string
		workers int
	}{
		{"default", 0},
		{"user-set 4", 4},
	} {
		func() {
			oldPlan, oldNote := planWorkerCPUs, annotateNICIRQs
			defer func() { planWorkerCPUs, annotateNICIRQs = oldPlan, oldNote }()
			const planned = 2
			var planCPUs []int
			planWorkerCPUs = func(n int, explicit bool, addr string) platform.CPUPlan {
				planCPUs = make([]int, planned)
				for i := range planCPUs {
					planCPUs[i] = cpus[i%len(cpus)]
				}
				return platform.CPUPlan{CPUs: append([]int(nil), planCPUs...), Allowed: append(slices.Clone(cpus), little),
					Little: []int{little}, Requested: n, Heterogeneous: true, Note: "test plan"}
			}
			var gotAddr string
			var gotPlan []int
			var calls int
			annotateNICIRQs = func(p platform.CPUPlan, addr string) platform.CPUPlan {
				calls++
				gotAddr, gotPlan = addr, slices.Clone(p.CPUs)
				return p.WithNICIRQs(platform.NICIRQs{Vectors: []platform.NICVector{
					{IRQ: 167, CPU: little, Name: "enp1s0-8", Dev: "enp1s0", Count: 9000},
				}})
			}
			var log lockedBuf
			eng, stop := start909(t, tc.workers, &log)
			defer stop()
			// RLIMIT_MEMLOCK may hold the plan to fewer workers; the
			// planner's plan is cut to that and nothing else is added.
			started := planned
			if m := MaxWorkersForMemlock(); m > 0 && m < started {
				started = m
			}
			if calls != 1 {
				t.Errorf("%s: the interrupt finder ran %d times, want 1", tc.name, calls)
			}
			if !strings.HasPrefix(gotAddr, "127.0.0.1:") || strings.HasSuffix(gotAddr, ":0") {
				t.Errorf("%s: the interrupt finder got address %q, want the concrete bound one", tc.name, gotAddr)
			}
			if !slices.Equal(gotPlan, planCPUs[:started]) {
				t.Errorf("%s: the interrupt finder got the plan %v, the planner made %v", tc.name, gotPlan, planCPUs)
			}
			if got := len(eng.workers); got != started {
				t.Errorf("%s: %d loops started, the planner's plan holds %d", tc.name, got, started)
			}
			if got := eng.NumWorkers(); got != started {
				t.Errorf("%s: NumWorkers() = %d, want %d", tc.name, got, started)
			}
			if got := eng.Metrics().Workers; got != started {
				t.Errorf("%s: Metrics().Workers = %d, want %d", tc.name, got, started)
			}
			for i, c := range planCPUs[:started] {
				if got := eng.workers[i].CPUID(); got != c {
					t.Errorf("%s: loop %d: CPUID() = %d, want %d", tc.name, i, got, c)
				}
			}
			out := log.String()
			if !strings.Contains(out, fmt.Sprintf(" workers=%d", started)) {
				t.Errorf("%s: the listening line does not say workers=%d:\n%s", tc.name, started, out)
			}
			for _, w := range []string{"nic_irq_cpus=1000", "NIC interrupt vectors are serviced by CPUs that run no loop", "enp1s0-8 (IRQ 167)"} {
				if !strings.Contains(out, w) {
					t.Errorf("%s: the log lacks %q:\n%s", tc.name, w, out)
				}
			}
			if strings.Contains(out, "irq_loops") {
				t.Errorf("%s: the log still counts NIC interrupt loops:\n%s", tc.name, out)
			}
			running, err := pintest.Census()
			if err != nil {
				t.Fatal(err)
			}
			single := 0
			for _, th := range pintest.Off(running, base) {
				if th.Single() {
					single++
				}
			}
			if single != started {
				t.Errorf("%s: %d thread(s) pinned to one CPU, want %d (one per loop): %v",
					tc.name, single, started, pintest.Off(running, base))
			}
		}()
	}
}

// The report is made on the plan RLIMIT_MEMLOCK left, so a serviced big CPU
// whose loop the cut dropped is named in the Warn, and the cut itself is
// #1049's: the interrupts add no loop and change no cut (celeris#1036). The
// planner and the interrupt finder are stood in for.
func TestMemlockCutIsTheReportsPlan1036(t *testing.T) {
	big := []int{0, 1, 6, 7, 8, 9, 10, 11}
	oldPlan, oldCeiling := planWorkerCPUs, memlockCeiling
	t.Cleanup(func() { planWorkerCPUs, memlockCeiling = oldPlan, oldCeiling })
	planWorkerCPUs = func(n int, explicit bool, addr string) platform.CPUPlan {
		return platform.CPUPlan{CPUs: append([]int(nil), big...), Allowed: []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
			Little: []int{2, 3, 4, 5}, Requested: n, Heterogeneous: true, Note: "test plan: 8 loops"}
	}
	vectors := platform.NICIRQs{Vectors: []platform.NICVector{
		{IRQ: 167, CPU: 4, Name: "enp1s0-8", Dev: "enp1s0", Count: 9000},
		{IRQ: 135, CPU: 8, Name: "enp1s0-6", Dev: "enp1s0", Count: 9000},
	}}
	for _, tc := range []struct {
		ceiling   int
		want      []int // the loops: the planner's, cut by the ceiling
		uncovered []int
	}{
		{-1, big, []int{4}},
		{9, big, []int{4}},
		{8, big, []int{4}},
		{3, big[:3], []int{4, 8}},
	} {
		memlockCeiling = func() int { return tc.ceiling }
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		plan := planWithinMemlock(12, false, "", log)
		if !slices.Equal(plan.CPUs, tc.want) {
			t.Errorf("ceiling %d: CPUs %v, want %v", tc.ceiling, plan.CPUs, tc.want)
		}
		note := plan.WithNICIRQs(vectors)
		if !slices.Equal(note.CPUs, plan.CPUs) || note.Note != plan.Note || note.Requested != plan.Requested {
			t.Errorf("ceiling %d: the report changed the plan: %+v, was %+v", tc.ceiling, note, plan)
		}
		if !slices.Equal(note.IRQUncovered, tc.uncovered) {
			t.Errorf("ceiling %d: uncovered %v, want %v", tc.ceiling, note.IRQUncovered, tc.uncovered)
		}
		buf.Reset()
		platform.LogPinOutcome(log, "io_uring", note, nil)
		out := buf.String()
		for _, c := range tc.uncovered {
			if !strings.Contains(out, fmt.Sprintf("CPU %d: ", c)) {
				t.Errorf("ceiling %d: the Warn does not name CPU %d:\n%s", tc.ceiling, c, out)
			}
		}
	}
}
