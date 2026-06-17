package celeris

import (
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// nowNano returns the current time in Unix nanoseconds. A package var so the
// adaptive promotion TTL (celeris#364) can be exercised deterministically in
// tests without sleeping.
var nowNano = func() int64 { return time.Now().UnixNano() }

// staticEntry holds the pre-composed handler chain and full path for a fully
// static route, enabling O(1) map lookup instead of a trie walk.
type staticEntry struct {
	handlers []HandlerFunc
	fullPath string
	async    bool
}

// asyncSetting is the tri-state per-route/per-group dispatch override
// carried from the registration API down to the router. asyncDefault
// means "inherit the server default (Config.AsyncHandlers)"; asyncOn /
// asyncOff are explicit overrides set by .Async(true) / .Async(false).
type asyncSetting int8

const (
	asyncDefault asyncSetting = iota
	asyncOn
	asyncOff
)

// resolveAsync collapses a setting against the router's server default.
func (r *router) resolveAsync(s asyncSetting) bool {
	switch s {
	case asyncOn:
		return true
	case asyncOff:
		return false
	default:
		return r.defaultAsync
	}
}

// Method index constants for array-indexed trees and static routes.
// Standard HTTP methods use O(1) array indexing; custom methods fall back to maps.
const (
	mGET = iota
	mPOST
	mPUT
	mDELETE
	mPATCH
	mHEAD
	mOPTIONS
	nMethods
)

var methodNames = [nMethods]string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"}

func methodIndex(method string) int {
	switch method {
	case "GET":
		return mGET
	case "POST":
		return mPOST
	case "PUT":
		return mPUT
	case "DELETE":
		return mDELETE
	case "PATCH":
		return mPATCH
	case "HEAD":
		return mHEAD
	case "OPTIONS":
		return mOPTIONS
	default:
		return -1
	}
}

// router is a compressed radix trie router with a separate tree per HTTP method.
// Standard methods (GET, POST, PUT, DELETE, PATCH, HEAD, OPTIONS) use fixed-size
// arrays for O(1) method dispatch, avoiding map hash overhead on the hot path.
type router struct {
	trees        [nMethods]*node
	staticRoutes [nMethods]map[string]staticEntry
	customTrees  map[string]*node                  // overflow for non-standard methods
	customStatic map[string]map[string]staticEntry // overflow for non-standard methods
	namedRoutes  map[string]*Route
	namedMu      sync.RWMutex

	// defaultAsync is the server-level dispatch default, set from
	// Config.AsyncHandlers at server construction. Routes/groups that
	// don't override inherit it.
	defaultAsync bool
	// asyncRouteCount tracks how many registered routes resolve to
	// async dispatch. hasAsyncRoutes() reads it to let the engine
	// decide whether the async dispatch infrastructure is needed at
	// all (a server with zero async routes keeps the inline fast path).
	asyncRouteCount int

	// adaptiveRoutes (celeris#356) holds the fullPaths of routes that
	// inherited the server-level AsyncHandlers=true default (rather than an
	// explicit .Async()). They start INLINE (ring-batched send, the cheap
	// path) and are promoted to async dispatch only when an inline run is
	// observed to block — so trivial handlers keep io_uring's syscall
	// batching while genuinely-blocking handlers still get goroutine
	// isolation. Built at registration; read-only while serving.
	adaptiveRoutes map[string]bool
	// promoted records adaptive fullPaths that have been promoted to async
	// after sustained blocking inline runs. sync.Map: concurrent reads on the
	// hot path, rare writes at promotion time.
	promoted sync.Map
	// slowStreak tracks consecutive slow inline runs per adaptive fullPath
	// (fullPath -> *atomic.Int32). Hysteresis: a one-off cold/GC outlier must
	// not poison a fast route; only a handler that blocks on EVERY run (DB/
	// cache) accumulates adaptivePromoteStreak slow runs and gets promoted. A
	// fast run resets the streak.
	slowStreak sync.Map
	// fastStreak tracks consecutive FAST inline runs per adaptive fullPath
	// (fullPath -> *atomic.Int32). Once a route is fast on adaptiveSettleStreak
	// consecutive runs it is provably non-blocking and gets SETTLED — future
	// requests skip the per-request inline timing (two time.Now() vDSO calls)
	// entirely (celeris#361). A slow run resets it.
	fastStreak sync.Map
	// settled holds adaptive fullPaths proven non-blocking (see fastStreak).
	// A settled route is no longer timed/promotable; it runs inline like a
	// plain sync route. Explicit .Async()/.Sync() clears it (setAsync).
	settled sync.Map
}

// Route is an opaque handle to a registered route. Use the Name method to
// assign a name for reverse lookup via [Server.URL].
type Route struct {
	method string
	path   string
	name   string
	router *router
	node   *node
}

// Name sets a name for this route, enabling reverse URL generation via
// [Server.URL]. Panics if a route with the same name is already registered.
func (r *Route) Name(name string) *Route {
	r.name = name
	if r.router != nil {
		r.router.namedMu.Lock()
		if _, exists := r.router.namedRoutes[name]; exists {
			r.router.namedMu.Unlock()
			panic("celeris: duplicate route name: " + name)
		}
		r.router.namedRoutes[name] = r
		r.router.namedMu.Unlock()
	}
	return r
}

// TryName is like [Route.Name] but returns an error instead of panicking when
// a route with the same name already exists.
func (r *Route) TryName(name string) error {
	if r.router == nil {
		r.name = name
		return nil
	}
	r.router.namedMu.Lock()
	defer r.router.namedMu.Unlock()
	if _, exists := r.router.namedRoutes[name]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateRouteName, name)
	}
	r.name = name
	r.router.namedRoutes[name] = r
	return nil
}

