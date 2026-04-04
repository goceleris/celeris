package celeris

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/goceleris/celeris/internal/negotiate"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

var smallInts [1000]string

func init() {
	for i := range smallInts {
		smallInts[i] = strconv.Itoa(i)
	}
}

func itoa(n int) string {
	if uint(n) < uint(len(smallInts)) {
		return smallInts[n]
	}
	return strconv.Itoa(n)
}

type jsonState struct {
	buf bytes.Buffer
	enc *json.Encoder
}

var jsonEncPool = sync.Pool{New: func() any {
	s := &jsonState{}
	s.enc = json.NewEncoder(&s.buf)
	s.enc.SetEscapeHTML(false)
	return s
}}

// Status sets the response status code and returns the Context for chaining.
// Note: response methods (JSON, Blob, etc.) take their own status code
// parameter and do not read the value set by Status.
func (c *Context) Status(code int) *Context {
	c.statusCode = code
	return c
}

// JSON serializes v as JSON and writes it with the given status code.
// Returns ErrResponseWritten if a response has already been sent.
func (c *Context) JSON(code int, v any) error {
	js := jsonEncPool.Get().(*jsonState)
	js.buf.Reset()
	if err := js.enc.Encode(v); err != nil {
		jsonEncPool.Put(js)
		return err
	}
	b := js.buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	err := c.Blob(code, "application/json", b)
	jsonEncPool.Put(js)
	return err
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
	return c.Blob(code, "text/html; charset=utf-8", unsafe.Slice(unsafe.StringData(html), len(html)))
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
	return c.Blob(code, "text/plain", unsafe.Slice(unsafe.StringData(body), len(body)))
}

// Blob writes a response with the given content type and data.
// Returns ErrResponseWritten if a response has already been sent.
func (c *Context) Blob(code int, contentType string, data []byte) error {
	if c.written {
		return ErrResponseWritten
	}
	if c.bufferDepth > 0 {
		c.capturedBody = append(c.capturedBody[:0], data...)
		c.capturedStatus = code
		c.capturedType = contentType
		c.statusCode = code
		c.buffered = true
		return nil
	}
	c.statusCode = code
	c.written = true
	c.bytesWritten = len(data)
	if c.captureBody {
		c.capturedBody = make([]byte, len(data))
		copy(c.capturedBody, data)
		c.capturedStatus = code
		c.capturedType = contentType
	}
	nUser := len(c.respHeaders)
	total := nUser + 2
	var headers [][2]string
	if total <= len(c.respHdrBuf) {
		// respHeaders shares backing array with respHdrBuf — copy user
		// headers to a stack temporary before overwriting the buffer.
		// Max user headers in fast path: len(respHdrBuf) - 2 = 6.
		var tmp [6][2]string
		copy(tmp[:nUser], c.respHeaders)
		headers = c.respHdrBuf[:0:len(c.respHdrBuf)]
		headers = append(headers, [2]string{"content-type", stripCRLF(contentType)})
		headers = append(headers, [2]string{"content-length", itoa(len(data))})
		headers = append(headers, tmp[:nUser]...)
	} else {
		headers = make([][2]string, 0, total)
		headers = append(headers, [2]string{"content-type", stripCRLF(contentType)})
		headers = append(headers, [2]string{"content-length", itoa(len(data))})
		headers = append(headers, c.respHeaders...)
	}
	if c.stream.ResponseWriter != nil {
		return c.stream.ResponseWriter.WriteResponse(c.stream, code, headers, data)
	}
	return nil
}

// NoContent writes a response with no body.
// Returns ErrResponseWritten if a response has already been sent.
func (c *Context) NoContent(code int) error {
	if c.written {
		return ErrResponseWritten
	}
	if c.bufferDepth > 0 {
		c.capturedStatus = code
		c.capturedBody = nil
		c.capturedType = ""
		c.statusCode = code
		c.buffered = true
		return nil
	}
	c.statusCode = code
	c.written = true
	c.bytesWritten = 0
	if c.stream.ResponseWriter != nil {
		return c.stream.ResponseWriter.WriteResponse(c.stream, code, c.respHeaders, nil)
	}
	return nil
}

