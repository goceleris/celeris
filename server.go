package celeris

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/observe"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// Version is the semantic version of the celeris module.
const Version = "1.3.4"

// ErrAlreadyStarted is returned when Start or StartWithContext is called on a
// server that is already running.
var ErrAlreadyStarted = errors.New("celeris: server already started")

// ErrRouteNotFound is returned by [Server.URL] when no route with the given
// name has been registered.
var ErrRouteNotFound = errors.New("celeris: named route not found")

// ErrDuplicateRouteName is returned by [Route.TryName] when a route with the
// given name has already been registered.
var ErrDuplicateRouteName = errors.New("celeris: duplicate route name")

// RouteInfo describes a registered route. Returned by [Server.Routes].
type RouteInfo struct {
	// Method is the HTTP method (e.g. "GET", "POST").
	Method string
	// Path is the route pattern (e.g. "/users/:id").
	Path string
	// HandlerCount is the total number of handlers (middleware + handler) in the chain.
	HandlerCount int
}

// Server is the top-level entry point for a celeris HTTP server.
// A Server is safe for concurrent use after Start is called. Route registration
// methods (GET, POST, Use, Group, etc.) must be called before Start.
type Server struct {
	config        Config
	router        *router
	middleware    []HandlerFunc
	preMiddleware []HandlerFunc
	// engineRef holds the active engine. Stored as an atomic.Pointer so that
	// concurrent observers (e.g. Server.Addr() called from a polling test
	// helper while doPrepare is still running on a goroutine) see a
	// consistent value without taking a mutex.
	engineRef atomic.Pointer[engine.Engine]
	collector *observe.Collector

	notFoundHandler         HandlerFunc
	methodNotAllowedHandler HandlerFunc
	errorHandler            func(*Context, error)

	trustedNets   []*net.IPNet
	shutdownHooks []func(ctx context.Context)

	startOnce sync.Once
	startErr  error

	// routesRegistered is set the first time handle() runs. Use() panics if
	// it sees this true (see Use godoc for rationale).
	routesRegistered bool
}

// New creates a Server with the given configuration.
// The metrics [observe.Collector] is created eagerly so that [Server.Collector]
// returns non-nil before Start is called (unless Config.DisableMetrics is set).
func New(cfg Config) *Server {
	s := &Server{
		config: cfg,
		router: newRouter(),
	}
	if !cfg.DisableMetrics {
		s.collector = observe.NewCollector()
	}
	return s
}

func (s *Server) handle(method, path string, handlers ...HandlerFunc) *Route {
	s.routesRegistered = true
	chain := make([]HandlerFunc, 0, len(s.middleware)+len(handlers))
	chain = append(chain, s.middleware...)
	chain = append(chain, handlers...)
	return s.router.addRoute(method, path, chain)
}

// loadEngine returns the active engine, or nil if Start has not yet
// installed one. Safe for concurrent use.
func (s *Server) loadEngine() engine.Engine {
	p := s.engineRef.Load()
	if p == nil {
		return nil
	}
	return *p
}

// Use registers global middleware that runs before every route handler.
// Middleware executes in registration order. Middleware chains are composed
// at route registration time, so Use must be called before registering routes;
// calling it after panics to surface the silent-divergence bug
// (some routes would have the middleware, others would not).
func (s *Server) Use(middleware ...HandlerFunc) *Server {
	if s.routesRegistered {
		panic("celeris: Server.Use called after routes were registered — chains were already baked at handle() time, so this Use call would only apply to routes registered hereafter and produce silently inconsistent middleware coverage. Move Use calls before any GET/POST/etc.")
	}
	s.middleware = append(s.middleware, middleware...)
	return s
}

// Pre registers pre-routing middleware that executes before route lookup.
// Pre-middleware may modify the request method or path (e.g. for rewriting or
// stripping a prefix) before the router resolves the handler chain.
// If a pre-middleware handler aborts, no routing occurs and the request is
// considered handled. Must be called before Start.
func (s *Server) Pre(middleware ...HandlerFunc) *Server {
	s.preMiddleware = append(s.preMiddleware, middleware...)
	return s
}

// Handle registers a handler for the given HTTP method and path pattern.
// Use this for non-standard methods (e.g., WebDAV PROPFIND) or when the
// method is determined at runtime.
func (s *Server) Handle(method, path string, handlers ...HandlerFunc) *Route {
	return s.handle(method, path, handlers...)
}

