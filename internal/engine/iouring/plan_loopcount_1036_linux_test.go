//go:build linux

package iouring

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/platform/pintest"
)

// celeris#1036: the plan sets the loop count. On a big.LITTLE host the planner
// returns fewer entries than the workers asked for (one per non-little CPU);
// the engine must start len(plan.CPUs) loops, not the number it asked for. The
// test host's CPUs are alike (the planner would return every worker), so the
// planner is stood in for by one that returns fewer entries.
//
// The test also holds on a runner whose RLIMIT_MEMLOCK funds one worker: the
// planner is asked for the whole request there, so a request of 2 is shortened
// to a plan of 1 and the engine's count comes from the plan.
//
// Two requests: a user-set count (Config.Workers: the engine tells the planner
// the count is explicit) and the default (it does not).
func TestLoopCountIsThePlanLength1036(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 2*time.Minute)
		return
	}
	cpus, err := pintest.AllowedCPUs()
	if err != nil || len(cpus) < 1 {
		t.Fatalf("premise: the process has no allowed CPU: %v (%v)", cpus, err)
	}
	base, _ := pintest.StartupMask()
	for _, tc := range []struct {
		name         string
		workers      int // Resources.Workers; 0 = the default
		planLen      int
		wantExplicit bool
		rxFree       bool // the plan carries what the rx-port rule decided
	}{
		{"user-set 6, plan holds 3", 6, 3, true, false},
		{"default, plan holds 2", 0, 2, false, false},
		// The rx-port rule (celeris#1036 item 1) shortens the default plan by
		// the CPUs it frees: the engine starts the plan's loops, whatever the
		// reason it is short, and says what the rule did on its Info line.
		{"default, the rx-port rule freed two CPUs", 0, 2, false, true},
	} {
		func() {
			old := planWorkerCPUs
			defer func() { planWorkerCPUs = old }()
			var gotN int
			var gotExplicit bool
			var gotAddr string
			var stub []int
			// The planner is asked first and RLIMIT_MEMLOCK bounds what it
			// returns (celeris#1036), so it is asked for the whole request even
			// where memlock funds one worker (the CI runners' 8 MiB). The stub
			// shortens whatever it is asked for, so the shortening is exercised
			// there too: a request of 2 gets a plan of 1.
			planWorkerCPUs = func(n int, explicit bool, addr string) platform.CPUPlan {
				gotN, gotExplicit, gotAddr = n, explicit, addr
				stub = make([]int, max(min(tc.planLen, n-1), 1))
				for i := range stub {
					stub[i] = cpus[i%len(cpus)]
				}
				p := platform.CPUPlan{CPUs: append([]int(nil), stub...), Allowed: cpus, Requested: n,
					Heterogeneous: true, Note: "test plan: fewer loops than asked for"}
				if tc.rxFree {
					p.RxFree = platform.RxFree{Port: "enp49s0", SharePercent: 73, Freed: []int{6, 8}}
				}
				return p
			}
			var log lockedBuf
			eng, stop := start909(t, tc.workers, &log)
			defer stop()
			// What the engine must run, from the test's own reading of the
			// limit: the plan, held to what memlock funds.
			planLen := len(stub)
			if m := MaxWorkersForMemlock(); m > 0 {
				planLen = min(planLen, m)
			}
			want := stub[:planLen]
			wantAsked := tc.workers
			if wantAsked == 0 {
				wantAsked = max(runtime.GOMAXPROCS(0), 2)
			}
			// The planner is given the concrete address the engine binds.
			if gotAddr != eng.Addr().String() {
				t.Errorf("%s: the planner was given the address %q, the engine listens on %q", tc.name, gotAddr, eng.Addr())
			}
			if tc.rxFree {
				for _, w := range []string{"freed_cpus=6,8", "rx_port=enp49s0", "rx_share_pct=73"} {
					if !strings.Contains(log.String(), w) {
						t.Errorf("%s: the plan's Info line lacks %q:\n%s", tc.name, w, log.String())
					}
				}
			}
			if gotExplicit != tc.wantExplicit {
				t.Errorf("%s: the engine told the planner explicit=%v, want %v", tc.name, gotExplicit, tc.wantExplicit)
			}
			if gotN != wantAsked {
				t.Errorf("%s: the planner was asked for %d workers, want the whole request, %d (memlock bounds the plan, not the request)",
					tc.name, gotN, wantAsked)
			}
			if len(stub) >= gotN {
				t.Errorf("%s: premise: the stub plan holds %d for a request of %d, nothing was shortened", tc.name, len(stub), gotN)
			}
			if got := len(eng.workers); got != planLen {
				t.Errorf("%s: %d loops started, the plan holds %d", tc.name, got, planLen)
			}
			if got := eng.NumWorkers(); got != planLen {
				t.Errorf("%s: NumWorkers() = %d, want %d", tc.name, got, planLen)
			}
			if got := eng.Metrics().Workers; got != planLen {
				t.Errorf("%s: Metrics().Workers = %d, want %d", tc.name, got, planLen)
			}
			for i, c := range want {
				if got := eng.workers[i].CPUID(); got != c {
					t.Errorf("%s: loop %d: CPUID() = %d, want %d", tc.name, i, got, c)
				}
			}
			// The "listening" line, not the plan's own line, which reads the plan.
			listening := listeningLine1036(t, &log)
			if !strings.Contains(listening, fmt.Sprintf(" workers=%d", planLen)) {
				t.Errorf("%s: the listening line does not say workers=%d:\n%s", tc.name, planLen, log.String())
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
			if single != planLen {
				t.Errorf("%s: %d thread(s) pinned to one CPU, want %d (one per loop): %v",
					tc.name, single, planLen, pintest.Off(running, base))
			}
		}()
	}
}

// listeningLine1036 returns the engine's "engine listening" line, waiting up to
// 10 s for it. Listen publishes the address (what start909 waits for) and logs
// that line after it, so a caller that read the log the moment Addr() turned
// non-nil could see a log without the line (celeris#1036: a CI run on a loaded
// runner did). The line is whole once it is there: one Write per record.
func listeningLine1036(t *testing.T, log *lockedBuf) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		for _, ln := range strings.Split(log.String(), "\n") {
			if strings.Contains(ln, "engine listening") {
				return ln
			}
		}
		if time.Now().After(deadline) {
			t.Errorf("no \"engine listening\" line within 10 s of the address being published:\n%s", log.String())
			return ""
		}
	}
}
