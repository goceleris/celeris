package celeris

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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

// ErrNoCookie is returned by Context.Cookie when the named cookie is not present.
var ErrNoCookie = errors.New("celeris: named cookie not present")

// ErrResponseWritten is returned when a response method is called after
// a response has already been written.
var ErrResponseWritten = errors.New("celeris: response already written")

// ErrEmptyBody is returned by Bind, BindJSON, and BindXML when the request
// body is empty.
var ErrEmptyBody = errors.New("celeris: empty request body")

// DefaultMaxFormSize is the default maximum memory used for multipart form
// parsing (32 MB), matching net/http.
const DefaultMaxFormSize int64 = 32 << 20

const maxStreamBodySize = 100 << 20 // 100MB

// SameSite controls the SameSite attribute of a cookie.
type SameSite int

const (
	// SameSiteDefaultMode leaves the SameSite attribute unset (browser default).
	SameSiteDefaultMode SameSite = iota
	// SameSiteLaxMode sets SameSite=Lax (cookies sent with top-level navigations).
	SameSiteLaxMode
	// SameSiteStrictMode sets SameSite=Strict (cookies sent only in first-party context).
	SameSiteStrictMode
	// SameSiteNoneMode sets SameSite=None (requires Secure; cookies sent in all contexts).
	SameSiteNoneMode
)

// String returns the SameSite attribute value ("Lax", "Strict", "None", or "").
func (s SameSite) String() string {
	switch s {
	case SameSiteLaxMode:
		return "Lax"
	case SameSiteStrictMode:
		return "Strict"
	case SameSiteNoneMode:
		return "None"
	default:
		return ""
	}
}

// Cookie represents an HTTP cookie for use with Context.SetCookie.
type Cookie struct {
	// Name is the cookie name.
	Name string
	// Value is the cookie value.
	Value string
	// Path limits the scope of the cookie to the given URL path.
	Path string
	// Domain limits the scope of the cookie to the given domain.
	Domain string
	// MaxAge=0 means no Max-Age attribute is sent. Negative value means
	// delete the cookie (Max-Age=0 in the header).
	MaxAge int
	// Secure flags the cookie for HTTPS-only transmission.
	Secure bool
	// HTTPOnly prevents client-side scripts from accessing the cookie.
	HTTPOnly bool
	// SameSite controls cross-site request cookie behavior.
	SameSite SameSite
}

// HandlerFunc defines the handler used by middleware and routes.
// Returning a non-nil error propagates it up through the middleware chain.
// The routerAdapter safety net writes an appropriate response for unhandled errors.
type HandlerFunc func(*Context) error

// HTTPError is a structured error that carries an HTTP status code.
// Handlers return HTTPError to signal a specific status code to the
// routerAdapter safety net. Use NewHTTPError to create one.
type HTTPError struct {
	// Code is the HTTP status code (e.g. 400, 404, 500).
	Code int
	// Message is a human-readable error description sent in the response body.
	Message string
	// Err is an optional wrapped error for use with errors.Is / errors.As.
	Err error
}

