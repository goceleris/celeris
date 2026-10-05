//go:build linux

package celeris

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestAdaptiveImmediatePromote_Epoll verifies improvement #3 end-to-end on the
// real epoll engine (which runs HandleStream's inline timing): under
// AsyncHandlers=true, an UNMARKED route whose handler blocks for longer than
// adaptiveBlockingThreshold is promoted to async dispatch on the FIRST request,
// not after adaptivePromoteStreak (8) of them. A fast route must NOT promote
// on its fast runs.
//
// celeris#752: the fast-route bar used to be absolute. Any /ping run that the
// runner stretched past adaptiveBlockingThreshold (2 ms; one descheduling under
// -race plus coverage on a 4-vCPU hosted runner) promoted /ping, and the test
// failed with the router doing exactly what it should. The fast arm now times
// every /ping run inside the handler and judges each promotion against those
// runs: a promotion that follows a run measured over the bar is the runner's
// doing (counted, the route re-armed, not a failure); a promotion with no
// slow run behind it is the router promoting a fast route, and more than
// ping752MaxUnexplained of those in ping752Runs runs fails.
func TestAdaptiveImmediatePromote_Epoll(t *testing.T) {
	s := New(Config{Engine: Epoll, AsyncHandlers: true})
	var pingRun atomic.Int64    // 1-based index of the /ping run in flight
	var pingLastNs atomic.Int64 // in-handler duration of the latest /ping run
	forceSlowEvery := ping752ForceSlowEvery()
	s.GET("/ping", func(c *Context) error {
		start := time.Now()
		if n := pingRun.Add(1); forceSlowEvery > 0 && n%int64(forceSlowEvery) == 0 {
			time.Sleep(3 * time.Millisecond) // CELERIS_752_FORCE_SLOW_EVERY: a run the runner stretched
		}
		err := c.String(http.StatusOK, "ok")
		pingLastNs.Store(int64(time.Since(start)))
		return err
	})
	s.GET("/slow", func(c *Context) error {
		time.Sleep(3 * time.Millisecond) // > adaptiveBlockingThreshold (2ms)
		return c.String(http.StatusOK, "slow")
	})
	if !s.router.adaptiveRoutes["/slow"] || !s.router.adaptiveRoutes["/ping"] {
		t.Fatal("both routes must be adaptive under AsyncHandlers=true")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.StartWithListener(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	// Readiness by a TCP dial of the engine's own address, which touches no
	// route (so /slow is untouched until our 1 probe), and which fails at
	// once with Start's error if Start returns first (celeris#706).
	base := "http://" + waitServerStarted(t, s, done, 5*time.Second)
	client := &http.Client{Timeout: 10 * time.Second}

	if s.router.isPromoted("/slow") {
		t.Fatal("/slow must not be promoted before any request to it")
	}
	// Exactly ONE request to the blocking route.
	resp, err := client.Get(base + "/slow")
	if err != nil {
		t.Fatalf("GET /slow: %v", err)
	}
	_ = resp.Body.Close()

	if !s.router.isPromoted("/slow") {
		t.Fatal("#3: a single >2ms inline run must promote /slow immediately (got not-promoted)")
	}
	// Cross-route: the /slow promotion must not promote /ping, which has not
	// run once yet, so no timing can explain it.
	if s.router.isPromoted("/ping") {
		t.Fatal("/ping (fast) must not be promoted by a /slow run: it has not run yet")
	}

	// The fast arm. After each /ping response the route's promotion state is
	// read; a promotion is EXPLAINED when a run since the last re-arm was
	// measured, inside the handler, over ping752BlockingBar, or the
	// last ping752Streak runs were all over ping752StreakBar
	// (the streak path). The handler's span is inside the router's, so an
	// explained promotion is one the router was right to make. Either way the
	// route is re-armed (promotion cleared) so the next run is timed again.
	var (
		maxSinceArm      time.Duration
		slowStreak       int
		explained        int
		unexplained      int
		slowRuns         int
		maxRun           time.Duration
		unexplainedFirst = -1
	)
	for i := 1; i <= ping752Runs; i++ {
		resp, err := client.Get(base + "/ping")
		if err != nil {
			t.Fatalf("GET /ping run %d: %v", i, err)
		}
		_ = resp.Body.Close()
		d := time.Duration(pingLastNs.Load())
		maxRun = max(maxRun, d)
		maxSinceArm = max(maxSinceArm, d)
		if d > ping752BlockingBar {
			slowRuns++
		}
		if d > ping752StreakBar {
			slowStreak++
		} else {
			slowStreak = 0
		}
		if !s.router.isPromoted("/ping") {
			continue
		}
		if maxSinceArm > ping752BlockingBar || slowStreak >= ping752Streak {
			explained++
		} else {
			unexplained++
			if unexplainedFirst < 0 {
				unexplainedFirst = i
			}
		}
		s.router.promoted.Delete("/ping")
		if v, ok := s.router.slowStreak.Load("/ping"); ok {
			v.(*atomic.Int32).Store(0)
		}
		maxSinceArm, slowStreak = 0, 0
	}
	t.Logf("RESULT752 runs=%d slow_runs=%d max_run=%s promotions_explained=%d promotions_unexplained=%d first_unexplained_run=%d bar_unexplained<=%d force_slow_every=%d",
		ping752Runs, slowRuns, maxRun, explained, unexplained, unexplainedFirst, ping752MaxUnexplained, forceSlowEvery)
	if unexplained > ping752MaxUnexplained {
		t.Fatalf("/ping (fast) was promoted %d times in %d runs with no run measured over %s (nor %d consecutive over %s) behind the promotion, first after run %d: the router is promoting a fast route",
			unexplained, ping752Runs, ping752BlockingBar, ping752Streak, ping752StreakBar, unexplainedFirst)
	}
}

// The bars the fast arm judges a promotion against are the router's
// documented contract (handler.go: adaptiveBlockingThreshold = 2 ms,
// adaptivePromoteThreshold = 300 us, adaptivePromoteStreak = 8), written out
// here rather than read from those constants: a router whose bar drifted
// (to 0, say) would otherwise explain its own wrong promotions. A deliberate
// change of a bar in handler.go changes these with it, as it does the 3 ms
// /slow sleep above.
const (
	ping752BlockingBar = 2 * time.Millisecond
	ping752StreakBar   = 300 * time.Microsecond
	ping752Streak      = 8
)

// ping752Runs is how many /ping runs the fast arm judges. A router that
// promotes fast runs promotes on (nearly) every one of them once re-armed,
// or once per ping752Streak runs on the streak path (8 of 64).
const ping752Runs = 64

// ping752MaxUnexplained is the number of promotions with no measured slow run
// behind them that the fast arm tolerates. The handler's span is inside the
// router's, so a descheduling in the few instructions between the router's
// clock read and the handler's can stretch the router's span alone; that
// needs a > 2 ms stall in a sub-microsecond window, and two of them in one
// test is far below the rates the defects above produce (64 and 8).
const ping752MaxUnexplained = 2

// ping752ForceSlowEvery reads CELERIS_752_FORCE_SLOW_EVERY: a diagnostic
// override that makes every k-th /ping run sleep 3 ms, i.e. the runner
// stretching a run past the bar, to show the arm does not fail on it.
func ping752ForceSlowEvery() int {
	k, _ := strconv.Atoi(os.Getenv("CELERIS_752_FORCE_SLOW_EVERY"))
	return k
}
