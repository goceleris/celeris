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
// after this call (and inherited by sub-groups created after it). With no
// argument it means async (true); pass false to force the group sync even
// on an async-default server. Individual routes can still override via
// Route.Async. Chainable:
//
//	api := srv.Group("/api").Async()   // async for all /api/* routes
//	api.GET("/products", productHandler)
func (g *RouteGroup) Async(opt ...bool) *RouteGroup {
	if len(opt) > 0 && !opt[0] {
		g.async = asyncOff
	} else {
		g.async = asyncOn
	}
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

// HEAD registers a handler for HEAD requests.
func (g *RouteGroup) HEAD(path string, handlers ...HandlerFunc) *Route {
	return g.handle("HEAD", path, handlers...)
}

// OPTIONS registers a handler for OPTIONS requests.
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
// prefix. Uses FileFromDir for path traversal protection.
func (g *RouteGroup) Static(prefix, root string) *Route {
	p := strings.TrimRight(prefix, "/") + "/*filepath"
	return g.GET(p, func(c *Context) error {
		return c.FileFromDir(root, c.Param("filepath"))
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
