package celeris

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/internal/cpumon"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
	"github.com/goceleris/celeris/observe"
)

// Version is the semantic version of the celeris module.
const Version = "1.6.0"

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
	// cpuMon, when non-nil, supplies the adaptive engine's live sampler with
	// CPU utilization data and powers CPUUtilization in observe.Collector
	// snapshots. Created lazily in doPrepare and closed in Shutdown so
	// resources (e.g. the /proc/stat file descriptor on Linux) are released
	// when the server stops. cpuMonMu serializes the doPrepare write against
	// the Shutdown read/close: Start (doPrepare) and Shutdown run on
	// different goroutines and the sync.Once guarding doPrepare gives no
	// happens-before to Shutdown, so the field handoff must be locked.
	cpuMon   cpumon.Monitor
	cpuMonMu sync.Mutex

	// listenCancel cancels the context handed to Engine.Listen by the
	// Start* entry points; Shutdown calls it once the engine's graceful
	// phase is over. Without it Start/StartWithListener never return on
	// the native engines (celeris#595): iouring/epoll Listen parks on
	// <-ctx.Done() and their Engine.Shutdown is a documented no-op, so a
	// Listen handed context.Background() has nothing that can wake it.
	// shutdownCalled closes the Start/Shutdown race — a Shutdown that
	// lands before the cancel is published makes the next listen context
	// start out already cancelled instead of blocking forever.
	// lifecycleMu guards both; it is never held across Listen or across
	// the cancel call itself.
	lifecycleMu    sync.Mutex
	listenCancel   context.CancelFunc
	shutdownCalled bool

	// Who shuts the server down when a Start*Context call's context is
	// cancelled and the caller also calls Shutdown (celeris#673): the first
	// to claim it, under lifecycleMu, so the OnShutdown hooks never run
	// twice. directShutdown: a direct Shutdown call has begun, so the
	// watcher leaves the shutdown to it. watcherShutdown: the watcher has
	// claimed the shutdown, and is closed once it has run; a direct Shutdown
	// that finds it waits for that and returns watcherErr, written before
	// the close.
	directShutdown  bool
	watcherShutdown chan struct{}
	watcherErr      error
	// directShutdownDone is made, under lifecycleMu, by the first direct
	// Shutdown call before it does anything else, and closed when that call
	// returns. listen waits for it after Listen has returned, so a Start*
	// call that a direct Shutdown stopped returns only after that Shutdown,
	// OnShutdown hooks included, as it does after a cancel (celeris#703).
	directShutdownDone chan struct{}

	// listenDone is closed when the Listen call of the Start* entry point
	// that published the engine returns. Shutdown waits for it before it
	// runs the OnShutdown hooks (celeris#703), and the Start*Context watcher
	// wakes on it. What is over by then depends on the engine. On epoll and
	// io_uring the drain runs in Listen, which returns only after its
	// workers have closed their connections and joined async dispatch, so
	// every handler they run has returned; see Shutdown for what that does
	// not cover. On std, Listen returns when Serve does, as soon as
	// Engine.Shutdown has closed the listener, at the start of its drain;
	// Shutdown waits here only after its own Engine.Shutdown call, which is
	// that drain, has returned. On adaptive, Engine.Shutdown waits for the
	// adaptive Listen, which returns after its sub-engines' Listen calls.
	// publishEngine makes it together with the engine, so a Shutdown that
	// loads a non-nil engine always finds it: every published engine is
	// followed by exactly one Listen (see listen). Written once, before
	// engineRef is stored, and only read after engineRef is loaded non-nil,
	// so the atomic orders it.
	listenDone chan struct{}

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
	// Seed the router's per-route async resolution with the server-level
	// default so routes/groups that don't override inherit it.
	s.router.defaultAsync = cfg.AsyncHandlers
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
//
// Global middleware also runs for a request no route matches, before its 404
// or 405 answer (the [Server.NotFound] / [Server.MethodNotAllowed] handler,
// or the built-in one). So logger, metrics, otel and requestid see unmatched
// requests, an authentication middleware answers them as it answers routed
// ones, and a middleware that serves its own paths, such as swagger, pprof,
// debug, healthcheck, the metrics endpoint or static, answers them with no
// route registered for them. Group and route middleware do not run for an
// unmatched request. Under [Config.AsyncHandlers] an unmatched request is
// dispatched like a route inheriting that default, one decision for all
// unmatched requests ([Config.AsyncHandlers] says when a blocking one still
// runs inline). A middleware that serves its own paths answers them
// for every client its AuthFunc admits: pprof's and debug's default admits a
// loopback peer, which behind a reverse proxy on the same host is every
// client, and the metrics endpoint has no AuthFunc by default.
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
// If a pre-middleware handler aborts, or answers the request (writes the
// response, has it captured by a buffering middleware, or takes the
// connection over), no routing occurs and the request is considered handled.
// Must be called before Start.
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

