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

// TestRouteAsync_ServerDefaultAsync verifies routes inherit an async
// server default (Config.AsyncHandlers=true) when not overridden.
func TestRouteAsync_ServerDefaultAsync(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	s.GET("/ping", noopHandler)
	if !s.router.routeAsync("GET", "/ping") {
		t.Fatal("route should inherit async server default")
	}
	if !s.router.hasAsyncRoutes() {
		t.Fatal("hasAsyncRoutes should be true when default is async")
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

// TestRouteAsync_RouteOverrideOff forces a single route sync on an
// async-default server.
func TestRouteAsync_RouteOverrideOff(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	s.GET("/db", noopHandler)
	s.GET("/cached", noopHandler).Async(false)
	if !s.router.routeAsync("GET", "/db") {
		t.Fatal("/db should inherit async default")
	}
	if s.router.routeAsync("GET", "/cached") {
		t.Fatal("/cached should be forced sync via Async(false)")
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