// Use prepends middleware to this specific route's handler chain.
// Must be called before [Server.Start]. Panics if the route has no handlers.
func (r *Route) Use(middleware ...HandlerFunc) *Route {
	if r.node == nil || len(r.node.handlers) == 0 {
		panic("celeris: Use called on route with no handlers")
	}
	n := len(r.node.handlers)
	final := r.node.handlers[n-1]
	chain := make([]HandlerFunc, 0, n+len(middleware))
	chain = append(chain, r.node.handlers[:n-1]...)
	chain = append(chain, middleware...)
	chain = append(chain, final)
	r.node.handlers = chain

	// Keep the static fast-path map in sync with the updated chain,
	// preserving the route's resolved async flag.
	if r.router != nil {
		if m := r.router.getStaticMap(r.method); m != nil {
			if _, ok := m[r.path]; ok {
				m[r.path] = staticEntry{handlers: chain, fullPath: r.path, async: r.node.async}
			}
		}
	}
	return r
}

// Async marks this route for the per-conn dispatch goroutine, overriding
// any group-level or server-level (Config.AsyncHandlers) default.
// SAFETY: do NOT call .Sync() (or .Async(false)) on a WebSocket / SSE
// handler — detached flows run async by construction.
//
// Precedence is route > group > server default. Must be called before
// [Server.Start]. Chainable. Async() with no argument means true; the
// variadic-bool form is kept for back-compat — prefer .Sync() to opt a
// single route out of an async-default group/server.
//
//	srv.GET("/users/:id", userHandler).Async()      // async, this route
//	api.GET("/cached", cachedHandler).Sync()        // opt out of group async
//
// See [Config.AsyncHandlers] for the server-level default and the
// per-handler async design overview.
func (r *Route) Async(opt ...bool) *Route {
	want := true
	if len(opt) > 0 {
		want = opt[0]
	}
	return r.setAsync(want)
}

// Sync forces this route to run inline on the worker / event-loop thread,
// overriding any group-level or server-level async default. Equivalent to
// .Async(false) but reads more naturally at the call site:
//
//	api := srv.Group("/api").Async()
//	api.GET("/products", productHandler)            // async (from group)
//	api.GET("/healthz", healthHandler).Sync()       // opt this one back to sync
//
// SAFETY: do NOT call .Sync() on a handler that hijacks/detaches the
// connection (WebSocket upgrade, SSE). Detached flows run async by
// construction (a middleware goroutine owns the connection after Detach),
// so the per-route flag cannot downgrade them to sync.
func (r *Route) Sync() *Route {
	return r.setAsync(false)
}

// setAsync is the shared implementation of [Route.Async] and [Route.Sync].
// Updates the node, the router's asyncRouteCount, and the static-fast-path
// map entry so all three observers see the same async flag.
func (r *Route) setAsync(want bool) *Route {
	if r.node == nil {
		return r
	}
	if r.router != nil {
		// celeris#356: an explicit .Async()/.Sync() overrides the adaptive
		// (server-default) classification — the route is no longer
		// inline-first-with-promotion, so drop it from the adaptive set and
		// clear any prior promotion.
		if r.router.adaptiveRoutes[r.path] {
			delete(r.router.adaptiveRoutes, r.path)
			r.router.promoted.Delete(r.path)
			r.router.settled.Delete(r.path)
			r.router.fastStreak.Delete(r.path)
			r.router.slowStreak.Delete(r.path)
			// Adaptive routes were already counted (they may promote): keep the
			// count when the explicit choice is async, drop it when sync.
			if !want && r.router.asyncRouteCount > 0 {
				r.router.asyncRouteCount--
			}
		} else if r.node.async != want {
			if want {
				r.router.asyncRouteCount++
			} else if r.router.asyncRouteCount > 0 {
				r.router.asyncRouteCount--
			}
		}
	}
	r.node.async = want
	// Keep the static fast-path map in sync.
	if r.router != nil {
		if m := r.router.getStaticMap(r.method); m != nil {
			if e, ok := m[r.path]; ok {
				e.async = want
				m[r.path] = e
			}
		}
	}
	return r
}