// HEAD registers a handler for HEAD requests. A path without a HEAD route
// is answered by its GET route, if it has one: the GET handler runs (it sees
// Method() == "HEAD") and the engine sends its headers without the body
// (RFC 9110 §9.3.2). Register HEAD only to answer it differently.
func (s *Server) HEAD(path string, handlers ...HandlerFunc) *Route {
	return s.handle("HEAD", path, handlers...)
}

// OPTIONS registers a handler for OPTIONS requests. A path without an
// OPTIONS route that has any other route is answered automatically: 200 with
// an Allow header listing the methods the path answers (HEAD whenever it has
// GET, and OPTIONS) and Content-Length: 0 (RFC 9110 §9.3.7). The global
// middleware ([Server.Use]) runs first, so a CORS middleware answers its
// preflight; group and route middleware do not run for it.
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
// The global middleware ([Server.Use]) runs first, and the handler runs only if
// no middleware has answered the request. Without one, the answer is
// "404 Not Found" (text/plain), written after the global middleware the same
// way.
func (s *Server) NotFound(handler HandlerFunc) *Server {
	s.notFoundHandler = handler
	return s
}

// MethodNotAllowed registers a custom handler for requests where the path matches
// but the HTTP method does not. The Allow header is set automatically. The
// global middleware ([Server.Use]) runs first, and the handler runs only if no
// middleware has answered the request; without one, the answer is
// "405 Method Not Allowed" (text/plain) with the Allow header. HEAD to
// a path with a GET route, and OPTIONS to a path with any route, are answered
// (see [Server.HEAD], [Server.OPTIONS]) and never reach this handler.
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
//
// The hooks run before the Start* call that served the server returns,
// whatever shut it down: a Start* call waits for the direct [Server.Shutdown]
// call that stopped it, and [Server.StartWithContext] and
// [Server.StartWithListenerAndContext] wait for the Shutdown a cancel of their
// context triggers, hooks included (celeris#703). A hook must therefore not
// wait for that Start call to return, directly or through anything that
// happens only after it returns. The two would wait on each other: a hook
// that returns when its ctx is done ends the wait when that ctx is done
// ([Config.ShutdownTimeout] after a cancel), and a hook that ignores ctx never
// does.
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
// When [Server.Shutdown] stops it, Start returns only after that Shutdown call
// has returned, [Server.OnShutdown] hooks included, so a main that exits when
// Start returns does not cut the hooks short (celeris#703). A hook must
// therefore not wait for Start to return; see [Server.OnShutdown].
//
// Returns ErrAlreadyStarted if called more than once. May also return
// configuration validation errors or engine initialization errors.
func (s *Server) Start() error {
	eng, err := s.prepare()
	if err != nil {
		return err
	}
	return s.listen(context.Background(), eng)
}

// listen runs eng.Listen for every Start* entry point, under the context
// listenContext derives from parent, and closes listenDone when it returns.
// Then, if a direct Shutdown call has begun, it waits for that call to
// return, so the Start* call returns after the OnShutdown hooks on every
// engine (celeris#703). The defers run in reverse: listenDone is closed
// before the wait, and it is all that Shutdown waits for, so the two never
// wait on each other.
func (s *Server) listen(parent context.Context, eng engine.Engine) error {
	defer s.waitDirectShutdown()
	defer close(s.listenDone)
	ctx, cancel := s.listenContext(parent)
	defer cancel()
	return eng.Listen(ctx)
}