// GET registers a handler for GET requests.
func (s *Server) GET(path string, handlers ...HandlerFunc) *Route {
	return s.handle("GET", path, handlers...)
}

// POST registers a handler for POST requests.
func (s *Server) POST(path string, handlers ...HandlerFunc) *Route {
	return s.handle("POST", path, handlers...)
}

// PUT registers a handler for PUT requests.
func (s *Server) PUT(path string, handlers ...HandlerFunc) *Route {
	return s.handle("PUT", path, handlers...)
}

// DELETE registers a handler for DELETE requests.
func (s *Server) DELETE(path string, handlers ...HandlerFunc) *Route {
	return s.handle("DELETE", path, handlers...)
}

// PATCH registers a handler for PATCH requests.
func (s *Server) PATCH(path string, handlers ...HandlerFunc) *Route {
	return s.handle("PATCH", path, handlers...)
}

// HEAD registers a handler for HEAD requests.
func (s *Server) HEAD(path string, handlers ...HandlerFunc) *Route {
	return s.handle("HEAD", path, handlers...)
}

// OPTIONS registers a handler for OPTIONS requests.
func (s *Server) OPTIONS(path string, handlers ...HandlerFunc) *Route {
	return s.handle("OPTIONS", path, handlers...)
}

// Any registers a handler for all HTTP methods, returning the [Route] for each.
func (s *Server) Any(path string, handlers ...HandlerFunc) []*Route {
	methods := []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"}
	routes := make([]*Route, len(methods))
	for i, method := range methods {
		routes[i] = s.handle(method, path, handlers...)
	}
	return routes
}

// NotFound registers a custom handler for requests that do not match any route.
func (s *Server) NotFound(handler HandlerFunc) *Server {
	s.notFoundHandler = handler
	return s
}

// MethodNotAllowed registers a custom handler for requests where the path matches
// but the HTTP method does not. The Allow header is set automatically.
func (s *Server) MethodNotAllowed(handler HandlerFunc) *Server {
	s.methodNotAllowedHandler = handler
	return s
}

// OnError registers a global error handler called when an unhandled error
// reaches the safety net after all middleware has had its chance. The handler
// should write a response. If it does not write, the default text/plain
// fallback applies. Must be called before Start.
func (s *Server) OnError(handler func(c *Context, err error)) *Server {
	s.errorHandler = handler
	return s
}

// OnShutdown registers a function to be called during Server.Shutdown.
// Hooks fire in registration order with the shutdown context. Must be
// called before Start.
func (s *Server) OnShutdown(fn func(ctx context.Context)) *Server {
	s.shutdownHooks = append(s.shutdownHooks, fn)
	return s
}

// Static registers a GET handler that serves files from root under the given
// prefix. Uses FileFromDir for path traversal protection.
//
//	s.Static("/assets", "./public")
func (s *Server) Static(prefix, root string) *Route {
	p := strings.TrimRight(prefix, "/") + "/*filepath"
	return s.GET(p, func(c *Context) error {
		return c.FileFromDir(root, c.Param("filepath"))
	})
}

// Routes returns information about all registered routes.
// Output is sorted by method, then path for deterministic results.
func (s *Server) Routes() []RouteInfo {
	return s.router.walk()
}

// URL generates a URL for the named route by substituting positional parameters.
// Parameter values are substituted in order for :param segments. For *catchAll
// segments, the value replaces the wildcard (a leading "/" is de-duplicated).
// Values are inserted as-is without URL encoding — callers should encode if needed.
// Returns [ErrRouteNotFound] if no route with the given name exists.
func (s *Server) URL(name string, params ...string) (string, error) {
	s.router.namedMu.RLock()
	route, ok := s.router.namedRoutes[name]
	s.router.namedMu.RUnlock()
	if !ok {
		return "", ErrRouteNotFound
	}

	paramIdx := 0
	u, err := buildURL(name, route.path, func(_ string) (string, bool) {
		if paramIdx >= len(params) {
			return "", false
		}
		v := params[paramIdx]
		paramIdx++
		return v, true
	})
	if err != nil {
		return "", fmt.Errorf("celeris: not enough params for route %q", name)
	}
	if paramIdx != len(params) {
		return "", fmt.Errorf("celeris: too many params for route %q: expected %d, got %d", name, paramIdx, len(params))
	}
	return u, nil
}