// SetHeader sets a response header, replacing any existing value for the key.
// Keys are lowercased for HTTP/2 compliance (RFC 7540 §8.1.2).
// CRLF characters are stripped to prevent header injection.
func (c *Context) SetHeader(key, value string) {
	// Inline fast path: most programmatic header keys are already lowercase
	// and clean. Scan for uppercase/CRLF and skip function call if clean (P8).
	k := key
	for i := range len(key) {
		b := key[i]
		if b >= 'A' && b <= 'Z' || b == '\r' || b == '\n' || b == 0 {
			k = sanitizeHeaderKey(key)
			break
		}
	}
	v := value
	for i := range len(value) {
		b := value[i]
		if b == '\r' || b == '\n' || b == 0 {
			v = stripCRLF(value)
			break
		}
	}
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
	k := key
	for i := range len(key) {
		b := key[i]
		if b >= 'A' && b <= 'Z' || b == '\r' || b == '\n' || b == 0 {
			k = sanitizeHeaderKey(key)
			break
		}
	}
	v := value
	for i := range len(value) {
		b := value[i]
		if b == '\r' || b == '\n' || b == 0 {
			v = stripCRLF(value)
			break
		}
	}
	c.respHeaders = append(c.respHeaders, [2]string{k, v})
}

// sanitizeHeaderKey lowercases and strips CRLF/null bytes. Fast path avoids
// allocation when the key is already lowercase and clean (common case).
func sanitizeHeaderKey(s string) string {
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c == '\r' || c == '\n' || c == 0 {
			return stripCRLF(strings.ToLower(s))
		}
	}
	return s
}

var crlfReplacer = strings.NewReplacer("\r", "", "\n", "", "\x00", "")

// stripCRLF removes \r, \n, and \x00 to prevent HTTP response splitting
// (CWE-113) and null-byte header smuggling.
func stripCRLF(s string) string {
	if strings.ContainsAny(s, "\r\n\x00") {
		return crlfReplacer.Replace(s)
	}
	return s
}

// ResponseHeaders returns the response headers that have been set so far.
// The returned slice must not be modified. Use SetHeader or AddHeader to
// change response headers.
func (c *Context) ResponseHeaders() [][2]string {
	return c.respHeaders
}

// StatusCode returns the response status code set by the handler.
func (c *Context) StatusCode() int {
	return c.statusCode
}

// DeleteCookie appends a Set-Cookie header that instructs the client to delete
// the named cookie. The path must match the original cookie's path.
func (c *Context) DeleteCookie(name, path string) {
	c.SetCookie(&Cookie{Name: name, Path: path, MaxAge: -1})
}

// Redirect sends an HTTP redirect to the given URL with the specified status code.
// Returns [ErrInvalidRedirectCode] if code is not in the range 300–308.
// Returns [ErrResponseWritten] if a response has already been sent.
func (c *Context) Redirect(code int, url string) error {
	if code < 300 || code > 308 {
		return fmt.Errorf("%w: %d", ErrInvalidRedirectCode, code)
	}
	c.SetHeader("location", url)
	return c.NoContent(code)
}

var cookieUnsafeReplacer = strings.NewReplacer(";", "", "\r", "", "\n", "")

// stripCookieUnsafe strips characters that could inject cookie attributes
// (semicolons) or cause header injection (CRLF) from cookie field values.
func stripCookieUnsafe(s string) string {
	if strings.ContainsAny(s, ";\r\n") {
		return cookieUnsafeReplacer.Replace(s)
	}
	return s
}

// SetCookie appends a Set-Cookie header to the response.
// Cookie values are sent as-is without encoding (per RFC 6265).
// Callers are responsible for encoding values if needed.
// Semicolons in Name, Value, Path, and Domain are stripped to prevent
// cookie attribute injection.
func (c *Context) SetCookie(cookie *Cookie) {
	var b strings.Builder
	b.Grow(128)
	b.WriteString(stripCookieUnsafe(cookie.Name))
	b.WriteByte('=')
	b.WriteString(stripCookieUnsafe(cookie.Value))
	if cookie.Path != "" {
		b.WriteString("; Path=")
		b.WriteString(stripCookieUnsafe(cookie.Path))
	}
	if cookie.Domain != "" {
		b.WriteString("; Domain=")
		b.WriteString(stripCookieUnsafe(cookie.Domain))
	}
	if !cookie.Expires.IsZero() {
		b.WriteString("; Expires=")
		b.WriteString(cookie.Expires.UTC().Format(http.TimeFormat))
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

// File serves the named file. The content type is detected from the file
// extension. Supports Range requests for partial content (HTTP 206).
//
// The entire file is loaded into memory (capped at 100 MB). Returns
// [HTTPError] with status 413 if the file exceeds this limit. For large
// files, consider using StreamWriter instead.
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

	if size > int64(maxStreamBodySize) {
		return NewHTTPError(413, "file exceeds 100MB limit")
	}

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
// error is returned. Symlinks are resolved and rechecked to prevent a
// symlink under baseDir from escaping the directory boundary.
func (c *Context) FileFromDir(baseDir, userPath string) error {
	abs := filepath.Clean(filepath.Join(baseDir, filepath.FromSlash(userPath)))
	base := filepath.Clean(baseDir)
	if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
		return NewHTTPError(400, "invalid file path")
	}
	// Resolve symlinks and recheck prefix to prevent symlink escape.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	if resolved != resolvedBase && !strings.HasPrefix(resolved, resolvedBase+string(filepath.Separator)) {
		return NewHTTPError(400, "invalid file path")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return NewHTTPError(400, "invalid file path")
	}
	return c.File(resolved)
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
// response with the given status code and content type. Returns [HTTPError]
// with status 413 if the data exceeds 100 MB.
func (c *Context) Stream(code int, contentType string, r io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(r, int64(maxStreamBodySize)+1))
	if err != nil {
		return err
	}
	if len(data) > maxStreamBodySize {
		return NewHTTPError(413, "stream body exceeds 100MB limit")
	}
	return c.Blob(code, contentType, data)
}

