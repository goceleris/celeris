package celeris

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/validation"
)

type routerAdapter struct {
	server                *Server
	notFoundChain         []HandlerFunc
	methodNotAllowedChain []HandlerFunc
	errorHandler          func(*Context, error)
}

func (a *routerAdapter) HandleStream(ctx context.Context, s *stream.Stream) error {
	c := acquireContext(s)
	// Prefer the engine's worker-local cached "now" (set on the stream
	// by populateCachedStream from H1State.NowNs) over a per-request
	// time.Now() vDSO. Falls back to time.Now() for synthetic / std-engine
	// streams that didn't go through populateCachedStream.
	if s.StartTimeNs != 0 {
		// Defer the time.Unix conversion: store the raw ns and only
		// materialize a time.Time when c.StartTime() is actually called
		// (rare — the per-request hot path only needs ns for the duration
		// computation in recoverAndRelease).
		c.startTimeNs = s.StartTimeNs
	} else {
		t := time.Now()
		c.startTime = t
		c.startTimeNs = t.UnixNano()
	}
	c.trustedNets = a.server.trustedNets

	// Propagate engine-supplied worker affinity into the celeris.Context.
	// Prefer the value stashed on the stream (set by the engine at accept
	// time on the per-conn cached H1State and copied to the stream by
	// populateCachedStream) — that's a direct field load. Fall back to
	// ctxkit for streams that didn't go through populateCachedStream
	// (synthetic test contexts, std engine path).
	if s.WorkerIDSet {
		c.workerID = s.WorkerID
		c.workerIDSet = true
	} else if ctx != nil {
		if wid, ok := ctxkit.WorkerIDFrom(ctx); ok {
			c.workerID = int32(wid)
			c.workerIDSet = true
		}
	}

	if a.server.config.MaxFormSize != 0 {
		c.maxFormSize = a.server.config.MaxFormSize
	}

	// WriteTimeout is enforced at the engine level via periodic timeout checks
	// (epoll/iouring) or http.Server.WriteTimeout (std), avoiding per-request
	// timer allocations.

	defer a.recoverAndRelease(c, s)

	// Run pre-routing middleware before route lookup. Pre-middleware may modify
	// c.method or c.path (e.g. URL rewriting, method override). If any
	// pre-middleware aborts, skip routing entirely.
	if len(a.server.preMiddleware) > 0 {
		c.handlers = a.server.preMiddleware
		c.index = -1
		// Error AND abort: error wins — handleError still runs, then we
		// flush any buffered body.
		if err := c.Next(); err != nil {
			a.handleError(c, s, err)
			if c.buffered && !c.written {
				c.bufferDepth = 1
				_ = c.FlushResponse()
			}
			return nil
		}
		// Pure abort (handler wrote a response and called Abort with no
		// error): skip routing and flush.
		if c.IsAborted() {
			if c.buffered && !c.written {
				c.bufferDepth = 1
				_ = c.FlushResponse()
			}
			return nil
		}
		// Reset for the actual handler chain.
		c.handlers = nil
		c.index = -1
	}

	// Per-connection route cache: keep-alive connections that hit the
	// same static method+path on every request can skip the static-route
	// map lookup. Only valid when the lookup produced no params (a fully
	// static route — dynamic routes need fresh params each time).
	//
	// strings.Clone on cache fill: c.method and c.path may alias the H1
	// recv buffer, which is reused on the next request. Cloning gives
	// the cache a stable backing array so the byte-wise compare on the
	// next request reads the right bytes. Allocates once per conn (per
	// route fill); amortized across the entire keep-alive session.
	var handlers []HandlerFunc
	var fullPath string
	if cached, ok := s.CachedRouteHandlers.([]HandlerFunc); ok &&
		s.CachedRouteMethod == c.method && s.CachedRoutePath == c.path {
		handlers = cached
		fullPath = s.CachedRouteFullPath
	} else {
		var routeAsync bool
		handlers, fullPath, routeAsync = a.server.router.find(c.method, c.path, &c.params)
		if handlers != nil && len(c.params) == 0 {
			s.CachedRouteMethod = strings.Clone(c.method)
			s.CachedRoutePath = strings.Clone(c.path)
			s.CachedRouteHandlers = handlers
			s.CachedRouteFullPath = fullPath
			s.CachedRouteAsync = routeAsync
		}
	}

	if handlers == nil {
		a.handleUnmatched(c, s)
		return nil
	}

	c.handlers = handlers
	c.fullPath = fullPath

	// celeris#356: an adaptive route (inherited the AsyncHandlers=true default)
	// runs INLINE here until observed to block. Time this inline run; if the
	// handler chain exceeds adaptivePromoteThreshold it is genuinely blocking,
	// so promote the route to async dispatch — future requests then run on a
	// goroutine instead of stalling the event-loop worker. Non-adaptive configs
	// (no AsyncHandlers default) hit the empty-map fast path and skip timing.
	rt := a.server.router
	if rt.adaptiveRoutes[fullPath] && !rt.isPromoted(fullPath) {
		start := time.Now()
		err := c.Next()
		rt.recordInlineRun(fullPath, time.Since(start) > adaptivePromoteThreshold)
		if err != nil {
			a.handleError(c, s, err)
		}
	} else if err := c.Next(); err != nil {
		a.handleError(c, s, err)
	}
	if c.buffered && !c.written {
		c.bufferDepth = 1
		_ = c.FlushResponse()
	}
	return nil
}