// URLMap generates a URL for the named route by substituting named parameters
// from a map. This is an alternative to [Server.URL] that avoids positional
// ordering errors. Returns [ErrRouteNotFound] if no route with the given name
// has been registered.
func (s *Server) URLMap(name string, params map[string]string) (string, error) {
	s.router.namedMu.RLock()
	route, ok := s.router.namedRoutes[name]
	s.router.namedMu.RUnlock()
	if !ok {
		return "", ErrRouteNotFound
	}

	return buildURL(name, route.path, func(key string) (string, bool) {
		v, ok := params[key]
		return v, ok
	})
}

func buildURL(name, path string, lookup func(key string) (string, bool)) (string, error) {
	buf := make([]byte, 0, len(path))
	for i := 0; i < len(path); {
		switch path[i] {
		case ':':
			i++
			nameStart := i
			for i < len(path) && path[i] != '/' {
				i++
			}
			val, ok := lookup(path[nameStart:i])
			if !ok {
				return "", fmt.Errorf("celeris: missing param %q for route %q", path[nameStart:i], name)
			}
			buf = append(buf, val...)
		case '*':
			i++
			key := path[i:]
			val, ok := lookup(key)
			if !ok {
				return "", fmt.Errorf("celeris: missing param %q for route %q", key, name)
			}
			if len(val) == 0 {
				if len(buf) > 0 && buf[len(buf)-1] == '/' {
					buf = buf[:len(buf)-1]
				}
			} else if len(buf) > 0 && buf[len(buf)-1] == '/' && val[0] == '/' {
				val = val[1:]
			}
			buf = append(buf, val...)
			i = len(path)
		default:
			buf = append(buf, path[i])
			i++
		}
	}
	return string(buf), nil
}

// Group creates a new route group with the given prefix and middleware.
// Middleware provided here runs after server-level middleware but before route handlers.
func (s *Server) Group(prefix string, middleware ...HandlerFunc) *RouteGroup {
	return &RouteGroup{
		prefix:     prefix,
		middleware: middleware,
		server:     s,
	}
}

func (s *Server) prepare() (engine.Engine, error) {
	return s.doPrepare(nil)
}

// Start initializes and starts the server, blocking until Shutdown is called or
// the engine returns an error. Use StartWithContext for context-based lifecycle
// management.
//
// Returns ErrAlreadyStarted if called more than once. May also return
// configuration validation errors or engine initialization errors.
func (s *Server) Start() error {
	eng, err := s.prepare()
	if err != nil {
		return err
	}
	return eng.Listen(context.Background())
}

// Shutdown gracefully shuts down the server. Returns nil if the server has not
// been started. After the engine stops accepting new connections and drains
// in-flight requests, any hooks registered via [Server.OnShutdown] fire in
// registration order with the provided context.
func (s *Server) Shutdown(ctx context.Context) error {
	eng := s.loadEngine()
	if eng == nil {
		return nil
	}
	err := eng.Shutdown(ctx)
	for _, fn := range s.shutdownHooks {
		func() {
			defer func() { _ = recover() }()
			fn(ctx)
		}()
	}
	return err
}

// Addr returns the listener's bound address, or nil if the server has not been
// started. Useful when listening on ":0" to discover the OS-assigned port.
func (s *Server) Addr() net.Addr {
	eng := s.loadEngine()
	if eng == nil {
		return nil
	}
	return eng.Addr()
}

// EventLoopProvider returns the engine's event-loop provider, or nil if the
// engine does not expose one (e.g. the std net/http fallback) or the server
// has not been started. The returned provider is shared with the HTTP path;
// drivers register their own file descriptors on it to colocate database or
// cache I/O on the same worker threads as HTTP requests.
func (s *Server) EventLoopProvider() engine.EventLoopProvider {
	eng := s.loadEngine()
	if eng == nil {
		return nil
	}
	if p, ok := eng.(engine.EventLoopProvider); ok {
		return p
	}
	return nil
}

