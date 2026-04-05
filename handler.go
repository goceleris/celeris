package celeris

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

type routerAdapter struct {
	server                *Server
	notFoundChain         []HandlerFunc
	methodNotAllowedChain []HandlerFunc
	errorHandler          func(*Context, error)
}

func (a *routerAdapter) HandleStream(_ context.Context, s *stream.Stream) error {
	c := acquireContext(s)
	c.startTime = time.Now()
	c.trustedNets = a.server.trustedNets

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
		if err := c.Next(); err != nil {
			a.handleError(c, s, err)
			if c.buffered && !c.written {
				c.bufferDepth = 1
				_ = c.FlushResponse()
			}
			return nil
		}
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

	handlers, fullPath := a.server.router.find(c.method, c.path, &c.params)

	if handlers == nil {
		a.handleUnmatched(c, s)
		return nil
	}

	c.handlers = handlers
	c.fullPath = fullPath

	if err := c.Next(); err != nil {
		a.handleError(c, s, err)
	}
	if c.buffered && !c.written {
		c.bufferDepth = 1
		_ = c.FlushResponse()
	}
	return nil
}

// recoverAndRelease handles panic recovery and context release. Extracted to a
// separate noinline function so that HandleStream's stack frame is not inflated
// by the deferred closure and debug.Stack() call (P5).
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
				status := c.statusCode
				if status == 0 {
					status = 200
				}
				a.server.collector.RecordRequest(time.Since(c.startTime), status)
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
		a.server.collector.RecordRequest(time.Since(c.startTime), status)
	}
	releaseContext(c)
}

// handlePanic logs the panic and writes a 500 response. Separated from
// recoverAndRelease so debug.Stack() only runs when a panic actually occurs.
//
//go:noinline
func (a *routerAdapter) handlePanic(c *Context, s *stream.Stream, r any) {
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

var _ stream.Handler = (*routerAdapter)(nil)
