//go:build linux

package epoll

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
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
			var want []int
			// The default is GOMAXPROCS (two at least): on a small runner the
			// plan holds one fewer than it is asked for.
			planLen := tc.planLen
			planWorkerCPUs = func(n int, explicit bool, addr string) platform.CPUPlan {
				gotN, gotExplicit, gotAddr = n, explicit, addr
				planLen = max(min(tc.planLen, n-1), 1)
				want = make([]int, planLen)
				for i := range want {
					want[i] = cpus[i%len(cpus)]
				}
				p := platform.CPUPlan{CPUs: append([]int(nil), want...), Allowed: cpus, Requested: n,
					Heterogeneous: true, Note: "test plan: fewer loops than asked for"}
				if tc.rxFree {
					p.RxFree = platform.RxFree{Port: "enp49s0", SharePercent: 73, Freed: []int{6, 8}}
				}
				return p
			}
			var log lockedBuf
			eng, stop := start909(t, tc.workers, &log)
			defer stop()
			if gotExplicit != tc.wantExplicit {
				t.Errorf("%s: the engine told the planner explicit=%v, want %v", tc.name, gotExplicit, tc.wantExplicit)
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
			if tc.workers > 0 && gotN != tc.workers {
				t.Errorf("%s: the planner was asked for %d workers, want %d", tc.name, gotN, tc.workers)
			}
			if gotN < 2 {
				t.Fatalf("%s: the planner was asked for %d worker(s), the engine runs two at least", tc.name, gotN)
			}
			if got := len(eng.loops); got != planLen {
				t.Errorf("%s: %d loops started, the plan holds %d", tc.name, got, planLen)
			}
			if got := eng.NumWorkers(); got != planLen {
				t.Errorf("%s: NumWorkers() = %d, want %d", tc.name, got, planLen)
			}
			if got := eng.Metrics().Workers; got != planLen {
				t.Errorf("%s: Metrics().Workers = %d, want %d", tc.name, got, planLen)
			}
			for i, c := range want {
				if got := eng.loops[i].CPUID(); got != c {
					t.Errorf("%s: loop %d: CPUID() = %d, want %d", tc.name, i, got, c)
				}
			}
			// The "listening" line, not the plan's own line, which reads the plan.
			listening := listeningLine1036(t, &log)
			if !strings.Contains(listening, fmt.Sprintf(" loops=%d", planLen)) {
				t.Errorf("%s: the listening line does not say loops=%d:\n%s", tc.name, planLen, log.String())
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

// A default-sized epoll engine whose plan holds ONE CPU (a big.LITTLE cpuset
// with a single non-little CPU) runs one loop, below resource.MinWorkers. No
// default epoll engine ran on one loop before celeris#1036 (Resolve clamps to
// MinWorkers and Validate rejects a user value below it), so it serves
// keep-alive requests on several connections and stops cleanly here.
func TestEpollServesOnOneLoop1036(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 2*time.Minute)
		return
	}
	cpus, err := pintest.AllowedCPUs()
	if err != nil || len(cpus) < 1 {
		t.Fatalf("premise: the process has no allowed CPU: %v (%v)", cpus, err)
	}
	old := planWorkerCPUs
	defer func() { planWorkerCPUs = old }()
	planWorkerCPUs = func(n int, explicit bool, _ string) platform.CPUPlan {
		return platform.CPUPlan{CPUs: []int{cpus[len(cpus)-1]}, Allowed: cpus, Requested: n,
			Heterogeneous: true, Note: "test plan: one non-little CPU"}
	}
	var log lockedBuf
	eng, stop := start909(t, 0, &log)
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()
	if got := len(eng.loops); got != 1 {
		t.Fatalf("%d loops started, the plan holds 1", got)
	}
	const conns, reqs = 16, 200
	addr := eng.Addr().String()
	var wg sync.WaitGroup
	errs := make(chan error, conns)
	for c := range conns {
		wg.Go(func() {
			conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
			br := bufio.NewReader(conn)
			for i := range reqs {
				if _, err := fmt.Fprintf(conn, "GET /x HTTP/1.1\r\nHost: a\r\n\r\n"); err != nil {
					errs <- fmt.Errorf("conn %d request %d: %w", c, i, err)
					return
				}
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					errs <- fmt.Errorf("conn %d request %d: %w", c, i, err)
					return
				}
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != 200 || string(b) != "ok" {
					errs <- fmt.Errorf("conn %d request %d: status %d body %q", c, i, resp.StatusCode, b)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// The loop adds its request batch to the engine's count once per loop
	// iteration, after the write that answered the request: a client can read
	// the last response before that request is counted. Wait for the count
	// (2 s at most) instead of reading it once.
	m := eng.Metrics()
	for deadline := time.Now().Add(2 * time.Second); m.RequestCount < conns*reqs && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
		m = eng.Metrics()
	}
	if m.Workers != 1 || m.RequestCount < conns*reqs {
		t.Errorf("metrics: workers=%d requests=%d, want 1 and at least %d", m.Workers, m.RequestCount, conns*reqs)
	}
	stop()
	stopped = true
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
