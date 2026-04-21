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
	// internal/conn/h1.go needs to release cached contexts on connection close
	// but cannot import the root package (circular dependency). This single
	// hook is the only remaining ctxkit indirection.
	ctxkit.ReleaseContext = func(c any) {
		releaseContext(c.(*Context))
	}
}

// The AcquireTestContext / ReleaseTestContext / TestStream / SetTest*
// / AddTestParam helpers below are low-level building blocks for the
// celeristest package and other in-tree test infrastructure. End-user
// tests should reach for [celeristest.NewContext] /
// [celeristest.NewContextT] and the With* options instead — they
// build the Context+Stream pair, set up the recorder, and wire
// t.Cleanup automatically. These primitives are NOT marked Deprecated
// because celeristest itself depends on them; they are simply not
// the recommended API for new test code.

// AcquireTestContext returns a Context from the pool, bound to the given Stream.
// Prefer [celeristest.NewContext] for new code.
func AcquireTestContext(s *stream.Stream) *Context { return acquireContext(s) }

// ReleaseTestContext returns a Context to the pool, firing OnRelease callbacks.
// Prefer [celeristest.ReleaseContext] for new code.
func ReleaseTestContext(c *Context) { releaseContext(c) }

// TestStream returns the underlying stream, or nil. Test infrastructure only.
func TestStream(c *Context) *stream.Stream { return c.stream }

// SetTestStartTime sets the start time on a test context.
func SetTestStartTime(c *Context, t time.Time) { c.startTime = t }

// SetTestFullPath sets the full path on a test context.
// Prefer [celeristest.WithFullPath].
func SetTestFullPath(c *Context, path string) { c.fullPath = path }

// SetTestTrustedNets sets the trusted proxy networks on a test context.
// Prefer [celeristest.WithTrustedProxies].
func SetTestTrustedNets(c *Context, nets []*net.IPNet) { c.trustedNets = nets }

// AddTestParam appends a route parameter to a test context.
// Prefer [celeristest.WithParam].
func AddTestParam(c *Context, key, value string) {
	c.params = append(c.params, Param{Key: key, Value: value})
}

// SetTestHandlers installs a handler chain on a test context, using the
// inline handlerBuf when the chain is small enough.
// Prefer [celeristest.WithHandlers].
func SetTestHandlers(c *Context, handlers []HandlerFunc) {
	n := len(handlers)
	if n <= len(c.handlerBuf) {
		copy(c.handlerBuf[:n], handlers)
		c.handlers = c.handlerBuf[:n]
	} else {
		c.handlers = make([]HandlerFunc, n)
		copy(c.handlers, handlers)
	}
	c.index = -1
}