// waitDirectShutdown waits for the first direct Shutdown call to return, if
// one has begun. A hook that waits for the Start* call to return would wait
// for itself: see [Server.OnShutdown].
func (s *Server) waitDirectShutdown() {
	s.lifecycleMu.Lock()
	done := s.directShutdownDone
	s.lifecycleMu.Unlock()
	if done != nil {
		<-done
	}
}

// publishEngine installs eng as the running engine, together with the
// listenDone channel its Listen will close.
func (s *Server) publishEngine(eng engine.Engine) {
	s.listenDone = make(chan struct{})
	s.engineRef.Store(&eng)
}

// listenContext derives the context handed to Engine.Listen from parent and
// publishes its CancelFunc so Shutdown can unblock Listen (celeris#595). The
// derivation keeps caller-context semantics intact: cancelling parent still
// cancels Listen exactly as before, this only adds a second way to wake it.
// If Shutdown already ran (or is racing prepare), the returned context is
// already cancelled so Listen returns immediately instead of parking forever.
//
// The returned function cancels that context and releases what doPrepare
// opened for the run: the settle re-opener and the CPU monitor. Every Start*
// entry point defers it right after this call, so it runs once Listen has
// returned, and from then on the server never serves again (Start cannot be
// retried). Shutdown releases the same two, but a Start that ends without a
// Shutdown to come, because Listen failed or because Shutdown was called
// before Start, left the re-opener running and the monitor's /proc/stat
// descriptor open for the life of the process (celeris#737). Both releases
// are idempotent, so a Shutdown before or after it is harmless.
func (s *Server) listenContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	s.lifecycleMu.Lock()
	alreadyShutdown := s.shutdownCalled
	if !alreadyShutdown {
		s.listenCancel = cancel
	}
	s.lifecycleMu.Unlock()
	if alreadyShutdown {
		cancel()
	}
	return ctx, func() {
		cancel()
		s.router.stopSettleReopener()
		s.closeCPUMonitor()
	}
}

