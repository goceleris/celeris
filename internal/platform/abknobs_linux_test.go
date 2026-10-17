//go:build linux

package platform

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// CELERIS_AB_EXTRA_LOOP_CPU=4 puts a loop on the little CPU 4 besides the 8 big ones (9 loops on msr1).
func TestABExtraLoopCPU(t *testing.T) {
	h := msr1(t, rng(0, 11))
	t.Setenv("CELERIS_AB_EXTRA_LOOP_CPU", "4")
	p := planWorkerCPUs(12, false, h.src())
	eq(t, "cpus", p.CPUs, []int{0, 1, 6, 7, 8, 9, 10, 11, 4})
	t.Setenv("CELERIS_AB_EXTRA_LOOP_CPU", "99") // not an allowed CPU: ignored
	eq(t, "cpus not allowed", planWorkerCPUs(12, false, h.src()).CPUs, []int{0, 1, 6, 7, 8, 9, 10, 11})
	t.Setenv("CELERIS_AB_EXTRA_LOOP_CPU", "")
	eq(t, "cpus unset", planWorkerCPUs(12, false, h.src()).CPUs, []int{0, 1, 6, 7, 8, 9, 10, 11})
}

// A spinner is a thread on its CPU with the SCHED_IDLE policy (field 41 of /proc/self/task/<tid>/stat is 5),
// and it ends with its context.
func TestABSpinner(t *testing.T) {
	t.Setenv("CELERIS_AB_SPIN_CPUS", "0")
	ctx, cancel := context.WithCancel(context.Background())
	ABStartSpinners(ctx)
	deadline := time.Now().Add(3 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		ents, _ := os.ReadDir("/proc/self/task")
		for _, e := range ents {
			b, err := os.ReadFile("/proc/self/task/" + e.Name() + "/stat")
			if err != nil {
				continue
			}
			s := string(b)
			f := strings.Fields(s[strings.LastIndex(s, ")")+2:]) // fields after "(comm)": state is f[0] = field 3
			if len(f) < 39 {
				continue
			}
			policy, _ := strconv.Atoi(f[38]) // field 41
			psr, _ := strconv.Atoi(f[36])    // field 39
			if policy == 5 && psr == 0 {
				found = true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if !found {
		t.Skip("no SCHED_IDLE thread seen on CPU 0 (the kernel refused SCHED_IDLE, or CPU 0 is not allowed)")
	}
}
