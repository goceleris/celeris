package celeris

import (
	"testing"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

func noopHandler(_ *Context) error { return nil }

// TestRouteAsync_ServerDefaultSync verifies routes inherit a sync server
// default (Config.AsyncHandlers=false) when not overridden.
func TestRouteAsync_ServerDefaultSync(t *testing.T) {
	s := New(Config{AsyncHandlers: false})
	s.GET("/ping", noopHandler)
	if s.router.routeAsync("GET", "/ping") {
		t.Fatal("route should inherit sync server default")
	}
	if s.router.hasAsyncRoutes() {
		t.Fatal("hasAsyncRoutes should be false on a pure-sync server")
	}
}

// TestRouteAsync_ServerDefaultAdaptive verifies the celeris#356 contract: a
// route that inherits the AsyncHandlers=true default (not an explicit .Async())
// is ADAPTIVE — it starts INLINE (routeAsync=false, the ring-batched fast path)
// and is promoted to async only after a blocking inline run. hasAsyncRoutes
// stays true because adaptive routes may promote and need the async infra.
func TestRouteAsync_ServerDefaultAdaptive(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	s.GET("/ping", noopHandler)
	if s.router.routeAsync("GET", "/ping") {
		t.Fatal("adaptive route should start INLINE (not async) until it blocks")
	}
	if !s.router.adaptiveRoutes["/ping"] {
		t.Fatal("/ping should be registered adaptive under AsyncHandlers=true")
	}
	if !s.router.hasAsyncRoutes() {
		t.Fatal("hasAsyncRoutes should be true (adaptive routes may promote)")
	}
	// A blocking inline run promotes the route; it then resolves async.
	s.router.promoteRoute("/ping")
	if !s.router.routeAsync("GET", "/ping") {
		t.Fatal("after promotion the adaptive route should resolve async")
	}
}

// TestRouteAsync_RouteOverrideOn forces a single route async on an
// otherwise-sync server.
func TestRouteAsync_RouteOverrideOn(t *testing.T) {
	s := New(Config{AsyncHandlers: false})
	s.GET("/cpu", noopHandler)
	s.GET("/db", noopHandler).Async()
	if s.router.routeAsync("GET", "/cpu") {
		t.Fatal("/cpu should stay sync")
	}
	if !s.router.routeAsync("GET", "/db") {
		t.Fatal("/db should be async via Route.Async()")
	}
	if !s.router.hasAsyncRoutes() {
		t.Fatal("hasAsyncRoutes should be true once a route opts in")
	}
}

// TestRouteAsync_RouteOverrideOff verifies that on an async-default server, an
// inherited route is adaptive (inline-first, celeris#356) while an explicit
// .Async(false) route is hard-sync and never adaptive.
func TestRouteAsync_RouteOverrideOff(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	s.GET("/db", noopHandler)                  // inherits default → adaptive
	s.GET("/cached", noopHandler).Async(false) // explicit sync → never adaptive
	if s.router.routeAsync("GET", "/db") {
		t.Fatal("/db inherits the async default → adaptive, starts inline")
	}
	if !s.router.adaptiveRoutes["/db"] {
		t.Fatal("/db should be adaptive")
	}
	if s.router.routeAsync("GET", "/cached") {
		t.Fatal("/cached should be forced sync via Async(false)")
	}
	if s.router.adaptiveRoutes["/cached"] {
		t.Fatal("/cached is explicitly sync → must not be adaptive")
	}
}

// TestRouteAsync_ExplicitAsyncOptsOutOfAdaptive is the celeris#356
// no-regression guard for blocking handlers. A route explicitly marked
// .Async() on an async-default server (exactly how the probatorium driver
// routes — /cache, /db, /mc — register) must be hard-async, NOT adaptive:
// it must never enter the inline-first window, so a blocking handler never
// runs inline and stalls a worker. This is what makes #356 safe to ship
// without a driver-backend regression: trivial routes inherit (adaptive →
// inline win) while explicitly-async blocking routes stay always-async.
func TestRouteAsync_ExplicitAsyncOptsOutOfAdaptive(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	s.GET("/cache/:key", noopHandler).Async() // blocking driver route
	if s.router.adaptiveRoutes["/cache/:key"] {
		t.Fatal("explicit .Async() route must be removed from the adaptive set")
	}
	if _, promoted := s.router.promoted.Load("/cache/:key"); promoted {
		t.Fatal("explicit .Async() route must carry no promotion state")
	}
	if !s.router.routeAsync("GET", "/cache/42") {
		t.Fatal("explicit .Async() route must resolve hard-async (never inline)")
	}
}

// TestRouteAsync_GroupInherit verifies group-level Async applies to its
// routes.
func TestRouteAsync_GroupInherit(t *testing.T) {
	s := New(Config{AsyncHandlers: false})
	api := s.Group("/api").Async()
	api.GET("/products", noopHandler)
	if !s.router.routeAsync("GET", "/api/products") {
		t.Fatal("/api/products should be async via group default")
	}
	// A top-level route stays sync.
	s.GET("/ping", noopHandler)
	if s.router.routeAsync("GET", "/ping") {
		t.Fatal("/ping should stay sync (server default)")
	}
}

// TestRouteAsync_RouteOverridesGroup verifies most-specific-wins: a route
// override beats the group default.
func TestRouteAsync_RouteOverridesGroup(t *testing.T) {
	s := New(Config{AsyncHandlers: false})
	api := s.Group("/api").Async()
	api.GET("/products", noopHandler)            // async (group)
	api.GET("/cached", noopHandler).Async(false) // sync (route override)
	if !s.router.routeAsync("GET", "/api/products") {
		t.Fatal("/api/products should be async (group)")
	}
	if s.router.routeAsync("GET", "/api/cached") {
		t.Fatal("/api/cached should be sync (route override beats group)")
	}
}

// TestRouteAsync_SubGroupInherit verifies a sub-group inherits its
// parent's async setting and can override it.
func TestRouteAsync_SubGroupInherit(t *testing.T) {
	s := New(Config{AsyncHandlers: false})
	api := s.Group("/api").Async()
	v1 := api.Group("/v1")              // inherits async
	v2 := api.Group("/v2").Async(false) // overrides to sync
	v1.GET("/a", noopHandler)
	v2.GET("/b", noopHandler)
	if !s.router.routeAsync("GET", "/api/v1/a") {
		t.Fatal("/api/v1/a should inherit async from parent group")
	}
	if s.router.routeAsync("GET", "/api/v2/b") {
		t.Fatal("/api/v2/b should be sync (sub-group override)")
	}
}

// TestRouteAsync_ParamRoute verifies the resolver walks the tree (not just
// the static map) for parameterised routes.
func TestRouteAsync_ParamRoute(t *testing.T) {
	s := New(Config{AsyncHandlers: false})
	s.GET("/users/:id", noopHandler).Async()
	if !s.router.routeAsync("GET", "/users/42") {
		t.Fatal("/users/:id should resolve async via the tree walk")
	}
}

// TestRouteAsync_Count verifies asyncRouteCount tracks toggles correctly,
// including Route.Async flipping a route off and on.
func TestRouteAsync_Count(t *testing.T) {
	s := New(Config{AsyncHandlers: false})
	r1 := s.GET("/a", noopHandler).Async()
	s.GET("/b", noopHandler).Async()
	if s.router.asyncRouteCount != 2 {
		t.Fatalf("asyncRouteCount = %d, want 2", s.router.asyncRouteCount)
	}
	// Flip /a back to sync — count should drop to 1.
	r1.Async(false)
	if s.router.asyncRouteCount != 1 {
		t.Fatalf("asyncRouteCount = %d, want 1 after flipping /a sync", s.router.asyncRouteCount)
	}
	if !s.router.hasAsyncRoutes() {
		t.Fatal("still one async route (/b)")
	}
	// Flip /a async again — back to 2 (idempotent toggle accounting).
	r1.Async()
	if s.router.asyncRouteCount != 2 {
		t.Fatalf("asyncRouteCount = %d, want 2 after re-flipping /a async", s.router.asyncRouteCount)
	}
}

// TestRouteAsync_Unmatched verifies the resolver returns false (sync) for
// paths with no registered route.
func TestRouteAsync_Unmatched(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	s.GET("/known", noopHandler)
	if s.router.routeAsync("GET", "/unknown") {
		t.Fatal("unmatched path should resolve sync (no async dispatch for 404)")
	}
}

// TestRouteAsync_ResolverInterface verifies the routerAdapter exposes the
// resolver the H2 processor relies on.
func TestRouteAsync_ResolverInterface(t *testing.T) {
	s := New(Config{AsyncHandlers: false})
	s.GET("/db", noopHandler).Async()
	var ra stream.AsyncRouteResolver = &routerAdapter{server: s}
	if !ra.RouteAsync("GET", "/db") {
		t.Fatal("adapter RouteAsync should report /db async")
	}
	if ra.RouteAsync("GET", "/missing") {
		t.Fatal("adapter RouteAsync should report unmatched sync")
	}
}

// TestRouteAsync_AdaptiveHysteresis verifies celeris#356 promotion hysteresis:
// a single slow inline run (cold start / GC) must NOT promote a route, but
// adaptivePromoteStreak consecutive slow runs (a genuinely-blocking handler)
// must. A fast run resets the streak.
func TestRouteAsync_AdaptiveHysteresis(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	s.GET("/h", noopHandler)
	rt := s.router
	// One slow outlier, then a fast run → no promotion.
	rt.recordInlineRun("/h", true)
	rt.recordInlineRun("/h", false)
	if rt.routeAsync("GET", "/h") {
		t.Fatal("a single slow run must not promote (cold-start hysteresis)")
	}
	// Sustained slowness (blocking handler) → promotion after the streak.
	for i := 0; i < adaptivePromoteStreak; i++ {
		rt.recordInlineRun("/h", true)
	}
	if !rt.routeAsync("GET", "/h") {
		t.Fatalf("%d consecutive slow runs must promote to async", adaptivePromoteStreak)
	}
}

// TestRouteAsync_AdaptiveSettles verifies celeris#361: a route fast on
// adaptiveSettleStreak CONSECUTIVE inline runs SETTLES (leaves the timed
// learning path so the hot loop stops paying two time.Now() per request), and a
// slow run before the streak completes resets it. A settled route still runs
// inline (not async).
func TestRouteAsync_AdaptiveSettles(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	route := s.GET("/s", noopHandler)
	rt := s.router

	// One short of the streak → still learning.
	for i := 0; i < adaptiveSettleStreak-1; i++ {
		rt.recordInlineRun("/s", false)
	}
	if !rt.adaptiveLearning("/s") {
		t.Fatal("route must still be learning before the settle streak completes")
	}

	// A slow run resets the fast streak; the next near-full run must NOT settle.
	rt.recordInlineRun("/s", true)
	for i := 0; i < adaptiveSettleStreak-1; i++ {
		rt.recordInlineRun("/s", false)
	}
	if !rt.adaptiveLearning("/s") {
		t.Fatal("a slow run must reset the fast streak — route not settled yet")
	}

	// One more fast run completes the streak → settle.
	rt.recordInlineRun("/s", false)
	if rt.adaptiveLearning("/s") {
		t.Fatalf("route must settle after %d consecutive fast runs", adaptiveSettleStreak)
	}
	if rt.routeAsync("GET", "/s") {
		t.Fatal("a settled route runs INLINE, not async")
	}

	// An explicit override clears the settled state (setAsync).
	route.Sync()
	if _, settled := rt.settled.Load("/s"); settled {
		t.Fatal("explicit .Sync() must clear the settled state")
	}
}