// adaptivePromoteThreshold is the inline handler duration that counts as a
// "slow" run for adaptive promotion (celeris#356). A non-blocking handler
// (route + middleware, no I/O) returns in single-digit microseconds; a blocking
// one (DB/cache round-trip) takes 100µs+. 50µs separates them with margin for
// GC/scheduling jitter.
const adaptivePromoteThreshold = 50 * time.Microsecond

// adaptivePromoteStreak is how many CONSECUTIVE slow inline runs promote an
// adaptive route to async. The consecutive requirement (a fast run resets the
// streak) makes a one-off cold start / GC pause harmless, while a handler that
// blocks on every request promotes within a handful of requests.
const adaptivePromoteStreak = 8

// recoverAndRelease handles panic recovery and context release. Extracted to a
// separate noinline function so that HandleStream's stack frame is not inflated
// by the deferred closure and debug.Stack() call (P5).
//
// Layering with middleware/recovery: this is the last-resort safety net.
// Panics from user handlers normally hit middleware/recovery (when
// installed) inside the chain, which converts them to errors before
// they reach this function. recover() here is for catastrophic cases
// where recovery middleware itself panics, isn't installed, or where
// pre-routing middleware panics outside the route chain. Custom panic
// handling (Sentry, structured 500 responses, etc.) belongs in
// middleware/recovery — this function's a.handlePanic is intentionally
// minimal.
//
//go:noinline
func (a *routerAdapter) recoverAndRelease(c *Context, s *stream.Stream) {
	if r := recover(); r != nil {
		a.handlePanic(c, s, r)
	}
	if c.detached {
		go func() {
			<-c.detachDone
			if a.server.collector != nil {
				// Read the snapshot captured by Detach's done() callback
				// to avoid racing late writes from a handler that touched
				// the Context after calling done().
				status := 200
				var elapsed time.Duration
				if snap := c.detachSnap; snap != nil {
					if snap.status != 0 {
						status = snap.status
					}
					elapsed = snap.elapsed
				}
				a.server.collector.RecordRequestSharded(uint32(c.workerID), elapsed, status)
			}
			releaseContext(c)
		}()
		return
	}
	if a.server.collector != nil {
		status := c.statusCode
		if status == 0 {
			status = 200
		}
		// Use the raw int64 ns. time.Since on a time.Unix-constructed
		// time.Time falls back to wall-clock subtraction; this saves the
		// detour through time.Time.Sub.
		duration := time.Duration(time.Now().UnixNano() - c.startTimeNs)
		a.server.collector.RecordRequestSharded(uint32(c.workerID), duration, status)
	}
	releaseContext(c)
}

// handlePanic logs the panic and writes a 500 response. Separated from
// recoverAndRelease so debug.Stack() only runs when a panic actually occurs.
//
//go:noinline
func (a *routerAdapter) handlePanic(c *Context, s *stream.Stream, r any) {
	// validation.RecordPanic is a no-op in production (zero-cost stub
	// from validation/disabled.go); under -tags=validation it bumps
	// PanicCount, which probatorium reads via the unix socket to
	// assert that no panics escape the recover safety net.
	validation.RecordPanic()
	a.server.logger().Error("handler panic recovered",
		"error", fmt.Sprint(r),
		"method", c.method,
		"path", c.path,
		"stack", string(debug.Stack()),
	)
	c.statusCode = 500
	if !c.written && s.ResponseWriter != nil {
		hdrs := make([][2]string, 0, len(c.respHeaders)+2)
		hdrs = append(hdrs, c.respHeaders...)
		hdrs = append(hdrs, [2]string{"content-type", "text/plain"})
		hdrs = append(hdrs, [2]string{"cache-control", "no-store"})
		_ = s.ResponseWriter.WriteResponse(s, 500, hdrs, []byte("Internal Server Error"))
		c.written = true
	}
}

