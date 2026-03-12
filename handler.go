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
	server *Server
}

func (a *routerAdapter) HandleStream(_ context.Context, s *stream.Stream) error {
	start := time.Now()

	c := acquireContext(s)

	if a.server.config.MaxFormSize != 0 {
		c.maxFormSize = a.server.config.MaxFormSize
	}

	if a.server.config.WriteTimeout > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), a.server.config.WriteTimeout)
		defer cancel()
		c.ctx = ctx
	}

	handlers, fullPath := a.server.router.find(c.method, c.path, &c.params)

	defer func() {
		if r := recover(); r != nil {
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
	}()

	if handlers == nil {
		a.handleUnmatched(c, s)
		return nil
	}

	c.handlers = handlers
	c.fullPath = fullPath

	a.handleError(c, s, c.Next())
	if c.buffered && !c.written {
		c.bufferDepth = 1
		_ = c.FlushResponse()
	}
	return nil
}

func (a *routerAdapter) handleUnmatched(c *Context, s *stream.Stream) {
	allowed := a.server.router.allowedMethods(c.path, c.method)
	if len(allowed) > 0 {
		c.statusCode = 405
		allowVal := strings.Join(allowed, ", ")
		if a.server.methodNotAllowedHandler != nil {
			c.SetHeader("allow", allowVal)
			c.handlers = []HandlerFunc{a.server.methodNotAllowedHandler}
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
		if a.server.notFoundHandler != nil {
			c.handlers = []HandlerFunc{a.server.notFoundHandler}
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