// Negotiate inspects the Accept header and returns the best matching content type
// from the provided offers. Returns "" if no match. Supports quality values (q=).
func (c *Context) Negotiate(offers ...string) string {
	accept := c.Header("accept")
	if accept == "" {
		if len(offers) > 0 {
			return offers[0]
		}
		return ""
	}
	return negotiate.Accept(accept, offers)
}

// Respond writes the response in the format that best matches the Accept header.
// Supported types: application/json, application/xml, text/plain.
// Falls back to JSON if no match.
func (c *Context) Respond(code int, v any) error {
	best := c.Negotiate("application/json", "application/xml", "text/plain")
	switch best {
	case "application/xml":
		return c.XML(code, v)
	case "text/plain":
		return c.String(code, "%v", v)
	default:
		return c.JSON(code, v)
	}
}

// CaptureResponse enables response body capture for this request.
// After calling Next(), use ResponseBody() and ResponseContentType() to inspect.
// The response is written to the wire AND a copy is captured for inspection
// (ideal for loggers). Use BufferResponse to defer the wire write entirely.
func (c *Context) CaptureResponse() {
	c.extended = true
	c.captureBody = true
}

// ResponseBody returns the captured response body, or nil if capture was not
// enabled. Available after [Context.CaptureResponse] + c.Next(), or after
// [Context.BufferResponse] + a response method (JSON, Blob, etc.).
func (c *Context) ResponseBody() []byte { return c.capturedBody }

// ResponseContentType returns the captured Content-Type, or "" if not captured.
func (c *Context) ResponseContentType() string { return c.capturedType }

// BufferResponse instructs response methods (JSON, XML, Blob, NoContent, etc.)
// to capture the response instead of writing to the wire. Multiple middleware
// layers can call BufferResponse — responses are depth-tracked and only sent
// when the outermost layer calls FlushResponse.
func (c *Context) BufferResponse() {
	c.extended = true
	c.bufferDepth++
}

// FlushResponse sends the buffered response to the wire. Each call decrements
// the buffer depth; the actual write happens when depth reaches zero.
// Returns nil if nothing was buffered. Returns ErrResponseWritten if already sent.
// Calling FlushResponse without a prior BufferResponse is a safe no-op.
func (c *Context) FlushResponse() error {
	if c.written {
		return ErrResponseWritten
	}
	if !c.buffered {
		return nil
	}
	if c.bufferDepth > 0 {
		c.bufferDepth--
	}
	if c.bufferDepth > 0 {
		return nil
	}
	c.bufferDepth = 0
	c.buffered = false
	if c.capturedType == "" {
		return c.NoContent(c.capturedStatus)
	}
	return c.Blob(c.capturedStatus, c.capturedType, c.capturedBody)
}

// DiscardBufferedResponse decrements the buffer depth and clears any captured
// response data without writing to the wire. Used by timeout middleware to
// discard a stale buffered response before writing an error response.
func (c *Context) DiscardBufferedResponse() {
	if c.bufferDepth > 0 {
		c.bufferDepth--
	}
	c.buffered = false
	c.capturedBody = c.capturedBody[:0]
	c.capturedStatus = 0
	c.capturedType = ""
}

// SetResponseBody replaces the buffered response body. Only valid after
// BufferResponse + c.Next(). Used by transform middleware (compress, etc.).
func (c *Context) SetResponseBody(body []byte) {
	c.capturedBody = append(c.capturedBody[:0], body...)
}

// ResponseStatus returns the captured response status code.
func (c *Context) ResponseStatus() int { return c.capturedStatus }

// IsWritten returns true if a response has been written to the wire.
func (c *Context) IsWritten() bool { return c.written }