func newRouter() *router {
	return &router{
		namedRoutes:    make(map[string]*Route),
		adaptiveRoutes: make(map[string]bool),
	}
}

// isPromoted reports whether an adaptive route (celeris#356) is currently
// promoted to async dispatch.
//
// Promotion is REVERSIBLE (celeris#364): it expires after adaptivePromoteTTL.
// Once expired, the route is dropped from the promoted set and its slow streak
// is cleared, so the next request runs INLINE again and is re-timed — both the
// routing decision (adaptivePromoted) and the per-request timing decision
// (adaptiveLearning) key off this method, so they flip back together. A route
// that is genuinely blocking re-promotes within adaptivePromoteStreak runs; a
// route that was promoted by a transient load/jitter spike (a CPU-bound chain
// whose inline wall-clock briefly crossed the threshold under contention) stays
// inline. Without this, a single spike pinned a route to the slower async path
// until process restart (the intermittent ~32% chain collapse).
//
// The clock is read only for routes actually in the promoted set, so learning
// and settled routes pay nothing here.
func (r *router) isPromoted(fullPath string) bool {
	v, ok := r.promoted.Load(fullPath)
	if !ok {
		return false
	}
	if nowNano()-v.(int64) > int64(adaptivePromoteTTL) {
		// Expired: re-enter the learning/inline path to re-evaluate.
		r.promoted.Delete(fullPath)
		if sv, ok := r.slowStreak.Load(fullPath); ok {
			sv.(*atomic.Int32).Store(0)
		}
		return false
	}
	return true
}

// promoteRoute marks an adaptive route as async after sustained blocking inline
// runs, stamped with the promotion time so isPromoted can expire it
// (celeris#364). Re-promotion refreshes the stamp.
func (r *router) promoteRoute(fullPath string) {
	r.promoted.Store(fullPath, nowNano())
}

// recordInlineRun feeds one inline-run observation into the adaptive
// classifier (celeris#356). A fast run resets the slow streak; a slow run
// increments it, and once a route is slow on adaptivePromoteStreak CONSECUTIVE
// runs it is promoted to async. The consecutive requirement makes a single cold
// start / GC pause harmless while a genuinely-blocking handler (slow on every
// run) promotes within a handful of requests.
func (r *router) recordInlineRun(fullPath string, slow bool) {
	if !slow {
		if v, ok := r.slowStreak.Load(fullPath); ok {
			v.(*atomic.Int32).Store(0)
		}
		// celeris#361: a route fast on adaptiveSettleStreak CONSECUTIVE runs is
		// provably non-blocking — settle it so adaptiveLearning short-circuits
		// and handler.go stops timing every request forever.
		fv, _ := r.fastStreak.LoadOrStore(fullPath, new(atomic.Int32))
		if fv.(*atomic.Int32).Add(1) >= adaptiveSettleStreak {
			r.settled.Store(fullPath, struct{}{})
		}
		return
	}
	if v, ok := r.fastStreak.Load(fullPath); ok {
		v.(*atomic.Int32).Store(0)
	}
	v, _ := r.slowStreak.LoadOrStore(fullPath, new(atomic.Int32))
	if v.(*atomic.Int32).Add(1) >= adaptivePromoteStreak {
		r.promoteRoute(fullPath)
	}
}

// adaptiveLearning reports whether an adaptive route is still being observed —
// i.e. neither settled (proven non-blocking, celeris#361) nor promoted (proven
// blocking, celeris#356). Only learning routes pay the per-request inline
// timing in handler.go; once decided, the hot path skips it. Steady state is a
// single sync.Map.Load (settled), the same lookup cost the prior isPromoted
// check carried, but without the two time.Now() vDSO calls per request.
func (r *router) adaptiveLearning(fullPath string) bool {
	if _, ok := r.settled.Load(fullPath); ok {
		return false
	}
	return !r.isPromoted(fullPath)
}

