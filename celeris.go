package celeris

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/observe"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

// Version is the semantic version of the celeris module.
const Version = "1.0.0"

// ErrAlreadyStarted is returned when Start or StartWithContext is called on a
// server that is already running.
var ErrAlreadyStarted = errors.New("celeris: server already started")

// ErrRouteNotFound is returned by [Server.URL] when no route with the given
// name has been registered.
var ErrRouteNotFound = errors.New("celeris: named route not found")

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
	config     Config
	router     *router
	middleware []HandlerFunc
	engine     engine.Engine
	collector  *observe.Collector

	notFoundHandler         HandlerFunc
	methodNotAllowedHandler HandlerFunc
}

// New creates a Server with the given configuration.
func New(cfg Config) *Server {
	return &Server{
		config: cfg,
		router: newRouter(),
	}
}

func (s *Server) handle(method, path string, handlers ...HandlerFunc) *Route {
	chain := make([]HandlerFunc, 0, len(s.middleware)+len(handlers))
	chain = append(chain, s.middleware...)
	chain = append(chain, handlers...)
	return s.router.addRoute(method, path, chain)
}

// Use registers global middleware that runs before every route handler.
// Middleware executes in registration order. Middleware chains are composed
// at route registration time, so Use must be called before registering routes.
func (s *Server) Use(middleware ...HandlerFunc) *Server {
	s.middleware = append(s.middleware, middleware...)
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

// Any registers a handler for all HTTP methods.
func (s *Server) Any(path string, handlers ...HandlerFunc) *Server {
	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"} {
		s.handle(method, path, handlers...)
	}
	return s
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

// Routes returns information about all registered routes.
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

	path := route.path
	buf := make([]byte, 0, len(path))
	paramIdx := 0

	for i := 0; i < len(path); {
		switch path[i] {
		case ':':
			if paramIdx >= len(params) {
				return "", fmt.Errorf("celeris: not enough params for route %q", name)
			}
			buf = append(buf, params[paramIdx]...)
			paramIdx++
			i++
			for i < len(path) && path[i] != '/' {
				i++
			}
		case '*':
			if paramIdx >= len(params) {
				return "", fmt.Errorf("celeris: not enough params for route %q", name)
			}
			val := params[paramIdx]
			if len(val) == 0 {
				// Trim trailing slash when catchAll value is empty.
				if len(buf) > 0 && buf[len(buf)-1] == '/' {
					buf = buf[:len(buf)-1]
				}
			} else if len(buf) > 0 && buf[len(buf)-1] == '/' && val[0] == '/' {
				val = val[1:]
			}
			buf = append(buf, val...)
			paramIdx++
			i = len(path)
		default:
			buf = append(buf, path[i])
			i++
		}
	}

	if paramIdx != len(params) {
		return "", fmt.Errorf("celeris: too many params for route %q: expected %d, got %d", name, paramIdx, len(params))
	}

	return string(buf), nil
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

	path := route.path
	buf := make([]byte, 0, len(path))

	for i := 0; i < len(path); {
		switch path[i] {
		case ':':
			i++ // skip ':'
			nameStart := i
			for i < len(path) && path[i] != '/' {
				i++
			}
			key := path[nameStart:i]
			val, ok := params[key]
			if !ok {
				return "", fmt.Errorf("celeris: missing param %q for route %q", key, name)
			}
			buf = append(buf, val...)
		case '*':
			i++ // skip '*'
			key := path[i:]
			val, ok := params[key]
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
	if s.engine != nil {
		return nil, ErrAlreadyStarted
	}

	cfg := s.config.toResourceConfig().WithDefaults()
	if errs := cfg.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("config validation: %w", errs[0])
	}

	s.collector = observe.NewCollector()

	var handler stream.Handler = &routerAdapter{server: s}
	eng, err := createEngine(cfg, handler)
	if err != nil {
		return nil, fmt.Errorf("create engine: %w", err)
	}
	s.engine = eng

	s.collector.SetEngineMetricsFn(func() observe.EngineMetrics {
		return eng.Metrics()
	})

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("celeris starting",
		"addr", cfg.Addr,
		"engine", eng.Type().String(),
		"protocol", cfg.Protocol.String(),
	)

	return eng, nil
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
// been started.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.engine == nil {
		return nil
	}
	return s.engine.Shutdown(ctx)
}

// Addr returns the listener's bound address, or nil if the server has not been
// started. Useful when listening on ":0" to discover the OS-assigned port.
func (s *Server) Addr() net.Addr {
	if s.engine == nil {
		return nil
	}
	return s.engine.Addr()
}

// EngineInfo returns information about the running engine, or nil if not started.
func (s *Server) EngineInfo() *EngineInfo {
	if s.engine == nil {
		return nil
	}
	return &EngineInfo{
		Type:    EngineType(s.engine.Type()),
		Metrics: s.engine.Metrics(),
	}
}

// Collector returns the metrics collector, or nil if the server has not been started.
func (s *Server) Collector() *observe.Collector {
	return s.collector
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
	if shutdownTimeout == 0 {
		shutdownTimeout = 30 * time.Second
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = eng.Shutdown(shutCtx)
	}()

	return eng.Listen(ctx)
}
