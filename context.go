package celeris

import (
	"context"
	"math"
	"mime/multipart"
	"net/url"
	"strings"
	"sync"

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
}

// Context is the request context passed to handlers. It is pooled via sync.Pool.
// A Context is obtained from the pool and must not be retained after the handler returns.
type Context struct {
	stream   *stream.Stream
	index    int16
	handlers []HandlerFunc
	params   Params
	keys     map[string]any
	ctx      context.Context

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

	respHdrBuf [8][2]string // reusable buffer for response headers (avoids heap escape)
}

var contextPool = sync.Pool{New: func() any { return &Context{} }}

const abortIndex int16 = math.MaxInt16 / 2

func acquireContext(s *stream.Stream) *Context {
	var c *Context
	if s.CachedCtx != nil {
		c = s.CachedCtx.(*Context)
	} else {
		c = contextPool.Get().(*Context)
		// Cache on the stream for per-connection reuse (H1 keep-alive).
		// H2 streams are not cached, so CachedCtx stays nil.
		if s.CachedCtx == nil {
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

// Set stores a key-value pair for this request.
func (c *Context) Set(key string, value any) {
	if c.keys == nil {
		c.keys = make(map[string]any)
	}
	c.keys[key] = value
}

// Get returns the value for a key.
func (c *Context) Get(key string) (any, bool) {
	if c.keys == nil {
		return nil, false
	}
	v, ok := c.keys[key]
	return v, ok
}

// Keys returns a copy of all key-value pairs stored on this context.
// Returns nil if no values have been set.
func (c *Context) Keys() map[string]any {
	if c.keys == nil {
		return nil
	}
	cp := make(map[string]any, len(c.keys))
	for k, v := range c.keys {
		cp[k] = v
	}
	return cp
}

func (c *Context) reset() {
	c.stream = nil
	c.index = -1
	if cap(c.handlers) > 64 {
		c.handlers = nil
	} else {
		c.handlers = c.handlers[:0]
	}
	c.params = c.params[:0]
	c.keys = nil
	c.ctx = nil
	c.method = ""
	c.path = ""
	c.rawQuery = ""
	c.fullPath = ""
	c.statusCode = 200
	c.respHeaders = c.respHeaders[:0]
	c.written = false
	c.aborted = false
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
	c.maxFormSize = 0
	c.captureBody = false
	c.capturedBody = nil
	c.capturedStatus = 0
	c.capturedType = ""
	c.bufferDepth = 0
	c.buffered = false
	c.bytesWritten = 0
	c.detached = false
	c.detachDone = nil
}
