package celeris

import (
	"fmt"
	"log/slog"
	"slices"
	"sync"
)

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

// Async overrides the dispatch mode for this specific route, taking
// precedence over any group-level or server-level (Config.AsyncHandlers)
// default. Async() with no argument means async (true); pass false to
// force this route sync even on an async-default server/group. Must be
// called before [Server.Start]. Chainable:
//
//	srv.GET("/users/:id", userHandler).Async()       // async, this route
//	api.GET("/cached", cachedHandler).Async(false)   // opt out of group async
func (r *Route) Async(opt ...bool) *Route {
	want := true
	if len(opt) > 0 {
		want = opt[0]
	}
	if r.node == nil {
		return r
	}
	if r.node.async != want && r.router != nil {
		if want {
			r.router.asyncRouteCount++
		} else if r.router.asyncRouteCount > 0 {
			r.router.asyncRouteCount--
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
		namedRoutes: make(map[string]*Route),
	}
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
		if async {
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

	if async {
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
				return e.async
			}
		}
	} else if r.customStatic != nil {
		if m := r.customStatic[method]; m != nil {
			if e, ok := m[path]; ok {
				return e.async
			}
		}
	}
	var params Params
	_, _, async := r.find(method, path, &params)
	return async
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