// cancelListen wakes a Listen parked on the context published by
// listenContext and latches the shut-down state for any Start racing us.
// Idempotent: context.CancelFunc is safe to call more than once.
func (s *Server) cancelListen() {
	s.lifecycleMu.Lock()
	s.shutdownCalled = true
	cancel := s.listenCancel
	s.listenCancel = nil
	s.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Shutdown gracefully shuts down the server. Returns nil if the server has not
// been started. After the engine stops accepting new connections and drains
// in-flight requests, any hooks registered via [Server.OnShutdown] fire in
// registration order with the provided context. The CPUMonitor owned by the
// Server is closed as part of shutdown.
//
// On every engine Shutdown waits for that drain, bounded by ctx: it runs the
// hooks, and returns, only once the Listen call of the Start* entry point has
// returned. If ctx is done first, the hooks still run, with ctx, and Shutdown
// returns ctx's error. Because it waits for the requests in flight, a handler
// that calls Shutdown and waits for it waits for itself until ctx is done:
// call it from another goroutine (celeris#703). The Start* call that the
// server was started with returns only after Shutdown has returned.
//
// The drain waits for every HTTP/1.1 request and every HTTP/2 stream. std
// and epoll accept no new connection once the shutdown has begun, and
// io_uring none while it waits for HTTP/2 handlers (its 250 ms send drain
// keeps accepting, celeris#595). An HTTP/2 stream on an async route (marked Async, or promoted to async under
// [Config.AsyncHandlers]) runs on the shared HTTP/2 worker pool: epoll,
// io_uring and adaptive send each HTTP/2 connection GOAWAY, refuse
// (REFUSED_STREAM) a stream its client opens after it, and serve the
// connection until those handlers have returned and their responses have
// gone out, response DATA waiting for the client's WINDOW_UPDATE included,
// while ctx is live (until its deadline or, for a ctx without one, until it
// is done, but no longer than [Config.WriteTimeout]) and never for less than
// 250 ms; std sends each h2c connection GOAWAY, then waits for its h2c
// streams' handlers, bounded by ctx (celeris#759, celeris#878). Once
// the handlers have returned, the native engines send what the sockets have
// not taken yet before they close the connections: epoll (and adaptive while
// it runs epoll) for as long, and never for less than 250 ms (celeris#760);
// io_uring for 250 ms (celeris#806).
//
// The listen context published by the Start* entry points is cancelled AFTER
// the engine's graceful phase, never before: on std, Engine.Shutdown IS the
// drain. Listen's own ctx.Done branch drains too, and a cancel of
// StartWithContext's context reaches it first; since celeris#753 the drain
// keeps the deadline of every Engine.Shutdown call whichever call started it,
// where Listen's used to win the drain's sync.Once with no budget at all. The
// shortest governs: a call whose own ctx is still live when another's ends the
// drain gets an error wrapping that call's ctx error (celeris#879).
// A Shutdown that arrives before the server ever started still latches the
// shut-down state, so a Start racing it returns instead of parking on a
// context nothing will ever cancel (celeris#595).
//
// When cancelling the context of [Server.StartWithContext] or
// [Server.StartWithListenerAndContext] has already started a shutdown,
// Shutdown does not run a second one: it waits for that one, hooks included,
// and returns its result, or ctx's error if ctx is done first.
func (s *Server) Shutdown(ctx context.Context) error {
	s.lifecycleMu.Lock()
	claimed := s.watcherShutdown
	var done chan struct{}
	if claimed == nil {
		s.directShutdown = true
		if s.directShutdownDone == nil {
			done = make(chan struct{})
			s.directShutdownDone = done
		}
	}
	s.lifecycleMu.Unlock()
	if claimed != nil {
		select {
		case <-claimed:
			return s.watcherErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if done != nil {
		// The Start* call this stops waits for it (see listen).
		defer close(done)
	}
	return s.shutdown(ctx)
}

// shutdown is Shutdown's body: it runs every time it is called.
func (s *Server) shutdown(ctx context.Context) error {
	// celeris#592: stop the settled-route re-opener so a shut-down server
	// leaves no goroutine behind. Idempotent, and a no-op if it never started.
	s.router.stopSettleReopener()
	eng := s.loadEngine()
	if eng == nil {
		s.cancelListen()
		s.closeCPUMonitor()
		return nil
	}
	err := eng.Shutdown(ctx)
	s.cancelListen()
	// celeris#703: the drain is over only when Listen has returned. On epoll
	// and io_uring Engine.Shutdown is a no-op and the drain runs in Listen
	// once cancelListen has cancelled its context, so without this wait the
	// hooks ran, and Shutdown returned, while requests were still being
	// handled. On std, Engine.Shutdown was the drain and Listen returned when
	// it closed the listener; on adaptive, Engine.Shutdown waited for its own
	// Listen. Either way Listen has returned, or returns at once. Deadlock
	// check: Listen waits for nothing that Shutdown holds or does after this
	// point. The Start*Context watcher that runs this on a cancel waits here
	// for Listen, never for the Start call. The Start call waits for the
	// watcher, and for a direct Shutdown, only after Listen has returned and
	// listenDone is closed (see listen).
	if werr := s.waitListen(ctx); err == nil {
		err = werr
	}
	s.closeCPUMonitor()
	for _, fn := range s.shutdownHooks {
		func() {
			defer func() { _ = recover() }()
			fn(ctx)
		}()
	}
	return err
}

// waitListen waits for listenDone, bounded by ctx, and returns ctx's error if
// ctx is done first. A server whose engine was installed without
// publishEngine (tests) has no listenDone and nothing to wait for.
func (s *Server) waitListen(ctx context.Context) error {
	done := s.listenDone
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// closeCPUMonitor releases the /proc/stat file descriptor on Linux. Idempotent
// and safe to call from Shutdown even if the monitor was never installed (e.g.
// the engine failed to start, or Start was never called).
func (s *Server) closeCPUMonitor() {
	s.cpuMonMu.Lock()
	defer s.cpuMonMu.Unlock()
	if s.cpuMon == nil {
		return
	}
	if c, ok := s.cpuMon.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	s.cpuMon = nil
}

// cpuMonAdapter wraps the internal cpumon.Monitor so it satisfies the
// public observe.CPUMonitor interface (Sample returns float64 instead of
// the internal CPUSample struct). Defined here rather than in observe/ to
// avoid a circular import (observe already imports nothing from celeris).
type cpuMonAdapter struct {
	m cpumon.Monitor
}

func (a cpuMonAdapter) Sample() (float64, error) {
	s, err := a.m.Sample()
	if err != nil {
		return 0, err
	}
	return s.Utilization, nil
}

func (a cpuMonAdapter) Close() error {
	if c, ok := a.m.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// newCPUMonitor returns a platform-appropriate CPU monitor. On Linux this
// reads /proc/stat; on other platforms it falls back to a runtime/metrics
// based estimator. Returns an error only if the platform-specific
// constructor itself fails (e.g. /proc/stat is unreadable on Linux); the
// caller should treat that as a non-fatal degradation and continue with
// a nil monitor.
func newCPUMonitor() (cpumon.Monitor, error) {
	return newPlatformCPUMonitor()
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
//
// It is for the celeris drivers: pass the server itself to
// [github.com/goceleris/celeris/driver/redis.WithEngine] or its postgres
// and memcached counterparts. The result's type is defined in an internal
// package, so code outside github.com/goceleris/celeris can pass it on but
// cannot name it.
// That type and its methods are not supported API until celeris#453
// defines a public engine interface; they may change in a minor release.
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
	// Report the EFFECTIVE async state, not the raw Config flag: the server
	// stands up async-dispatch infrastructure when the server-level default is
	// set OR any route opted in via .Async() (doPrepare flips the engine cfg the
	// same way via hasAsyncRoutes). WithEngine drivers consult this to pick their
	// netpoll-park fast path; keying on the raw config alone put a driver called
	// from a per-route-.Async() handler on the slower mini-loop path — the
	// documented footgun. NOTE: hasAsyncRoutes reflects routes registered so far,
	// so open WithEngine drivers AFTER registering .Async() routes (or set
	// Config.AsyncHandlers=true) to be order-independent.
	if !s.config.AsyncHandlers && !s.router.hasAsyncRoutes() {
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

// PauseAccept stops accepting new connections. Returns
// [ErrAcceptControlNotSupported] if the engine does not support accept
// control — the std and adaptive engines do not implement it.
//
// Connections that were already accepted continue to be served, and
// connections still waiting in the listen socket's accept queue are drained
// and served before that socket closes (celeris#663).
//
// Clients that completed their TCP handshake before the pause but had not
// sent their request yet are carried across it as well (celeris#662,
// celeris#675). TCP_DEFER_ACCEPT is on by default on the epoll and io_uring
// listen sockets, and while it is the kernel holds such a client outside
// the accept queue until about one second after its SYN. So the pause first
// clears the option on each listen socket and keeps it open, accepting and
// serving, for about 1.5 s, and only then closes it. PauseAccept blocks
// for that long, and the server keeps admitting new connections meanwhile.
//
// What a pause can still reset: a handshake still in flight when a listen
// socket closes, which is inherent to closing one, and a client whose
// retransmitted SYN-ACK is lost or goes unanswered on a lossy path. Set
// [Config.DisableDeferAccept] for a pause that closes the listen sockets at
// once; its documentation says what the option saves.
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

// Collector returns the metrics collector. It is created eagerly in New, so it
// is non-nil before Start is called; it returns nil only when
// Config.DisableMetrics is true.
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
	eng, err := s.doPrepare(func(cfg *resource.Config) {
		cfg.Listener = ln
	})
	// celeris#737: the caller handed ln over and may not close it, so a start
	// that fails before any engine runs closes it here. Otherwise it stays
	// bound, and the kernel keeps completing handshakes into a backlog that
	// nothing accepts. ErrAlreadyStarted is the exception: a server is
	// already running, and ln may be the very listener it serves on, so it
	// stays the caller's.
	if err != nil && !errors.Is(err, ErrAlreadyStarted) && ln != nil {
		_ = ln.Close()
	}
	return eng, err
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
			s.startErr = fmt.Errorf("config validation: %w", errors.Join(errs...))
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

		// Per-route async: when any route opted into async dispatch
		// (Route.Async / RouteGroup.Async), the engine must stand up the
		// async dispatch infrastructure even if the global default
		// (Config.AsyncHandlers) is sync — otherwise an async-marked
		// route (e.g. a DB handler) would run inline on the event-loop /
		// worker thread and block it. This only adjusts the resource
		// Config handed to the engine; the public Server.AsyncHandlers()
		// still reflects the user-supplied Config. The H2 processor
		// applies the per-route flag at finer (per-stream) granularity
		// via the AsyncRouteResolver regardless of this server-level
		// enable.
		if s.router.hasAsyncRoutes() {
			cfg.AsyncHandlers = true
		}

		ra := &routerAdapter{server: s}
		ra.buildUnmatchedChains()
		ra.optionsChain = newOptionsChain(s.middleware)
		ra.errorHandler = s.errorHandler
		var handler stream.Handler = ra

		// CPUMonitor: constructed eagerly here so the adaptive engine can
		// wire it into its live sampler (the sampler takes the monitor at
		// construction time). Stored on the Server so Shutdown can close it.
		// On any failure we fall back to a nil monitor — the live sampler
		// degrades to CPUUtilization=0 (no bias contribution).
		cpuMon, cpuMonErr := newCPUMonitor()
		if cpuMonErr != nil {
			cfg.Logger.Warn("CPUMonitor unavailable, bias will not fire", "err", cpuMonErr)
		}
		// Locked handoff: a concurrent Shutdown reads/closes s.cpuMon under
		// the same mutex. The monitor is fully constructed above (same
		// goroutine) before the lock publishes it, so Shutdown either sees
		// nil or a complete monitor — never a half-built one.
		s.cpuMonMu.Lock()
		s.cpuMon = cpuMon
		s.cpuMonMu.Unlock()

		// Optional process-global soft heap ceiling, applied before the
		// engine allocates its per-worker tables so the first GC target is
		// already in force during the connection ramp. Opt-in only
		// (Resources.MemoryLimitBytes>0) — see the field doc for the
		// library-global caveat. -1 (no limit) is the runtime default we
		// never override implicitly.
		if lim := cfg.Resources.MemoryLimitBytes; lim > 0 {
			debug.SetMemoryLimit(lim)
		}

		var err error
		eng, err = createEngine(cfg, handler, cpuMon)
		if err != nil {
			s.startErr = fmt.Errorf("create engine: %w", err)
			// celeris#737: only Shutdown closes the monitor, and a caller
			// whose Start failed has no reason to call it, so the
			// /proc/stat descriptor opened above would stay open.
			s.closeCPUMonitor()
			return
		}
		s.publishEngine(eng)

		// celeris#592: re-time settled adaptive routes. Settling is
		// otherwise terminal, so a route that settled while its backend was
		// fast and whose backend later turns slow runs inline on the engine
		// worker forever. The re-opener is a single per-server goroutine
		// (none when there are no adaptive routes) and adds nothing to the
		// request path; Shutdown stops it.
		//
		// Started only AFTER the engine exists and is published: a failed
		// createEngine returns from doPrepare with startErr set and no engine
		// ever stored, so the caller gets an error instead of a Server, never
		// calls Shutdown, and nothing would stop a ticker started earlier —
		// it would run until the process exits. Nothing can settle before the
		// engine serves a request, so the later start costs no coverage.
		s.router.startSettleReopener(adaptiveSettleTTL)

		if s.collector != nil {
			s.collector.SetEngineMetricsFn(func() observe.EngineMetrics {
				return eng.Metrics()
			})
			if cpuMon != nil {
				s.collector.SetCPUMonitor(cpuMonAdapter{cpuMon})
			}
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
// listener after calling this function. If the server fails to start before
// its engine runs (a configuration error or an engine that cannot be
// created), the listener is closed before the error is returned. The one
// exception is [ErrAlreadyStarted]: the server is already running, perhaps on
// that very listener, so this call leaves it to the caller.
//
// On the adaptive engine (the default on Linux) the listener goes to whichever
// sub-engine starts, and a later switch binds the second sub-engine to that
// same address; ln must therefore be a TCP listener, and the server keeps
// serving on ln's port across the switch. [Config.Addr] is ignored in favour
// of ln's address.
//
// As with [Server.Start], when [Server.Shutdown] stops it, StartWithListener
// returns only after that Shutdown call has returned, hooks included.
//
// Returns [ErrAlreadyStarted] if called more than once.
func (s *Server) StartWithListener(ln net.Listener) error {
	eng, err := s.prepareWithListener(ln)
	if err != nil {
		return err
	}
	return s.listen(context.Background(), eng)
}

// StartWithListenerAndContext combines [Server.StartWithListener] and
// [Server.StartWithContext]. When the context is canceled, the server shuts
// down gracefully using [Config.ShutdownTimeout], and this call returns once
// that shutdown, including the [Server.OnShutdown] hooks, has finished; so it
// does when a direct [Server.Shutdown] stops it. A hook must therefore not
// wait for this call to return; see [Server.OnShutdown].
func (s *Server) StartWithListenerAndContext(ctx context.Context, ln net.Listener) error {
	eng, err := s.prepareWithListener(ln)
	if err != nil {
		return err
	}
	return s.listenUntilCancelled(ctx, eng)
}

// listenUntilCancelled runs eng.Listen for StartWithContext and
// StartWithListenerAndContext. When ctx is cancelled it shuts the server down
// with Config.ShutdownTimeout, and it returns only after that Shutdown has
// returned.
//
// The Shutdown runs on a watcher goroutine that starts before Listen, not
// after Listen returns, because on std the drain's budget comes from
// Server.Shutdown: Listen's own ctx.Done branch drains with no deadline, and
// only the concurrent Server.Shutdown bounds it (see internal/engine/std Shutdown).
//
// celeris#673: the watcher used to choose between ctx.Done() and listenDone
// in a single select and to return without shutting down when it got
// listenDone. The context handed to Listen is derived from ctx, so a cancel
// also makes Listen return and close listenDone. When the watcher had not yet
// reached its select by then, both cases were ready and select picked one at
// random: half the time Shutdown never ran, so the OnShutdown hooks were
// skipped, the CPU monitor's descriptor stayed open and the settle re-opener
// ran for the life of the process. And when it did run, it ran after Start
// had already returned, so a caller that exits when Start returns could lose
// its hooks. The watcher now decides from state, not from which case select
// happened to take, and the caller waits for it.
func (s *Server) listenUntilCancelled(ctx context.Context, eng engine.Engine) error {
	shutdownTimeout := s.config.ShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = 30 * time.Second
	}
	listenDone := s.listenDone
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
		case <-listenDone:
		}
		// Shut down if the caller cancelled, whichever case woke us, unless
		// the caller has already called Server.Shutdown: running it a second
		// time would run every OnShutdown hook twice. Listen returning with
		// ctx still live means an error or a direct Shutdown, and either way
		// there is nothing for the watcher to do. The check and the claim
		// are one step under lifecycleMu, and a direct Shutdown that comes
		// after the claim waits for this one instead of running its own.
		if ctx.Err() == nil {
			return
		}
		s.lifecycleMu.Lock()
		if s.directShutdown || s.watcherShutdown != nil {
			s.lifecycleMu.Unlock()
			return
		}
		claimed := make(chan struct{})
		s.watcherShutdown = claimed
		s.lifecycleMu.Unlock()
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		s.watcherErr = s.shutdown(shutCtx)
		close(claimed)
	}()

	// The listen context is derived from the caller's ctx: cancelling ctx
	// still stops Listen, and a direct Server.Shutdown (without cancelling
	// ctx) can stop it too. listen closes listenDone when Listen returns.
	err := s.listen(ctx, eng)
	<-watcherDone
	return err
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
// Config.ShutdownTimeout (default 30s), and StartWithContext returns once that
// shutdown, including the [Server.OnShutdown] hooks, has finished; so it does
// when a direct [Server.Shutdown] stops it. A hook must therefore not wait for
// StartWithContext to return; see [Server.OnShutdown].
//
// Returns ErrAlreadyStarted if called more than once. May also return
// configuration validation errors or engine initialization errors.
func (s *Server) StartWithContext(ctx context.Context) error {
	eng, err := s.prepare()
	if err != nil {
		return err
	}
	return s.listenUntilCancelled(ctx, eng)
}
