package celeris

import (
	"context"
	"math"
	"mime/multipart"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

func init() {
	ctxkit.NewContext = func(s *stream.Stream) any {
		return acquireContext(s)
	}
	ctxkit.ReleaseContext = func(c any) {
		releaseContext(c.(*Context))
	}
	ctxkit.AddParam = func(c any, key, value string) {
		ctx := c.(*Context)
		ctx.params = append(ctx.params, Param{Key: key, Value: value})
	}
	ctxkit.SetHandlers = func(c any, handlers []any) {
		ctx := c.(*Context)
		n := len(handlers)
		var chain []HandlerFunc
		if n <= len(ctx.handlerBuf) {
			for i, h := range handlers {
				ctx.handlerBuf[i] = h.(HandlerFunc)
			}
			chain = ctx.handlerBuf[:n]
		} else {
			chain = make([]HandlerFunc, n)
			for i, h := range handlers {
				chain[i] = h.(HandlerFunc)
			}
		}
		ctx.handlers = chain
		ctx.index = -1
	}
	ctxkit.GetResponseWriter = func(c any) any {
		ctx := c.(*Context)
		if ctx.stream != nil {
			return ctx.stream.ResponseWriter
		}
		return nil
	}
	ctxkit.GetStream = func(c any) any {
		ctx := c.(*Context)
		if ctx.stream != nil {
			return ctx.stream
		}
		return nil
	}
	ctxkit.SetStartTime = func(c any, t time.Time) {
		c.(*Context).startTime = t
	}
}

// Context is the request context passed to handlers. It is pooled via sync.Pool.
// A Context is obtained from the pool and must not be retained after the handler returns.
type Context struct {
	stream     *stream.Stream
	index      int16
	handlers   []HandlerFunc
	handlerBuf [8]HandlerFunc
	params     Params
	paramBuf   [4]Param
	keys       map[string]any
	ctx        context.Context
	startTime  time.Time

	method   string
	path     string
	rawQuery string
	fullPath string

	statusCode  int
	respHeaders [][2]string
	written     bool
	aborted     bool

	queryCache  url.Values
	queryCached bool

	cookieCache  [][2]string
	cookieCached bool

	formParsed    bool
	formValues    url.Values
	multipartForm *multipart.Form
	maxFormSize   int64

	captureBody    bool
	capturedBody   []byte
	capturedStatus int
	capturedType   string

	bufferDepth  int
	buffered     bool
	bytesWritten int

	detached   bool
	detachDone chan struct{}

	extended bool // true when keys/query/cookie/form/capture/buffer/detach/overrides were used

	clientIPOverride string
	schemeOverride   string
	hostOverride     string

	respHdrBuf [8][2]string // reusable buffer for response headers (avoids heap escape)

	trustedNets []*net.IPNet

	onRelease    []func()
	onReleaseBuf [4]func()
}

var contextPool = sync.Pool{New: func() any {
	c := &Context{keys: make(map[string]any, 4)}
	c.params = c.paramBuf[:0]
	c.respHeaders = c.respHdrBuf[:0]
	c.onRelease = c.onReleaseBuf[:0]
	return c
}}

const abortIndex int16 = math.MaxInt16 / 2

// RequestIDKey is the canonical context store key for the request ID.
// Middleware that generates or reads request IDs should use this key
// with [Context.Set] and [Context.Get] for interoperability.
const RequestIDKey = "request_id"

func acquireContext(s *stream.Stream) *Context {
	var c *Context
	if s.CachedCtx != nil {
		c = s.CachedCtx.(*Context)
	} else {
		c = contextPool.Get().(*Context)
		// Cache on H1 streams for per-connection reuse (keep-alive).
		// H2 streams are ephemeral (released after one request), so caching
		// would leak the context — releaseContext skips pool.Put for cached
		// contexts, but stream.Release() nils CachedCtx, leaving the context
		// unreachable. H2 inline handlers use InlineCachedCtx instead.
		if s.IsH1() {
			s.CachedCtx = c
		}
	}
	c.stream = s
	c.index = -1
	c.statusCode = 200
	c.maxFormSize = DefaultMaxFormSize
	c.ctx = s.Context()
	c.extractRequestInfo()
	return c
}

func releaseContext(c *Context) {
	// If the context is cached on the stream for reuse, reset but
	// do not return to the pool. The stream owns its lifecycle.
	cached := c.stream != nil && c.stream.CachedCtx == c
	c.reset()
	if !cached {
		contextPool.Put(c)
	}
}

func (c *Context) extractRequestInfo() {
	headers := c.stream.Headers

	// Direct index access: H1 (populateCachedStream) always places
	// :method at [0] and :path at [1]. H2 (HPACK) usually follows the
	// same convention. Check the first 2 bytes of each name to verify
	// (":m" for :method, ":p" for :path) — this is faster than full
	// string comparison and covers all production pseudo-header names.
	if len(headers) >= 2 &&
		len(headers[0][0]) > 1 && headers[0][0][1] == 'm' &&
		len(headers[1][0]) > 1 && headers[1][0][1] == 'p' {
		c.method = headers[0][1]
		p := headers[1][1]
		if i := strings.IndexByte(p, '?'); i >= 0 {
			c.path = p[:i]
			c.rawQuery = p[i+1:]
		} else {
			c.path = p
		}
		return
	}

	// Fallback: scan all headers (non-standard ordering or malformed).
	for _, h := range headers {
		switch h[0] {
		case ":method":
			c.method = h[1]
		case ":path":
			p := h[1]
			if i := strings.IndexByte(p, '?'); i >= 0 {
				c.path = p[:i]
				c.rawQuery = p[i+1:]
			} else {
				c.path = p
			}
		}
	}
}

// Next executes the next handler in the chain. It returns the first non-nil
// error from a downstream handler, short-circuiting the remaining chain.
// Middleware can inspect or swallow errors by checking the return value.
func (c *Context) Next() error {
	c.index++
	for c.index < int16(len(c.handlers)) {
		if err := c.handlers[c.index](c); err != nil {
			return err
		}
		c.index++
	}
	return nil
}

// Abort prevents pending handlers from being called.
// Does not write a response. Use AbortWithStatus to abort and send a status code.
func (c *Context) Abort() {
	c.index = abortIndex
	c.aborted = true
}

// AbortWithStatus calls Abort and writes a status code with no body.
// It returns the error from NoContent for propagation.
func (c *Context) AbortWithStatus(code int) error {
	c.Abort()
	return c.NoContent(code)
}

// IsAborted returns true if the handler chain was aborted.
func (c *Context) IsAborted() bool {
	return c.aborted
}

// Context returns the request's context.Context. The returned context is
// always non-nil; it defaults to the stream's context.
func (c *Context) Context() context.Context {
	if c.ctx != nil {
		return c.ctx
	}
	return context.Background()
}

// SetContext sets the request's context. The provided ctx must be non-nil.
func (c *Context) SetContext(ctx context.Context) {
	c.ctx = ctx
}

// StartTime returns the time at which request processing began. Set once by
// the framework before the handler chain runs, so all middleware share the
// same timestamp without calling time.Now() independently.
func (c *Context) StartTime() time.Time { return c.startTime }

// Set stores a key-value pair for this request.
func (c *Context) Set(key string, value any) {
	c.extended = true
	c.keys[key] = value
}

// Get returns the value for a key.
func (c *Context) Get(key string) (any, bool) {
	v, ok := c.keys[key]
	return v, ok
}

// Keys returns a copy of all key-value pairs stored on this context.
// Returns nil if no values have been set.
func (c *Context) Keys() map[string]any {
	if len(c.keys) == 0 {
		return nil
	}
	cp := make(map[string]any, len(c.keys))
	for k, v := range c.keys {
		cp[k] = v
	}
	return cp
}

// OnRelease registers a function to be called when this Context is released
// back to the pool. Callbacks fire in LIFO order (like defer), before fields
// are cleared, so context state is still accessible. Panics in callbacks are
// recovered and silently discarded.
func (c *Context) OnRelease(fn func()) {
	c.extended = true
	c.onRelease = append(c.onRelease, fn)
}

func (c *Context) reset() {
	for i := len(c.onRelease) - 1; i >= 0; i-- {
		func() {
			defer func() { _ = recover() }()
			c.onRelease[i]()
		}()
	}
	c.stream = nil
	c.trustedNets = nil
	c.index = -1
	// Clear handler references so closures can be GCed, but only when
	// the slice is owned by this context (backed by handlerBuf or a
	// SetHandlers allocation). The production path assigns the router's
	// pre-composed chain directly; nilling those would corrupt shared state.
	if len(c.handlers) > 0 && cap(c.handlers) <= len(c.handlerBuf) &&
		&c.handlers[:cap(c.handlers)][0] == &c.handlerBuf[0] {
		for i := range c.handlers {
			c.handlers[i] = nil
		}
	}
	if cap(c.handlers) > 64 {
		c.handlers = nil
	} else {
		c.handlers = c.handlers[:0]
	}
	clear(c.paramBuf[:])
	c.params = c.paramBuf[:0]
	c.ctx = nil
	c.method = ""
	c.path = ""
	c.rawQuery = ""
	c.fullPath = ""
	c.statusCode = 200
	clear(c.respHdrBuf[:])
	c.respHeaders = c.respHdrBuf[:0]
	c.written = false
	c.aborted = false
	c.bytesWritten = 0
	c.maxFormSize = 0
	if c.extended {
		clear(c.keys)
		c.queryCache = nil
		c.queryCached = false
		c.cookieCache = c.cookieCache[:0]
		c.cookieCached = false
		if c.multipartForm != nil {
			_ = c.multipartForm.RemoveAll()
			c.multipartForm = nil
		}
		c.formParsed = false
		c.formValues = nil
		c.captureBody = false
		c.capturedBody = nil
		c.capturedStatus = 0
		c.capturedType = ""
		c.bufferDepth = 0
		c.buffered = false
		c.detached = false
		c.detachDone = nil
		c.clientIPOverride = ""
		c.schemeOverride = ""
		c.hostOverride = ""
		for i := range c.onRelease {
			c.onRelease[i] = nil
		}
		c.onRelease = c.onReleaseBuf[:0]
		c.extended = false
	}
}
