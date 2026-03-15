package celeris

import (
	"fmt"
	"slices"
	"sync"
)

// staticEntry holds the pre-composed handler chain and full path for a fully
// static route, enabling O(1) map lookup instead of a trie walk.
type staticEntry struct {
	handlers []HandlerFunc
	fullPath string
}

// router is a compressed radix trie router with a separate tree per HTTP method.
type router struct {
	trees        map[string]*node
	staticRoutes map[string]map[string]staticEntry // method -> path -> entry
	namedRoutes  map[string]*Route
	namedMu      sync.RWMutex
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

	// Keep the static fast-path map in sync with the updated chain.
	if r.router != nil {
		if m := r.router.staticRoutes[r.method]; m != nil {
			if _, ok := m[r.path]; ok {
				m[r.path] = staticEntry{handlers: chain, fullPath: r.path}
			}
		}
	}
	return r
}

func newRouter() *router {
	return &router{
		trees:        make(map[string]*node),
		staticRoutes: make(map[string]map[string]staticEntry),
		namedRoutes:  make(map[string]*Route),
	}
}

func (r *router) addRoute(method, path string, handlers []HandlerFunc) *Route {
	if path == "" || path[0] != '/' {
		panic("path must begin with '/'")
	}
	validatePath(path)

	root := r.trees[method]
	if root == nil {
		root = &node{path: "/"}
		r.trees[method] = root
	}

	route := &Route{method: method, path: path, router: r}

	if path == "/" {
		root.handlers = handlers
		root.fullPath = "/"
		route.node = root
		m := r.staticRoutes[method]
		if m == nil {
			m = make(map[string]staticEntry)
			r.staticRoutes[method] = m
		}
		m["/"] = staticEntry{handlers: handlers, fullPath: "/"}
		return route
	}

	// Split path into segments for insertion.
	segments := splitPath(path)
	current := root
	for _, seg := range segments {
		current = insertChild(current, seg)
	}
	current.handlers = handlers
	current.fullPath = path
	route.node = current

	// Register in static map for O(1) lookup on fully static paths.
	if isStaticPath(path) {
		m := r.staticRoutes[method]
		if m == nil {
			m = make(map[string]staticEntry)
			r.staticRoutes[method] = m
		}
		m[path] = staticEntry{handlers: handlers, fullPath: path}
	}

	return route
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

func (r *router) find(method, path string, params *Params) ([]HandlerFunc, string) {
	// Static fast path: O(1) map lookup for fully static routes.
	if m := r.staticRoutes[method]; m != nil {
		if e, ok := m[path]; ok {
			return e.handlers, e.fullPath
		}
	}

	root := r.trees[method]
	if root == nil {
		return nil, ""
	}

	// Collapse consecutive slashes: "//a///b" → "/a/b".
	path = cleanPath(path)

	if path == "/" {
		return root.handlers, root.fullPath
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
	for method := range r.trees {
		if method == except {
			continue
		}
		params = params[:0]
		if handlers, _ := r.find(method, path, &params); handlers != nil {
			allowed = append(allowed, method)
		}
	}
	return allowed
}

// walk returns all registered routes sorted by method then path for
// deterministic output.
func (r *router) walk() []RouteInfo {
	var routes []RouteInfo
	methods := make([]string, 0, len(r.trees))
	for method := range r.trees {
		methods = append(methods, method)
	}
	slices.Sort(methods)
	for _, method := range methods {
		root := r.trees[method]
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