// addRoute registers a route inheriting the server-level async default.
// Kept as the 3-arg form for existing callers/tests; addRouteWithAsync is
// the underlying implementation that also accepts a per-route/group
// dispatch override.
func (r *router) addRoute(method, path string, handlers []HandlerFunc) *Route {
	return r.addRouteWithAsync(method, path, handlers, asyncDefault)
}

func (r *router) addRouteWithAsync(method, path string, handlers []HandlerFunc, as asyncSetting) *Route {
	if path == "" || path[0] != '/' {
		panic("path must begin with '/'")
	}
	validatePath(path)

	async := r.resolveAsync(as)
	// celeris#356: a route that inherits the server-level AsyncHandlers=true
	// default (rather than an explicit .Async()) starts INLINE and is promoted
	// to async only when an inline run is observed to block. Explicit
	// .Async(true)/.Async(false) is honored verbatim.
	adaptive := as == asyncDefault && r.defaultAsync
	if adaptive {
		async = false
		r.adaptiveRoutes[path] = true
	}

	root := r.getTree(method)
	if root == nil {
		root = &node{path: "/"}
		r.setTree(method, root)
	}

	route := &Route{method: method, path: path, router: r}

	if path == "/" {
		if root.handlers != nil {
			warnDuplicateRoute(method, "/")
		}
		root.handlers = handlers
		root.fullPath = "/"
		root.async = async
		route.node = root
		r.setStaticEntry(method, "/", staticEntry{handlers: handlers, fullPath: "/", async: async})
		if async || adaptive {
			r.asyncRouteCount++
		}
		return route
	}

	// Split path into segments for insertion.
	segments := splitPath(path)
	current := root
	for _, seg := range segments {
		current = insertChild(current, seg)
	}
	if current.handlers != nil {
		warnDuplicateRoute(method, path)
	}
	current.handlers = handlers
	current.fullPath = path
	current.async = async
	route.node = current

	// Register in static map for O(1) lookup on fully static paths.
	if isStaticPath(path) {
		r.setStaticEntry(method, path, staticEntry{handlers: handlers, fullPath: path, async: async})
	}

	if async || adaptive {
		r.asyncRouteCount++
	}

	return route
}

// hasAsyncRoutes reports whether any registered route resolves to async
// dispatch. The engine uses this (OR'd with the server default) to decide
// whether to wire up the async dispatch infrastructure at all.
func (r *router) hasAsyncRoutes() bool {
	return r.asyncRouteCount > 0
}

// routeAsync resolves whether the route matching method+path runs async,
// without filling a Params slice for the caller. Fully static routes (the
// common async-annotated case, e.g. /api/...) resolve via the O(1) static
// map with zero allocation; parameterised routes pay a scratch Params walk.
// Returns false when no route matches (unmatched → 404 handler, which is
// never async).
func (r *router) routeAsync(method, path string) bool {
	idx := methodIndex(method)
	if idx >= 0 {
		if m := r.staticRoutes[idx]; m != nil {
			if e, ok := m[path]; ok {
				return e.async || r.adaptivePromoted(e.fullPath)
			}
		}
	} else if r.customStatic != nil {
		if m := r.customStatic[method]; m != nil {
			if e, ok := m[path]; ok {
				return e.async || r.adaptivePromoted(e.fullPath)
			}
		}
	}
	var params Params
	_, fullPath, async := r.find(method, path, &params)
	return async || r.adaptivePromoted(fullPath)
}

// adaptivePromoted reports whether an adaptive (server-default-async) route has
// been promoted to async dispatch after a blocking inline run (celeris#356).
// The adaptiveRoutes map is read-only while serving, so the lookup is a plain
// (lock-free) map read followed by a sync.Map load only for adaptive routes;
// non-adaptive configs (no AsyncHandlers default) keep the empty-map fast path.
func (r *router) adaptivePromoted(fullPath string) bool {
	return r.adaptiveRoutes[fullPath] && r.isPromoted(fullPath)
}

// warnDuplicateRoute emits a warning when a route is registered twice for
// the same method+path. The second registration silently overwrites the
// first, which is almost always a bug (typo, accidental duplicate, or a
// merge that kept two copies). We log instead of panicking to preserve
// back-compat for apps that intentionally rebuild their routing table.
func warnDuplicateRoute(method, path string) {
	slog.Default().Warn("celeris: duplicate route registration; previous handler overwritten",
		"method", method, "path", path)
}

