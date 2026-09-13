package celeris

import (
	"runtime"
	"testing"
	"time"
)

// TestRouteAdaptive_SettledRouteIsReopened covers the celeris#592 mechanism at
// the router level: settling is no longer terminal. A settled route returned to
// the timed path by the re-opener promotes on its first blocking run, and a
// route that is still fast re-settles on its very next run — which is what
// bounds the re-timing cost to one timed inline run per route per
// adaptiveSettleTTL.
func TestRouteAdaptive_SettledRouteIsReopened(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	s.GET("/s", noopHandler)
	rt := s.router

	for i := 0; i < adaptiveSettleStreak; i++ {
		rt.recordInlineRun("/s", false)
	}
	if rt.adaptiveLearning("/s") {
		t.Fatalf("route must settle after %d consecutive fast runs", adaptiveSettleStreak)
	}

	// Re-open: the route is timed again (this is the run that catches a
	// backend that has since turned slow).
	rt.reopenSettled()
	if !rt.adaptiveLearning("/s") {
		t.Fatal("reopenSettled must return a settled route to the timed path")
	}

	// Still fast → re-settles on the single re-timed run, because the fast
	// streak is deliberately preserved across the re-open.
	rt.recordInlineRun("/s", false)
	if rt.adaptiveLearning("/s") {
		t.Fatal("a still-fast route must re-settle on its first re-timed run (fast streak preserved)")
	}

	// Backend turned slow: the re-timed run is over adaptiveBlockingThreshold,
	// so handler.go calls promoteRouteImmediate and the route goes async.
	rt.reopenSettled()
	if !rt.adaptiveLearning("/s") {
		t.Fatal("reopenSettled must return the re-settled route to the timed path")
	}
	rt.promoteRouteImmediate("/s")
	if !rt.isPromoted("/s") {
		t.Fatal("a settled route that turns blocking must promote once re-timed")
	}
	if !rt.routeAsync("GET", "/s") {
		t.Fatal("a promoted route must dispatch async")
	}
}

// TestRouteAdaptive_SettleReopenerLifecycle verifies that the re-opener
// goroutine is started only when there are adaptive routes, actually clears the
// settled set on its tick, and is stopped (no goroutine left behind) by
// stopSettleReopener. Start/stop are both idempotent.
func TestRouteAdaptive_SettleReopenerLifecycle(t *testing.T) {
	// No adaptive routes (AsyncHandlers=false) → no goroutine at all.
	plain := New(Config{})
	plain.GET("/p", noopHandler)
	before := runtime.NumGoroutine()
	plain.router.startSettleReopener(time.Millisecond)
	if plain.router.reopenStop != nil {
		t.Fatal("a server with no adaptive routes must not start the re-opener")
	}
	plain.router.stopSettleReopener() // idempotent on a never-started router

	s := New(Config{AsyncHandlers: true})
	s.GET("/s", noopHandler)
	rt := s.router
	for i := 0; i < adaptiveSettleStreak; i++ {
		rt.recordInlineRun("/s", false)
	}
	if rt.adaptiveLearning("/s") {
		t.Fatal("precondition: route must be settled")
	}

	rt.startSettleReopener(time.Millisecond)
	rt.startSettleReopener(time.Millisecond) // idempotent: still one goroutine

	deadline := time.Now().Add(5 * time.Second)
	for !rt.adaptiveLearning("/s") {
		if time.Now().After(deadline) {
			t.Fatal("the re-opener did not clear the settled set within 5 s")
		}
		time.Sleep(time.Millisecond)
	}

	rt.stopSettleReopener()
	rt.stopSettleReopener() // idempotent
	if rt.reopenStop != nil {
		t.Fatal("stopSettleReopener must clear the stop channel")
	}
	// The goroutine exits on the stop channel; give it a moment and check we
	// are back to the baseline count.
	for i := 0; i < 200 && runtime.NumGoroutine() > before; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines after stop = %d, want <= %d (re-opener leaked)", after, before)
	}
}