// AsyncHandlers reports whether the server dispatches HTTP handlers to
// spawned goroutines (Config.AsyncHandlers). Celeris drivers opened via
// WithEngine(srv) consult this to auto-select their direct net.Conn path
// — direct I/O matches Go's netpoll model perfectly on the spawned
// handler G, whereas the mini-loop sync path is preferred when the
// caller runs on a LockOSThread'd worker.
//
// Only engines that actually implement async dispatch report true — even
// when Config.AsyncHandlers is set. Currently:
//
//   - Epoll: async dispatch implemented; returns true when configured.
//   - IOUring: async dispatch implemented; returns true when configured.
//   - Std (net/http fallback): always async natively (goroutine per
//     conn); returns true when configured.
//   - Adaptive: both targets (epoll, iouring) support async dispatch,
//     and direct-mode drivers use Go netpoll — no engine-registered
//     FDs, so the adaptive engine's hot-swap machinery is safe
//     around them. Returns true when configured; a switch while
//     direct-mode drivers are in-flight is a no-op for the drivers
//     (their net.TCPConn goroutines keep running regardless of which
//     sub-engine is active).
func (s *Server) AsyncHandlers() bool {
	if !s.config.AsyncHandlers {
		return false
	}
	switch s.config.Engine {
	case Epoll, IOUring, Adaptive, Std:
		return true
	default:
		return false
	}
}

// EngineInfo returns information about the running engine, or nil if not started.
func (s *Server) EngineInfo() *EngineInfo {
	eng := s.loadEngine()
	if eng == nil {
		return nil
	}
	return &EngineInfo{
		Type:    EngineType(eng.Type()),
		Metrics: eng.Metrics(),
	}
}

// PauseAccept stops accepting new connections. Existing connections continue
// to be served. Returns [ErrAcceptControlNotSupported] if the engine does not
// support accept control (e.g. std engine).
func (s *Server) PauseAccept() error {
	eng := s.loadEngine()
	if eng == nil {
		return ErrAcceptControlNotSupported
	}
	ac, ok := eng.(engine.AcceptController)
	if !ok {
		return ErrAcceptControlNotSupported
	}
	return ac.PauseAccept()
}

// ResumeAccept resumes accepting new connections after PauseAccept.
// Returns [ErrAcceptControlNotSupported] if the engine does not support
// accept control.
func (s *Server) ResumeAccept() error {
	eng := s.loadEngine()
	if eng == nil {
		return ErrAcceptControlNotSupported
	}
	ac, ok := eng.(engine.AcceptController)
	if !ok {
		return ErrAcceptControlNotSupported
	}
	return ac.ResumeAccept()
}

// Collector returns the metrics collector, or nil if the server has not been
// started or if Config.DisableMetrics is true.
func (s *Server) Collector() *observe.Collector {
	return s.collector
}

// logger returns the configured logger or slog.Default().
func (s *Server) logger() *slog.Logger {
	if s.config.Logger != nil {
		return s.config.Logger
	}
	return slog.Default()
}

func (s *Server) prepareWithListener(ln net.Listener) (engine.Engine, error) {
	return s.doPrepare(func(cfg *resource.Config) {
		cfg.Listener = ln
	})
}

