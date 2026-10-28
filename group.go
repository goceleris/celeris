package celeris

import "strings"

// RouteGroup is a collection of routes that share a common path prefix and
// middleware. Use [Server.Group] to create one. A RouteGroup must not be used
// after [Server.Start] is called.
type RouteGroup struct {
	prefix     string
	middleware []HandlerFunc
	server     *Server
	// async is the group-level dispatch override applied to routes
	// registered on this group (and inherited by sub-groups). Routes
	// can still override per-route via Route.Async. asyncDefault means
	// "inherit the server default (Config.AsyncHandlers)".
	async asyncSetting
}

func (g *RouteGroup) handle(method, path string, handlers ...HandlerFunc) *Route {
	fullPath := g.prefix + path
	chain := make([]HandlerFunc, 0, len(g.server.middleware)+len(g.middleware)+len(handlers))
	chain = append(chain, g.server.middleware...)
	chain = append(chain, g.middleware...)
	chain = append(chain, handlers...)
	return g.server.router.addRouteWithAsync(method, fullPath, chain, g.async)
}

// Async sets the dispatch mode for all routes registered on this group
// AFTER this call (and inherited by sub-groups created after it),
// overriding the server-level [Config.AsyncHandlers] default. Individual
// routes can still override via [Route.Async] / [Route.Sync] (route > group >
// server precedence); use [RouteGroup.Sync] to opt a group out of an async
// default.
// Must be called before [Server.Start]. Chainable:
//
//	api := srv.Group("/api").Async()              // async for all /api/* routes
//	api.GET("/products", productHandler)
//	api.GET("/cached", cachedHandler).Sync()      // opt this one back to sync
//
// Async only affects routes registered AFTER the call. To make the entire
// group async, call .Async() immediately after Group(...).
//
// SAFETY: do NOT call .Sync() on a group whose handlers hijack/detach the
// connection (WebSocket upgrade, SSE) — detached flows are async by
// construction.
func (g *RouteGroup) Async() *RouteGroup {
	g.async = asyncOn
	return g
}

// Sync forces all routes registered on this group AFTER this call to run
// inline on the worker / event-loop thread, overriding any server-level
// async default. Individual routes can still override via [Route.Async].
// Chainable.
//
// SAFETY: do NOT call .Sync() on a group whose handlers hijack/detach the
// connection (WebSocket upgrade, SSE) — detached flows are async by
// construction.
func (g *RouteGroup) Sync() *RouteGroup {
	g.async = asyncOff
	return g
}

// Use adds middleware to this group. Group middleware runs after server-level
// middleware but before route handlers within this group. Middleware chains
// are composed at route registration time, so Use must be called before
// registering routes on this group.
func (g *RouteGroup) Use(middleware ...HandlerFunc) *RouteGroup {
	g.middleware = append(g.middleware, middleware...)
	return g
}

// GET registers a handler for GET requests.
func (g *RouteGroup) GET(path string, handlers ...HandlerFunc) *Route {
	return g.handle("GET", path, handlers...)
}

// POST registers a handler for POST requests.
func (g *RouteGroup) POST(path string, handlers ...HandlerFunc) *Route {
	return g.handle("POST", path, handlers...)
}

// PUT registers a handler for PUT requests.
func (g *RouteGroup) PUT(path string, handlers ...HandlerFunc) *Route {
	return g.handle("PUT", path, handlers...)
}

// DELETE registers a handler for DELETE requests.
func (g *RouteGroup) DELETE(path string, handlers ...HandlerFunc) *Route {
	return g.handle("DELETE", path, handlers...)
}

// PATCH registers a handler for PATCH requests.
func (g *RouteGroup) PATCH(path string, handlers ...HandlerFunc) *Route {
	return g.handle("PATCH", path, handlers...)
}

// HEAD registers a handler for HEAD requests. See [Server.HEAD]: a path
// without a HEAD route is answered by its GET route.
func (g *RouteGroup) HEAD(path string, handlers ...HandlerFunc) *Route {
	return g.handle("HEAD", path, handlers...)
}

// OPTIONS registers a handler for OPTIONS requests. See [Server.OPTIONS]: a
// path without an OPTIONS route is answered automatically.
func (g *RouteGroup) OPTIONS(path string, handlers ...HandlerFunc) *Route {
	return g.handle("OPTIONS", path, handlers...)
}

// Any registers a handler for all HTTP methods, returning the [Route] for each.
func (g *RouteGroup) Any(path string, handlers ...HandlerFunc) []*Route {
	methods := []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"}
	routes := make([]*Route, len(methods))
	for i, method := range methods {
		routes[i] = g.handle(method, path, handlers...)
	}
	return routes
}

// Handle registers a handler for the given HTTP method and path pattern.
func (g *RouteGroup) Handle(method, path string, handlers ...HandlerFunc) *Route {
	return g.handle(method, path, handlers...)
}

// Static registers a GET handler that serves files from root under the given
// prefix. Uses FileFromDir for path traversal protection. root may be ".",
// "./" (from v1.6.0; before, every file below them was refused): "." is the
// working directory, so name a directory that holds only what is public. A root
// that resolves to the root of a filesystem ("/", "." when the working directory
// is "/", a symlink to "/") panics here, with no way to acknowledge it: serve a
// filesystem root on purpose with middleware/static and AllowFilesystemRoot. A
// root made of ".." segments means the directory it resolves to, resolved once
// when the route is registered, and nothing outside it is served. An empty root
// serves nothing. A file that is not there is a 404; devices, FIFOs and sockets
// are not served. Files and directories whose name starts with a dot (.env, .git) are
// not served, and a path to one, or to a symlink that leads to one, is a 404
// whether or not it exists; the exact segment ".well-known" is served. There is
// no opt-out here: use middleware/static with Dotfiles: static.DotfilesAllow.
func (g *RouteGroup) Static(prefix, root string) *Route {
	p := strings.TrimRight(prefix, "/") + "/*filepath"
	root = staticRoot(root)
	return g.GET(p, func(c *Context) error {
		return c.serveStaticFile(root, c.Param("filepath"))
	})
}

// Group creates a sub-group with the given path prefix. The sub-group inherits
// middleware from the parent group.
func (g *RouteGroup) Group(prefix string, middleware ...HandlerFunc) *RouteGroup {
	return &RouteGroup{
		prefix:     g.prefix + prefix,
		middleware: append(g.combineMiddleware(), middleware...),
		server:     g.server,
		async:      g.async, // inherit parent's dispatch override
	}
}

func (g *RouteGroup) combineMiddleware() []HandlerFunc {
	combined := make([]HandlerFunc, len(g.middleware))
	copy(combined, g.middleware)
	return combined
}