// BytesWritten returns the response body size in bytes, or 0 if no response
// was written. For NoContent responses, returns 0.
func (c *Context) BytesWritten() int { return c.bytesWritten }

// Hijack takes over the underlying TCP connection. After Hijack, the caller
// owns the connection and is responsible for closing it. Supported on all
// engines for HTTP/1.1 connections. HTTP/2 connections cannot be hijacked
// (multiplexed streams share a single TCP connection).
func (c *Context) Hijack() (net.Conn, error) {
	if c.written {
		return nil, errors.New("celeris: cannot hijack after response written")
	}
	h, ok := c.stream.ResponseWriter.(stream.Hijacker)
	if !ok {
		return nil, ErrHijackNotSupported
	}
	conn, err := h.Hijack(c.stream)
	if err != nil {
		return nil, err
	}
	c.written = true
	return conn, nil
}

// Detach removes the Context from the handler chain's lifecycle.
// After Detach, the Context will not be released when the handler returns.
// The caller MUST call the returned done function when finished with the Context —
// failure to do so permanently leaks the Context from the pool.
// This is required for streaming responses on native engines where the handler
// must return to free the event loop thread.
func (c *Context) Detach() (done func()) {
	if c.detached {
		return func() {} // already detached — return no-op done
	}
	// Materialize any unsafe string headers (zero-copy H1 headers backed by
	// the connection's read buffer) before the handler returns and the buffer
	// is reused for the next recv. Pseudo-header keys are string literals
	// (safe), but their values (:authority, :path for non-"/" paths, :method
	// for non-standard methods) may be UnsafeString backed by the buffer.
	for i, h := range c.stream.Headers {
		if len(h[0]) > 0 && h[0][0] == ':' {
			c.stream.Headers[i][1] = strings.Clone(h[1])
			continue
		}
		c.stream.Headers[i][0] = strings.Clone(h[0])
		c.stream.Headers[i][1] = strings.Clone(h[1])
	}
	// Also materialize extracted fields that may reference the buffer.
	c.method = strings.Clone(c.method)
	c.path = strings.Clone(c.path)
	c.rawQuery = strings.Clone(c.rawQuery)

	c.extended = true
	c.detached = true
	if c.stream.OnDetach != nil {
		c.stream.OnDetach()
	}
	ch := make(chan struct{})
	c.detachDone = ch
	return func() { close(ch) }
}

// StreamWriter provides incremental response writing. Obtained via [Context.StreamWriter].
type StreamWriter struct {
	streamer stream.Streamer
	stream   *stream.Stream
}

// WriteHeader sends the status line and headers. Must be called once before Write.
func (sw *StreamWriter) WriteHeader(status int, headers [][2]string) error {
	return sw.streamer.WriteHeader(sw.stream, status, headers)
}

// Write sends a chunk of the response body. May be called multiple times.
func (sw *StreamWriter) Write(data []byte) (int, error) {
	err := sw.streamer.Write(sw.stream, data)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// Flush ensures buffered data is sent to the network.
func (sw *StreamWriter) Flush() error {
	return sw.streamer.Flush(sw.stream)
}

// Close signals end of the response body.
func (sw *StreamWriter) Close() error {
	return sw.streamer.Close(sw.stream)
}

// StreamWriter returns a [StreamWriter] for incremental response writing.
// Returns nil if the engine does not support streaming.
// On native engines (epoll, io_uring), the caller must call [Context.Detach]
// before spawning a goroutine that uses the StreamWriter. Call Close() when done.
func (c *Context) StreamWriter() *StreamWriter {
	s, ok := c.stream.ResponseWriter.(stream.Streamer)
	if !ok {
		return nil
	}
	return &StreamWriter{streamer: s, stream: c.stream}
}

// Attachment sets the Content-Disposition header to "attachment" with the
// given filename, prompting the client to download the response.
func (c *Context) Attachment(filename string) {
	if filename != "" {
		c.SetHeader("content-disposition",
			fmt.Sprintf(`attachment; filename="%s"`, escapeQuotedString(filename)))
	} else {
		c.SetHeader("content-disposition", "attachment")
	}
}

// Inline sets the Content-Disposition header to "inline" with the given
// filename, suggesting the client display the content in-browser.
func (c *Context) Inline(filename string) {
	if filename != "" {
		c.SetHeader("content-disposition",
			fmt.Sprintf(`inline; filename="%s"`, escapeQuotedString(filename)))
	} else {
		c.SetHeader("content-disposition", "inline")
	}
}

// escapeQuotedString escapes backslash and double-quote for use in
// HTTP quoted-string values (RFC 2616 §2.2).
func escapeQuotedString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