// doPrepare is the shared implementation for prepare and prepareWithListener.
// The optional configureFn is called after defaults are applied but before
// validation, allowing callers to set fields like Listener.
func (s *Server) doPrepare(configureFn func(cfg *resource.Config)) (engine.Engine, error) {
	var eng engine.Engine
	s.startOnce.Do(func() {
		cfg := s.config.toResourceConfig().WithDefaults()
		if configureFn != nil {
			configureFn(&cfg)
		}
		if errs := cfg.Validate(); len(errs) > 0 {
			s.startErr = fmt.Errorf("config validation: %w", errs[0])
			return
		}

		for _, cidr := range s.config.TrustedProxies {
			_, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				ip := net.ParseIP(cidr)
				if ip == nil {
					s.startErr = fmt.Errorf("celeris: invalid TrustedProxies entry: %s", cidr)
					return
				}
				if ip4 := ip.To4(); ip4 != nil {
					_, ipNet, _ = net.ParseCIDR(ip4.String() + "/32")
				} else {
					_, ipNet, _ = net.ParseCIDR(ip.String() + "/128")
				}
			}
			s.trustedNets = append(s.trustedNets, ipNet)
		}

		ra := &routerAdapter{server: s}
		if s.notFoundHandler != nil {
			ra.notFoundChain = make([]HandlerFunc, 0, len(s.middleware)+1)
			ra.notFoundChain = append(ra.notFoundChain, s.middleware...)
			ra.notFoundChain = append(ra.notFoundChain, s.notFoundHandler)
		}
		if s.methodNotAllowedHandler != nil {
			ra.methodNotAllowedChain = make([]HandlerFunc, 0, len(s.middleware)+1)
			ra.methodNotAllowedChain = append(ra.methodNotAllowedChain, s.middleware...)
			ra.methodNotAllowedChain = append(ra.methodNotAllowedChain, s.methodNotAllowedHandler)
		}
		ra.errorHandler = s.errorHandler
		var handler stream.Handler = ra
		var err error
		eng, err = createEngine(cfg, handler)
		if err != nil {
			s.startErr = fmt.Errorf("create engine: %w", err)
			return
		}
		s.engineRef.Store(&eng)

		if s.collector != nil {
			s.collector.SetEngineMetricsFn(func() observe.EngineMetrics {
				return eng.Metrics()
			})
		}

		logger := cfg.Logger
		if logger == nil {
			logger = slog.Default()
		}
		addr := cfg.Addr
		msg := "celeris starting"
		if cfg.Listener != nil {
			addr = cfg.Listener.Addr().String()
			msg = "celeris starting with listener"
		}
		logger.Info(msg,
			"addr", addr,
			"engine", eng.Type().String(),
			"protocol", cfg.Protocol.String(),
		)
	})
	if s.startErr != nil {
		return nil, s.startErr
	}
	if eng == nil {
		return nil, ErrAlreadyStarted
	}
	return eng, nil
}

// StartWithListener starts the server using an existing [net.Listener].
// This enables zero-downtime restarts via socket inheritance (e.g., passing
// the listener FD to a child process via environment variable).
//
// Listener ownership: on the std engine, the supplied listener is used
// directly. On the native engines (epoll, io_uring), the address is
// extracted from ln and the listener is closed so the engine workers can
// rebind their own SO_REUSEPORT sockets bound to the same (host, port).
// In both cases, the caller must not Accept on or close the supplied
// listener after calling this function.
//
// Returns [ErrAlreadyStarted] if called more than once.
func (s *Server) StartWithListener(ln net.Listener) error {
	eng, err := s.prepareWithListener(ln)
	if err != nil {
		return err
	}
	return eng.Listen(context.Background())
}

// StartWithListenerAndContext combines [Server.StartWithListener] and
// [Server.StartWithContext]. When the context is canceled, the server shuts
// down gracefully using [Config.ShutdownTimeout].
func (s *Server) StartWithListenerAndContext(ctx context.Context, ln net.Listener) error {
	eng, err := s.prepareWithListener(ln)
	if err != nil {
		return err
	}

	shutdownTimeout := s.config.ShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = 30 * time.Second
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = s.Shutdown(shutCtx)
	}()

	return eng.Listen(ctx)
}

// InheritListener returns a [net.Listener] from the file descriptor in the
// named environment variable. Returns nil, nil if the variable is not set.
// Used for zero-downtime restart patterns.
func InheritListener(envVar string) (net.Listener, error) {
	fdStr := os.Getenv(envVar)
	if fdStr == "" {
		return nil, nil
	}
	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		return nil, fmt.Errorf("celeris: invalid fd in %s: %w", envVar, err)
	}
	f := os.NewFile(uintptr(fd), "inherited-listener")
	if f == nil {
		return nil, fmt.Errorf("celeris: invalid fd %d", fd)
	}
	defer func() { _ = f.Close() }()
	return net.FileListener(f)
}

// StartWithContext starts the server with the given context for lifecycle management.
// When the context is canceled, the server shuts down gracefully using
// Config.ShutdownTimeout (default 30s).
//
// Returns ErrAlreadyStarted if called more than once. May also return
// configuration validation errors or engine initialization errors.
func (s *Server) StartWithContext(ctx context.Context) error {
	eng, err := s.prepare()
	if err != nil {
		return err
	}

	shutdownTimeout := s.config.ShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = 30 * time.Second
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = s.Shutdown(shutCtx)
	}()

	return eng.Listen(ctx)
}
