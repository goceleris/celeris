//go:build linux

package epoll

import (
	"fmt"
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
			if calls != 1 {
				t.Errorf("%s: the interrupt finder ran %d times, want 1", tc.name, calls)
			}
			if !strings.HasPrefix(gotAddr, "127.0.0.1:") || strings.HasSuffix(gotAddr, ":0") {
				t.Errorf("%s: the interrupt finder got address %q, want the concrete bound one", tc.name, gotAddr)
			}
			if !slices.Equal(gotPlan, planCPUs) {
				t.Errorf("%s: the interrupt finder got the plan %v, the planner made %v", tc.name, gotPlan, planCPUs)
			}
			if got := len(eng.loops); got != planned {
				t.Errorf("%s: %d loops started, the planner's plan holds %d", tc.name, got, planned)
			}
			if got := eng.NumWorkers(); got != planned {
				t.Errorf("%s: NumWorkers() = %d, want %d", tc.name, got, planned)
			}
			if got := eng.Metrics().Workers; got != planned {
				t.Errorf("%s: Metrics().Workers = %d, want %d", tc.name, got, planned)
			}
			for i, c := range planCPUs {
				if got := eng.loops[i].CPUID(); got != c {
					t.Errorf("%s: loop %d: CPUID() = %d, want %d", tc.name, i, got, c)
				}
			}
			out := log.String()
			if !strings.Contains(out, fmt.Sprintf(" loops=%d", planned)) {
				t.Errorf("%s: the listening line does not say loops=%d:\n%s", tc.name, planned, out)
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
			if single != planned {
				t.Errorf("%s: %d thread(s) pinned to one CPU, want %d (one per loop): %v",
					tc.name, single, planned, pintest.Off(running, base))
			}
		}()
	}
}
