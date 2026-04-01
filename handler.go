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
}

func (a *routerAdapter) HandleStream(_ context.Context, s *stream.Stream) error {
	var start time.Time
	if a.server.collector != nil {
		start = time.Now()
	}

	c := acquireContext(s)

	if a.server.config.MaxFormSize != 0 {
		c.maxFormSize = a.server.config.MaxFormSize
	}

	// WriteTimeout is enforced at the engine level via periodic timeout checks
	// (epoll/iouring) or http.Server.WriteTimeout (std), avoiding per-request
	// timer allocations.

	handlers, fullPath := a.server.router.find(c.method, c.path, &c.params)

	defer a.recoverAndRelease(c, s, start)

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
func (a *routerAdapter) recoverAndRelease(c *Context, s *stream.Stream, start time.Time) {
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
				a.server.collector.RecordRequest(time.Since(start), status)
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
		a.server.collector.RecordRequest(time.Since(start), status)
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
		_ = s.ResponseWriter.WriteResponse(s, 500, [][2]string{
			{"content-type", "text/plain"},
		}, []byte("Internal Server Error"))
		c.written = true
	}
}

func (a *routerAdapter) handleUnmatched(c *Context, s *stream.Stream) {
	allowed := a.server.router.allowedMethods(c.path, c.method)
	if len(allowed) > 0 {
		c.statusCode = 405
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
			_ = s.ResponseWriter.WriteResponse(s, 405, [][2]string{
				{"content-type", "text/plain"},
				{"allow", allowVal},
			}, []byte("405 Method Not Allowed"))
			c.written = true
		}
	} else {
		c.statusCode = 404
		chain := a.notFoundChain
		if chain == nil && a.server.notFoundHandler != nil {
			chain = []HandlerFunc{a.server.notFoundHandler}
		}
		if chain != nil {
			c.handlers = chain
			a.handleError(c, s, c.Next())
		}
		if !c.written && s.ResponseWriter != nil {
			_ = s.ResponseWriter.WriteResponse(s, 404, [][2]string{
				{"content-type", "text/plain"},
			}, []byte("404 Not Found"))
			c.written = true
		}
	}
}

func (a *routerAdapter) handleError(c *Context, s *stream.Stream, err error) {
	if err == nil || c.written {
		return
	}
	var he *HTTPError
	if errors.As(err, &he) {
		c.statusCode = he.Code
		if s.ResponseWriter != nil {
			_ = s.ResponseWriter.WriteResponse(s, he.Code, [][2]string{
				{"content-type", "text/plain"},
			}, []byte(he.Message))
			c.written = true
		}
	} else {
		c.statusCode = 500
		if s.ResponseWriter != nil {
			_ = s.ResponseWriter.WriteResponse(s, 500, [][2]string{
				{"content-type", "text/plain"},
			}, []byte("Internal Server Error"))
			c.written = true
		}
	}
}

var _ stream.Handler = (*routerAdapter)(nil)