func (a *routerAdapter) handleUnmatched(c *Context, s *stream.Stream) {
	allowed := a.server.router.allowedMethods(c.path, c.method)
	if len(allowed) > 0 {
		c.statusCode = 405
		c.fullPath = "<method-not-allowed>"
		allowVal := strings.Join(allowed, ", ")
		chain := a.methodNotAllowedChain
		if chain == nil && a.server.methodNotAllowedHandler != nil {
			chain = []HandlerFunc{a.server.methodNotAllowedHandler}
		}
		if chain != nil {
			c.SetHeader("allow", allowVal)
			c.handlers = chain
			a.handleError(c, s, c.Next())
		}
		if !c.written && s.ResponseWriter != nil {
			hdrs := make([][2]string, 0, len(c.respHeaders)+2)
			hdrs = append(hdrs, c.respHeaders...)
			hdrs = append(hdrs, [2]string{"content-type", "text/plain"})
			hdrs = append(hdrs, [2]string{"allow", allowVal})
			_ = s.ResponseWriter.WriteResponse(s, 405, hdrs, []byte("405 Method Not Allowed"))
			c.written = true
		}
	} else {
		c.statusCode = 404
		c.fullPath = "<unmatched>"
		chain := a.notFoundChain
		if chain == nil && a.server.notFoundHandler != nil {
			chain = []HandlerFunc{a.server.notFoundHandler}
		}
		if chain != nil {
			c.handlers = chain
			a.handleError(c, s, c.Next())
		}
		if !c.written && s.ResponseWriter != nil {
			hdrs := make([][2]string, 0, len(c.respHeaders)+1)
			hdrs = append(hdrs, c.respHeaders...)
			hdrs = append(hdrs, [2]string{"content-type", "text/plain"})
			_ = s.ResponseWriter.WriteResponse(s, 404, hdrs, []byte("404 Not Found"))
			c.written = true
		}
	}
}

func (a *routerAdapter) handleError(c *Context, s *stream.Stream, err error) {
	if err == nil || c.written {
		return
	}
	if a.errorHandler != nil {
		a.errorHandler(c, err)
		if c.written {
			return
		}
	}
	hdrs := make([][2]string, 0, len(c.respHeaders)+2)
	hdrs = append(hdrs, c.respHeaders...)
	hdrs = append(hdrs, [2]string{"content-type", "text/plain"})
	hdrs = append(hdrs, [2]string{"cache-control", "no-store"})
	var he *HTTPError
	if errors.As(err, &he) {
		c.statusCode = he.Code
		if s.ResponseWriter != nil {
			_ = s.ResponseWriter.WriteResponse(s, he.Code, hdrs, []byte(he.Message))
			c.written = true
		}
	} else {
		c.statusCode = 500
		if s.ResponseWriter != nil {
			_ = s.ResponseWriter.WriteResponse(s, 500, hdrs, []byte("Internal Server Error"))
			c.written = true
		}
	}
}

// RouteAsync reports whether the route matching method+path is configured
// for async dispatch. Implements stream.AsyncRouteResolver so the H2
// processor can choose inline vs. pooled handler execution per stream.
func (a *routerAdapter) RouteAsync(method, path string) bool {
	return a.server.router.routeAsync(method, path)
}

// HasAsyncRoutes reports whether any route opted into async dispatch.
func (a *routerAdapter) HasAsyncRoutes() bool {
	return a.server.router.hasAsyncRoutes()
}

// AsyncRouteCount returns the number of routes registered with .Async(true).
// Engines expose this through Metrics().AsyncRoutes for diagnostics.
func (a *routerAdapter) AsyncRouteCount() int {
	return a.server.router.asyncRouteCount
}

var (
	_ stream.Handler            = (*routerAdapter)(nil)
	_ stream.AsyncRouteResolver = (*routerAdapter)(nil)
)
