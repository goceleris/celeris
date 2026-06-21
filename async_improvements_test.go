package celeris

import (
	"sync/atomic"
	"testing"
)

// --- #1: Server.AsyncHandlers() reports the EFFECTIVE async state ---------

// TestAsyncHandlersEffective_PerRouteAsync verifies the driver-footgun fix: a
// server with Config.AsyncHandlers=false but a .Async() route reports
// AsyncHandlers()==true, so a WithEngine driver picks its netpoll fast path
// under the recommended per-route-async idiom (instead of the slow mini-loop).
func TestAsyncHandlersEffective_PerRouteAsync(t *testing.T) {
	s := New(Config{Engine: Std, AsyncHandlers: false})
	if s.AsyncHandlers() {
		t.Fatal("no async routes + AsyncHandlers=false must report false")
	}
	s.GET("/db", noopHandler).Async()
	if !s.AsyncHandlers() {
		t.Fatal("a .Async() route must make AsyncHandlers() effective-true (driver fast path)")
	}
}

// TestAsyncHandlersEffective_ServerDefault verifies the server-level flag alone
// still reports true (unchanged behavior).
func TestAsyncHandlersEffective_ServerDefault(t *testing.T) {
	s := New(Config{Engine: Std, AsyncHandlers: true})
	if !s.AsyncHandlers() {
		t.Fatal("AsyncHandlers=true must report true")
	}
}

// TestAsyncHandlersEffective_GroupAsync verifies a group-level .Async() route
// also flips the effective state.
func TestAsyncHandlersEffective_GroupAsync(t *testing.T) {
	s := New(Config{Engine: Std, AsyncHandlers: false})
	api := s.Group("/api").Async()
	api.GET("/x", noopHandler)
	if !s.AsyncHandlers() {
		t.Fatal("a group .Async() route must make AsyncHandlers() effective-true")
	}
}

// --- #4: .UsesDriver() is an intent-revealing alias for .Async() ----------

func TestUsesDriver(t *testing.T) {
	s := New(Config{Engine: Std, AsyncHandlers: false})
	s.GET("/cache/:key", noopHandler).UsesDriver()
	if !s.router.routeAsync("GET", "/cache/anything") {
		t.Fatal(".UsesDriver() must resolve hard-async")
	}
	if s.router.adaptiveRoutes["/cache/:key"] {
		t.Fatal(".UsesDriver() (== .Async()) must not be adaptive")
	}
	if !s.AsyncHandlers() {
		t.Fatal(".UsesDriver() must make AsyncHandlers() effective-true")
	}
}

// --- #3: immediate promotion of an unambiguously-blocking inline run -------

// TestAdaptiveBlockingThresholdConst guards the threshold ordering: the
// immediate-promote bar must sit above the slow bar so the streak path still
// owns the borderline band.
func TestAdaptiveBlockingThresholdConst(t *testing.T) {
	if adaptiveBlockingThreshold <= adaptivePromoteThreshold {
		t.Fatalf("blocking threshold %v must exceed slow threshold %v",
			adaptiveBlockingThreshold, adaptivePromoteThreshold)
	}
}

// TestPromoteRouteImmediate verifies a single unambiguously-blocking inline run
// promotes the route immediately (no adaptivePromoteStreak wait), so a
// forgotten-async blocking handler stalls a worker for at most one request.
func TestPromoteRouteImmediate(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	s.GET("/slow", noopHandler) // inherits default → adaptive
	r := s.router
	if !r.adaptiveRoutes["/slow"] {
		t.Fatal("/slow should be adaptive under AsyncHandlers=true")
	}
	if r.isPromoted("/slow") {
		t.Fatal("must not be promoted before any blocking run")
	}
	r.promoteRouteImmediate("/slow") // one >adaptiveBlockingThreshold run
	if !r.isPromoted("/slow") {
		t.Fatal("a single blocking inline run must promote immediately")
	}
	if !r.routeAsync("GET", "/slow") {
		t.Fatal("a promoted route must resolve async")
	}
}

// TestPromoteImmediate_ClearsFastStreak verifies the immediate promotion clears
// any accumulated fast streak so the route cannot wrongly settle on stale fast
// runs after the promotion TTL expires.
func TestPromoteImmediate_ClearsFastStreak(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	s.GET("/slow", noopHandler)
	r := s.router
	for i := 0; i < 10; i++ {
		r.recordInlineRun("/slow", false) // accumulate fast runs
	}
	r.promoteRouteImmediate("/slow")
	if !r.isPromoted("/slow") {
		t.Fatal("must be promoted")
	}
	if v, ok := r.fastStreak.Load("/slow"); ok {
		if got := v.(*atomic.Int32).Load(); got != 0 {
			t.Fatalf("fast streak must be cleared on immediate promote, got %d", got)
		}
	}
}

// TestStreakStillRequiresEight verifies the slow-streak path is unchanged: a
// borderline-slow (300µs–2ms) route still needs adaptivePromoteStreak runs.
func TestStreakStillRequiresEight(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	s.GET("/borderline", noopHandler)
	r := s.router
	for i := 0; i < adaptivePromoteStreak-1; i++ {
		r.recordInlineRun("/borderline", true)
	}
	if r.isPromoted("/borderline") {
		t.Fatalf("must not promote before %d consecutive slow runs", adaptivePromoteStreak)
	}
	r.recordInlineRun("/borderline", true) // the 8th
	if !r.isPromoted("/borderline") {
		t.Fatalf("must promote on the %dth consecutive slow run", adaptivePromoteStreak)
	}
}