// SetTestScheme sets the scheme override on a test context.
// Prefer [celeristest.WithScheme].
func SetTestScheme(c *Context, scheme string) {
	c.extended = true
	c.schemeOverride = scheme
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
	streamWriter *StreamWriter

	detached   bool
	detachDone chan struct{}
	// detachSnap is allocated only when Detach() runs and stores the
	// status + elapsed snapshot captured by done() so the metrics
	// goroutine can read final values without racing late writes from
	// a handler that touches the Context after calling done()
	// (contract violation but seen in practice with deferred logger
	// blocks). Heap allocation is acceptable here because Detach is
	// the slow path; the alternative — embedding the fields directly —
	// grows every pooled Context by 24 bytes and pessimizes the H1/H2
	// hot path through cache pressure.
	detachSnap *detachSnapshot

	extended bool // true when keys/query/cookie/form/capture/buffer/detach/overrides were used

	// requestID is the canonical request ID, set by middleware/requestid
	// via [Context.SetRequestID]. Dedicated field so per-request writes
	// skip the any-interface boxing that Set(RequestIDKey, id) would cost.
	requestID string

	// stringKeys stores string-valued per-request data without the
	// any-interface boxing that c.Set would pay. Populated lazily on
	// first [Context.SetString]. Preferred storage for auth principals,
	// tenant IDs, trace correlators — any stringly-typed scalar that
	// middleware wants to share downstream without an alloc per request.
	stringKeys map[string]string

	clientIPOverride string
	schemeOverride   string
	hostOverride     string

	respHdrBuf [16][2]string // reusable buffer for response headers (avoids heap escape)

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

// capturedBodyMaxRetained caps the backing array size kept across
// Context pool cycles. Requests that produce larger bodies don't
// retain their buffer (avoiding pool-wide memory bloat from outliers).
const capturedBodyMaxRetained = 64 * 1024

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

// WorkerID returns the numeric ID of the event-loop worker handling this
// request, or -1 if no worker identity is available (std engine, tests
// constructing a Context without a Server, etc.). Handlers forwarding a
// DB or cache call through a celeris driver pool can pass this to the
// driver's WithWorker option to pin the pool conn to the same CPU,
// preserving per-request locality end-to-end:
//
//	import "github.com/goceleris/celeris/driver/postgres"
//
//	func handler(c *celeris.Context) error {
//	    ctx := postgres.WithWorker(c.Context(), c.WorkerID())
//	    row, err := pool.QueryContext(ctx, "SELECT ...")
//	    ...
//	}
//
// Epoll and io_uring engines populate this at accept time; the std
// engine (all platforms) returns -1 because Go's runtime scheduler
// picks the goroutine and there is no meaningful worker identity.
func (c *Context) WorkerID() int {
	id, _ := ctxkit.WorkerIDFrom(c.Context())
	return id
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
//
// Consults c.keys first (values set via [Context.Set]), then the
// dedicated string storage from [Context.SetString] and
// [Context.SetRequestID]. The any-interface boxing on fallback paths
// is re-done per Get call, so celeris-ecosystem code prefers the typed
// [Context.GetString] and [Context.RequestID] getters for zero-alloc
// reads.
func (c *Context) Get(key string) (any, bool) {
	if v, ok := c.keys[key]; ok {
		return v, ok
	}
	if c.stringKeys != nil {
		if s, ok := c.stringKeys[key]; ok {
			return s, true
		}
	}
	if key == RequestIDKey && c.requestID != "" {
		return c.requestID, true
	}
	return nil, false
}

// Keys returns a copy of all key-value pairs stored on this context.
// Returns nil if no values have been set.
func (c *Context) Keys() map[string]any {
	n := len(c.keys) + len(c.stringKeys)
	if c.requestID != "" {
		if _, inMap := c.stringKeys[RequestIDKey]; !inMap {
			if _, inKeys := c.keys[RequestIDKey]; !inKeys {
				n++
			}
		}
	}
	if n == 0 {
		return nil
	}
	cp := make(map[string]any, n)
	for k, v := range c.keys {
		cp[k] = v
	}
	for k, v := range c.stringKeys {
		if _, clash := cp[k]; !clash {
			cp[k] = v
		}
	}
	if c.requestID != "" {
		if _, clash := cp[RequestIDKey]; !clash {
			cp[RequestIDKey] = c.requestID
		}
	}
	return cp
}

// RequestID returns the canonical request ID for this context, set by
// middleware/requestid via [Context.SetRequestID]. Empty when no
// requestid middleware is installed or it skipped this request.
//
// Prefer this over Get(RequestIDKey) in celeris-ecosystem middleware:
// the underlying storage avoids the any-interface boxing that c.Get
// has to re-create on every call.
func (c *Context) RequestID() string { return c.requestID }

// SetRequestID stores the canonical request ID for this context. The
// value is exposed via both [Context.RequestID] (zero-alloc, preferred)
// and Get(RequestIDKey) (back-compat; re-boxes per call).
func (c *Context) SetRequestID(id string) {
	c.extended = true
	c.requestID = id
}

// SetString stores a string value on the context under key. Unlike
// [Context.Set], the value is kept in typed string storage — no
// any-interface boxing alloc per call.
//
// Reads: pair with [Context.GetString] for zero-alloc access. [Context.Get]
// falls back to the string storage, but re-boxes the string into an
// any on each call.
func (c *Context) SetString(key, value string) {
	c.extended = true
	if c.stringKeys == nil {
		c.stringKeys = make(map[string]string, 2)
	}
	c.stringKeys[key] = value
}

// GetString returns the string value stored under key by
// [Context.SetString], or ("", false) if absent. Also falls back to
// any-typed values written via [Context.Set] for back-compat.
func (c *Context) GetString(key string) (string, bool) {
	if c.stringKeys != nil {
		if s, ok := c.stringKeys[key]; ok {
			return s, true
		}
	}
	if v, ok := c.keys[key]; ok {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
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
	if n := len(c.params); n > 0 {
		clear(c.paramBuf[:n])
	}
	c.params = c.paramBuf[:0]
	c.ctx = nil
	c.method = ""
	c.path = ""
	c.rawQuery = ""
	c.fullPath = ""
	c.statusCode = 200
	if n := len(c.respHeaders); n > 0 {
		clear(c.respHdrBuf[:n])
	}
	c.respHeaders = c.respHdrBuf[:0]
	c.written = false
	c.aborted = false
	c.bytesWritten = 0
	c.maxFormSize = 0
	c.requestID = ""
	if c.extended {
		clear(c.keys)
		if c.stringKeys != nil {
			clear(c.stringKeys)
		}
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
		// Retain the capturedBody backing array across pool cycles so the
		// next Buffer/Capture request's append doesn't start from nil.
		// Cap retention at 64 KiB so an outlier response doesn't bloat
		// every pooled Context forever.
		if cap(c.capturedBody) > capturedBodyMaxRetained {
			c.capturedBody = nil
		} else {
			c.capturedBody = c.capturedBody[:0]
		}
		c.capturedStatus = 0
		c.capturedType = ""
		c.bufferDepth = 0
		c.buffered = false
		c.streamWriter = nil
		c.detached = false
		c.detachDone = nil
		c.detachSnap = nil
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

// detachSnapshot captures the response status and request elapsed time
// at the moment a detached handler signals completion via Detach's
// returned done(). Allocated lazily so non-detached requests pay no cost.
type detachSnapshot struct {
	status  int
	elapsed time.Duration
}