// isStaticPath reports whether path contains no parameter (`:`) or catchAll
// (`*`) segments, making it eligible for the O(1) static fast path.
func isStaticPath(path string) bool {
	for i := range len(path) {
		if path[i] == ':' || path[i] == '*' {
			return false
		}
	}
	return true
}

func (r *router) find(method, path string, params *Params) ([]HandlerFunc, string, bool) {
	idx := methodIndex(method)
	var root *node

	// Fast path: standard HTTP methods use array indexing (no hash).
	if idx >= 0 {
		if m := r.staticRoutes[idx]; m != nil {
			if e, ok := m[path]; ok {
				return e.handlers, e.fullPath, e.async
			}
		}
		root = r.trees[idx]
	} else {
		// Slow path: custom methods use maps.
		if r.customStatic != nil {
			if m := r.customStatic[method]; m != nil {
				if e, ok := m[path]; ok {
					return e.handlers, e.fullPath, e.async
				}
			}
		}
		if r.customTrees != nil {
			root = r.customTrees[method]
		}
	}

	if root == nil {
		return nil, "", false
	}

	// Collapse consecutive slashes: "//a///b" → "/a/b".
	path = cleanPath(path)

	if path == "/" {
		return root.handlers, root.fullPath, root.async
	}
	if len(path) > 1 && path[0] == '/' {
		path = path[1:]
	}

	return search(root, path, params)
}

// allowedMethods returns the HTTP methods that have a registered handler for
// the given path, excluding the specified method.
func (r *router) allowedMethods(path string, except string) []string {
	var allowed []string
	var params Params
	for i, root := range r.trees {
		if root == nil {
			continue
		}
		method := methodNames[i]
		if method == except {
			continue
		}
		params = params[:0]
		if handlers, _, _ := r.find(method, path, &params); handlers != nil {
			allowed = append(allowed, method)
		}
	}
	for method := range r.customTrees {
		if method == except {
			continue
		}
		params = params[:0]
		if handlers, _, _ := r.find(method, path, &params); handlers != nil {
			allowed = append(allowed, method)
		}
	}
	return allowed
}

// walk returns all registered routes sorted by method then path for
// deterministic output.
func (r *router) walk() []RouteInfo {
	var routes []RouteInfo
	methods := make([]string, 0, nMethods)
	for i, root := range r.trees {
		if root != nil {
			methods = append(methods, methodNames[i])
		}
	}
	for method := range r.customTrees {
		methods = append(methods, method)
	}
	slices.Sort(methods)
	for _, method := range methods {
		root := r.getTree(method)
		if root.handlers != nil {
			routes = append(routes, RouteInfo{
				Method:       method,
				Path:         root.fullPath,
				HandlerCount: len(root.handlers),
			})
		}
		walkNode(method, root, &routes)
	}
	return routes
}

func walkNode(method string, n *node, routes *[]RouteInfo) {
	for _, ch := range n.children {
		if ch.handlers != nil {
			*routes = append(*routes, RouteInfo{
				Method:       method,
				Path:         ch.fullPath,
				HandlerCount: len(ch.handlers),
			})
		}
		walkNode(method, ch, routes)
	}
}

// getTree returns the radix trie root for the given method.
func (r *router) getTree(method string) *node {
	if idx := methodIndex(method); idx >= 0 {
		return r.trees[idx]
	}
	if r.customTrees == nil {
		return nil
	}
	return r.customTrees[method]
}

// setTree sets the radix trie root for the given method.
func (r *router) setTree(method string, root *node) {
	if idx := methodIndex(method); idx >= 0 {
		r.trees[idx] = root
		return
	}
	if r.customTrees == nil {
		r.customTrees = make(map[string]*node)
	}
	r.customTrees[method] = root
}

// getStaticMap returns the static route map for the given method.
func (r *router) getStaticMap(method string) map[string]staticEntry {
	if idx := methodIndex(method); idx >= 0 {
		return r.staticRoutes[idx]
	}
	if r.customStatic == nil {
		return nil
	}
	return r.customStatic[method]
}

// setStaticEntry registers a static route entry for the given method and path.
func (r *router) setStaticEntry(method, path string, entry staticEntry) {
	if idx := methodIndex(method); idx >= 0 {
		if r.staticRoutes[idx] == nil {
			r.staticRoutes[idx] = make(map[string]staticEntry)
		}
		r.staticRoutes[idx][path] = entry
		return
	}
	if r.customStatic == nil {
		r.customStatic = make(map[string]map[string]staticEntry)
	}
	m := r.customStatic[method]
	if m == nil {
		m = make(map[string]staticEntry)
		r.customStatic[method] = m
	}
	m[path] = entry
}