// Error returns a string representation including the status code and message.
func (e *HTTPError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("code=%d, message=%s, err=%v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("code=%d, message=%s", e.Code, e.Message)
}

// Unwrap returns the wrapped error for use with errors.Is and errors.As.
func (e *HTTPError) Unwrap() error { return e.Err }

// WithError sets the wrapped error and returns the HTTPError for chaining.
func (e *HTTPError) WithError(err error) *HTTPError {
	e.Err = err
	return e
}

// NewHTTPError creates an HTTPError with the given status code and message.
// To wrap an existing error, use the WithError method on the returned HTTPError.
func NewHTTPError(code int, message string) *HTTPError {
	return &HTTPError{Code: code, Message: message}
}

// Param is a single URL parameter consisting of a key and a value.
type Param struct {
	// Key is the parameter name from the route pattern (e.g. "id" from ":id").
	Key string
	// Value is the matched segment from the request path (e.g. "42").
	Value string
}

// Params is a slice of Param.
type Params []Param

// Get returns the value of the first Param matching the given key.
// Returns empty string and false if the key is not found.
func (ps Params) Get(key string) (string, bool) {
	for _, p := range ps {
		if p.Key == key {
			return p.Value, true
		}
	}
	return "", false
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

	formParsed    bool
	formValues    url.Values
	multipartForm *multipart.Form
	maxFormSize   int64
}

var contextPool = sync.Pool{New: func() any { return &Context{} }}

const abortIndex int16 = math.MaxInt16 / 2

func acquireContext(s *stream.Stream) *Context {
	c := contextPool.Get().(*Context)
	c.stream = s
	c.index = -1
	c.statusCode = 200
	c.maxFormSize = DefaultMaxFormSize
	c.ctx = s.Context()
	c.extractRequestInfo()
	return c
}

func releaseContext(c *Context) {
	c.reset()
	contextPool.Put(c)
}

func (c *Context) extractRequestInfo() {
	for _, h := range c.stream.Headers {
		switch h[0] {
		case ":method":
			c.method = h[1]
		case ":path":
			p := h[1]
			if i := indexOf(p, '?'); i >= 0 {
				c.path = p[:i]
				c.rawQuery = p[i+1:]
			} else {
				c.path = p
			}
		}
	}
}

func indexOf(s string, b byte) int {
	for i := range len(s) {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// Method returns the HTTP method.
func (c *Context) Method() string { return c.method }

// Path returns the request path without query string.
func (c *Context) Path() string { return c.path }

// FullPath returns the matched route pattern (e.g. "/users/:id").
// Returns empty string if no route was matched.
func (c *Context) FullPath() string { return c.fullPath }

// Header returns the value of the named request header.
func (c *Context) Header(key string) string {
	for _, h := range c.stream.Headers {
		if h[0] == key {
			return h[1]
		}
	}
	return ""
}

// Param returns the value of a URL parameter by name.
func (c *Context) Param(key string) string {
	v, _ := c.params.Get(key)
	return v
}

// ParamInt returns a URL parameter parsed as an int.
// Returns an error if the parameter is missing or not a valid integer.
func (c *Context) ParamInt(key string) (int, error) {
	v, ok := c.params.Get(key)
	if !ok {
		return 0, fmt.Errorf("celeris: param %q not found", key)
	}
	return strconv.Atoi(v)
}

// ParamInt64 returns a URL parameter parsed as an int64.
// Returns an error if the parameter is missing or not a valid integer.
func (c *Context) ParamInt64(key string) (int64, error) {
	v, ok := c.params.Get(key)
	if !ok {
		return 0, fmt.Errorf("celeris: param %q not found", key)
	}
	return strconv.ParseInt(v, 10, 64)
}

// Query returns the value of a query parameter by name. Results are cached
// so repeated calls for different keys do not re-parse the query string.
func (c *Context) Query(key string) string {
	if c.rawQuery == "" {
		return ""
	}
	if !c.queryCached {
		c.queryCache, _ = url.ParseQuery(c.rawQuery)
		c.queryCached = true
	}
	if c.queryCache == nil {
		return ""
	}
	return c.queryCache.Get(key)
}

// QueryDefault returns the value of a query parameter, or the default if absent.
func (c *Context) QueryDefault(key, defaultValue string) string {
	v := c.Query(key)
	if v == "" {
		return defaultValue
	}
	return v
}

// QueryInt returns a query parameter parsed as an int.
// Returns the provided default value if the key is absent or not a valid integer.
func (c *Context) QueryInt(key string, defaultValue int) int {
	v := c.Query(key)
	if v == "" {
		return defaultValue
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultValue
	}
	return n
}

// QueryValues returns all values for the given query parameter key.
// Returns nil if the key is not present.
func (c *Context) QueryValues(key string) []string {
	if !c.queryCached {
		c.queryCache, _ = url.ParseQuery(c.rawQuery)
		c.queryCached = true
	}
	return c.queryCache[key]
}

// QueryParams returns all query parameters as url.Values.
func (c *Context) QueryParams() url.Values {
	if !c.queryCached {
		c.queryCache, _ = url.ParseQuery(c.rawQuery)
		c.queryCached = true
	}
	return c.queryCache
}

// Body returns the raw request body.
// The returned slice must not be modified or retained after the handler returns.
func (c *Context) Body() []byte {
	return c.stream.GetData()
}

// Bind auto-detects the request body format from the Content-Type header
// and deserializes into v. Supports application/json (default) and
// application/xml.
func (c *Context) Bind(v any) error {
	body := c.Body()
	if len(body) == 0 {
		return ErrEmptyBody
	}
	ct := c.Header("content-type")
	switch {
	case strings.HasPrefix(ct, "application/xml"), strings.HasPrefix(ct, "text/xml"):
		return xml.Unmarshal(body, v)
	default:
		return json.Unmarshal(body, v)
	}
}

// BindJSON deserializes the JSON request body into v.
func (c *Context) BindJSON(v any) error {
	body := c.Body()
	if len(body) == 0 {
		return ErrEmptyBody
	}
	return json.Unmarshal(body, v)
}

// BindXML deserializes the XML request body into v.
func (c *Context) BindXML(v any) error {
	body := c.Body()
	if len(body) == 0 {
		return ErrEmptyBody
	}
	return xml.Unmarshal(body, v)
}

// Status sets the response status code and returns the Context for chaining.
func (c *Context) Status(code int) *Context {
	c.statusCode = code
	return c
}

// JSON serializes v as JSON and writes it with the given status code.
// Returns ErrResponseWritten if a response has already been sent.
func (c *Context) JSON(code int, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Blob(code, "application/json", data)
}

// XML serializes v as XML and writes it with the given status code.
// Returns ErrResponseWritten if a response has already been sent.
func (c *Context) XML(code int, v any) error {
	data, err := xml.Marshal(v)
	if err != nil {
		return err
	}
	return c.Blob(code, "application/xml", data)
}

// HTML writes an HTML response with the given status code.
// Returns ErrResponseWritten if a response has already been sent.
func (c *Context) HTML(code int, html string) error {
	return c.Blob(code, "text/html; charset=utf-8", []byte(html))
}

// String writes a formatted string response.
// Returns ErrResponseWritten if a response has already been sent.
func (c *Context) String(code int, format string, args ...any) error {
	var body string
	if len(args) > 0 {
		body = fmt.Sprintf(format, args...)
	} else {
		body = format
	}
	return c.Blob(code, "text/plain", []byte(body))
}

// Blob writes a response with the given content type and data.
func (c *Context) Blob(code int, contentType string, data []byte) error {
	if c.written {
		return ErrResponseWritten
	}
	c.statusCode = code
	c.written = true
	headers := make([][2]string, 0, len(c.respHeaders)+2)
	headers = append(headers, [2]string{"content-type", contentType})
	headers = append(headers, [2]string{"content-length", strconv.Itoa(len(data))})
	headers = append(headers, c.respHeaders...)
	if c.stream.ResponseWriter != nil {
		return c.stream.ResponseWriter.WriteResponse(c.stream, code, headers, data)
	}
	return nil
}

// NoContent writes a response with no body.
func (c *Context) NoContent(code int) error {
	if c.written {
		return ErrResponseWritten
	}
	c.statusCode = code
	c.written = true
	if c.stream.ResponseWriter != nil {
		return c.stream.ResponseWriter.WriteResponse(c.stream, code, c.respHeaders, nil)
	}
	return nil
}

// SetHeader sets a response header, replacing any existing value for the key.
// Keys are lowercased for HTTP/2 compliance (RFC 7540 §8.1.2).
// CRLF characters are stripped to prevent header injection.
func (c *Context) SetHeader(key, value string) {
	k := stripCRLF(strings.ToLower(key))
	v := stripCRLF(value)
	for i, h := range c.respHeaders {
		if h[0] == k {
			c.respHeaders[i][1] = v
			return
		}
	}
	c.respHeaders = append(c.respHeaders, [2]string{k, v})
}

// AddHeader appends a response header value. Unlike SetHeader, it does not
// replace existing values — use this for headers that allow multiple values
// (e.g. set-cookie).
func (c *Context) AddHeader(key, value string) {
	c.respHeaders = append(c.respHeaders, [2]string{
		stripCRLF(strings.ToLower(key)),
		stripCRLF(value),
	})
}

// stripCRLF removes \r and \n to prevent HTTP response splitting (CWE-113).
func stripCRLF(s string) string {
	if strings.ContainsAny(s, "\r\n") {
		return strings.NewReplacer("\r", "", "\n", "").Replace(s)
	}
	return s
}

// StatusCode returns the response status code set by the handler.
func (c *Context) StatusCode() int {
	return c.statusCode
}

// Redirect sends an HTTP redirect to the given URL with the specified status code.
func (c *Context) Redirect(code int, url string) error {
	if code < 300 || code > 308 {
		return fmt.Errorf("celeris: redirect status code must be 3xx, got %d", code)
	}
	c.SetHeader("location", url)
	return c.NoContent(code)
}

// Cookie returns the value of the named cookie from the request, or
// ErrNoCookie if not found.
func (c *Context) Cookie(name string) (string, error) {
	raw := c.Header("cookie")
	if raw == "" {
		return "", ErrNoCookie
	}
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		if part[:eq] == name {
			v, err := url.QueryUnescape(part[eq+1:])
			if err != nil {
				return part[eq+1:], nil
			}
			return v, nil
		}
	}
	return "", ErrNoCookie
}

// SetCookie appends a Set-Cookie header to the response.
func (c *Context) SetCookie(cookie *Cookie) {
	var b strings.Builder
	b.WriteString(cookie.Name)
	b.WriteByte('=')
	b.WriteString(url.QueryEscape(cookie.Value))
	if cookie.Path != "" {
		b.WriteString("; Path=")
		b.WriteString(cookie.Path)
	}
	if cookie.Domain != "" {
		b.WriteString("; Domain=")
		b.WriteString(cookie.Domain)
	}
	if cookie.MaxAge > 0 {
		b.WriteString("; Max-Age=")
		b.WriteString(strconv.Itoa(cookie.MaxAge))
	} else if cookie.MaxAge < 0 {
		b.WriteString("; Max-Age=0")
	}
	if cookie.HTTPOnly {
		b.WriteString("; HttpOnly")
	}
	if cookie.Secure {
		b.WriteString("; Secure")
	}
	switch cookie.SameSite {
	case SameSiteLaxMode:
		b.WriteString("; SameSite=Lax")
	case SameSiteStrictMode:
		b.WriteString("; SameSite=Strict")
	case SameSiteNoneMode:
		b.WriteString("; SameSite=None")
	}
	c.AddHeader("set-cookie", b.String())
}

// Scheme returns the request scheme ("http" or "https"). It checks the
// X-Forwarded-Proto header first (set by reverse proxies), then falls back
// to the :scheme pseudo-header from the original request.
// Returns "http" if neither source provides a value.
func (c *Context) Scheme() string {
	if proto := c.Header("x-forwarded-proto"); proto != "" {
		return strings.ToLower(strings.TrimSpace(proto))
	}
	if scheme := c.Header(":scheme"); scheme != "" {
		return scheme
	}
	return "http"
}

// ClientIP extracts the client IP from X-Forwarded-For or X-Real-Ip headers.
// Returns empty string if neither header is present.
// These headers can be spoofed by clients. In production behind a reverse proxy,
// ensure only trusted proxies set these headers.
func (c *Context) ClientIP() string {
	if xff := c.Header("x-forwarded-for"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xri := c.Header("x-real-ip"); xri != "" {
		return strings.TrimSpace(xri)
	}
	return ""
}

// BasicAuth extracts HTTP Basic Authentication credentials from the
// Authorization header. Returns the username, password, and true if valid
// credentials are present; otherwise returns zero values and false.
func (c *Context) BasicAuth() (username, password string, ok bool) {
	auth := c.Header("authorization")
	if auth == "" {
		return
	}
	const prefix = "Basic "
	if len(auth) < len(prefix) || auth[:len(prefix)] != prefix {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(auth[len(prefix):])
	if err != nil {
		return
	}
	s := string(decoded)
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return
	}
	return s[:colon], s[colon+1:], true
}

// FormValue returns the first value for the named form field.
// Parses the request body on first call (url-encoded or multipart).
func (c *Context) FormValue(name string) string {
	if err := c.parseForm(); err != nil {
		return ""
	}
	return c.formValues.Get(name)
}

// FormValueOk returns the first value for the named form field plus a boolean
// indicating whether the field was present. Unlike FormValue, callers can
// distinguish a missing field from an empty value.
func (c *Context) FormValueOk(name string) (string, bool) {
	if err := c.parseForm(); err != nil {
		return "", false
	}
	vs, ok := c.formValues[name]
	if !ok || len(vs) == 0 {
		return "", false
	}
	return vs[0], true
}

// FormValues returns all values for the named form field.
func (c *Context) FormValues(name string) []string {
	if err := c.parseForm(); err != nil {
		return nil
	}
	return c.formValues[name]
}

// FormFile returns the first file for the named form field.
// Returns an error if the request is not multipart or the field is missing.
func (c *Context) FormFile(name string) (multipart.File, *multipart.FileHeader, error) {
	if err := c.parseForm(); err != nil {
		return nil, nil, err
	}
	if c.multipartForm == nil {
		return nil, nil, errors.New("celeris: request is not multipart")
	}
	files := c.multipartForm.File[name]
	if len(files) == 0 {
		return nil, nil, errors.New("celeris: file not found")
	}
	f, err := files[0].Open()
	if err != nil {
		return nil, nil, err
	}
	return f, files[0], nil
}

// MultipartForm returns the parsed multipart form, including file uploads.
// Returns an error if the request is not multipart.
func (c *Context) MultipartForm() (*multipart.Form, error) {
	if err := c.parseForm(); err != nil {
		return nil, err
	}
	if c.multipartForm == nil {
		return nil, errors.New("celeris: request is not multipart")
	}
	return c.multipartForm, nil
}

func (c *Context) parseForm() error {
	if c.formParsed {
		return nil
	}
	c.formParsed = true
	c.formValues = make(url.Values) // always init before error paths
	ct := c.Header("content-type")
	mediaType, mparams, _ := mime.ParseMediaType(ct)
	body := c.Body()
	switch mediaType {
	case "multipart/form-data":
		boundary := mparams["boundary"]
		if boundary == "" {
			return errors.New("celeris: missing multipart boundary")
		}
		r := multipart.NewReader(bytes.NewReader(body), boundary)
		form, err := r.ReadForm(c.maxFormSize)
		if err != nil {
			return err
		}
		c.multipartForm = form
		for k, vs := range form.Value {
			c.formValues[k] = vs
		}
	case "application/x-www-form-urlencoded":
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			return err
		}
		c.formValues = vals
	}
	return nil
}

// File serves the named file. The content type is detected from the file
// extension. Supports Range requests for partial content (HTTP 206).
//
// Security: filePath is opened directly — callers MUST sanitize user-supplied
// paths (e.g. filepath.Clean + prefix check) to prevent directory traversal.
func (c *Context) File(filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		return err
	}
	size := stat.Size()

	ext := filepath.Ext(filePath)
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	c.SetHeader("accept-ranges", "bytes")

	if rng := c.Header("range"); rng != "" {
		if start, end, ok := parseRange(rng, size); ok {
			length := end - start + 1
			if _, err := f.Seek(start, io.SeekStart); err != nil {
				return err
			}
			data := make([]byte, length)
			if _, err := io.ReadFull(f, data); err != nil {
				return err
			}
			c.SetHeader("content-range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
			return c.Blob(206, contentType, data)
		}
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	return c.Blob(200, contentType, data)
}

// FileFromDir safely serves a file from within baseDir. The userPath is
// cleaned and joined with baseDir; if the result escapes baseDir, a 400
// error is returned. This prevents directory traversal when serving
// user-supplied paths.
func (c *Context) FileFromDir(baseDir, userPath string) error {
	abs := filepath.Clean(filepath.Join(baseDir, filepath.FromSlash(userPath)))
	base := filepath.Clean(baseDir)
	if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
		return NewHTTPError(400, "invalid file path")
	}
	return c.File(abs)
}

func parseRange(header string, size int64) (start, end int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, false
	}
	spec := header[len(prefix):]
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	if parts[0] == "" {
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		start = size - n
		end = size - 1
	} else {
		var err error
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		if parts[1] == "" {
			end = size - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return 0, 0, false
			}
		}
	}
	if start < 0 || start >= size || end < start || end >= size {
		return 0, 0, false
	}
	return start, end, true
}

// Stream reads all data from r (capped at 100 MB) and writes it as the
// response with the given status code and content type.
func (c *Context) Stream(code int, contentType string, r io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(r, int64(maxStreamBodySize)+1))
	if err != nil {
		return err
	}
	if len(data) > maxStreamBodySize {
		return errors.New("celeris: stream body exceeds 100MB limit")
	}
	return c.Blob(code, contentType, data)
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
	c.handlers = nil
	c.params = c.params[:0]
	c.keys = nil
	c.ctx = nil
	c.method = ""
	c.path = ""
	c.rawQuery = ""
	c.fullPath = ""
	c.statusCode = 200
	c.respHeaders = c.respHeaders[:0:0]
	c.written = false
	c.aborted = false
	c.queryCache = nil
	c.queryCached = false
	if c.multipartForm != nil {
		_ = c.multipartForm.RemoveAll()
		c.multipartForm = nil
	}
	c.formParsed = false
	c.formValues = nil
	c.maxFormSize = 0
}
