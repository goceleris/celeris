package celeris

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/url"
	"strconv"
	"strings"
	"unsafe"

	"github.com/goceleris/celeris/internal/negotiate"
)

// Method returns the HTTP method.
func (c *Context) Method() string { return c.method }

// Path returns the request path without query string.
func (c *Context) Path() string { return c.path }

// SetPath overrides the request path. This is useful in middleware that
// rewrites URLs (e.g. prefix stripping) before downstream handlers see the path.
func (c *Context) SetPath(p string) { c.path = p }

// FullPath returns the matched route pattern (e.g. "/users/:id").
// Returns empty string if no route was matched.
func (c *Context) FullPath() string { return c.fullPath }

// RawQuery returns the raw query string without the leading '?'.
// Returns empty string if the URL has no query component.
func (c *Context) RawQuery() string { return c.rawQuery }

// SetRawQuery overrides the raw query string. Any cached query parameters
// from a previous call to Query/QueryValues/QueryParams are invalidated.
func (c *Context) SetRawQuery(q string) {
	c.rawQuery = q
	if c.queryCached {
		c.extended = true
		c.queryCache = nil
		c.queryCached = false
	}
}

// ContentLength returns the value of the Content-Length request header
// parsed as int64. Returns -1 if the header is absent or invalid.
func (c *Context) ContentLength() int64 {
	cl := c.Header("content-length")
	if cl == "" {
		return -1
	}
	n, err := strconv.ParseInt(cl, 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// Header returns the value of the named request header. Keys are normalized
// to lowercase automatically (HTTP/2 mandates lowercase; the H1 parser
// normalizes to lowercase).
func (c *Context) Header(key string) string {
	// Fast path: most programmatic keys are already lowercase.
	needsLower := false
	for i := range len(key) {
		if key[i] >= 'A' && key[i] <= 'Z' {
			needsLower = true
			break
		}
	}
	if needsLower {
		key = strings.ToLower(key)
	}
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

// ParamDefault returns the value of a URL parameter, or the default if absent or empty.
func (c *Context) ParamDefault(key, defaultValue string) string {
	v := c.Param(key)
	if v == "" {
		return defaultValue
	}
	return v
}

// Query returns the value of a query parameter by name. Results are cached
// so repeated calls for different keys do not re-parse the query string.
func (c *Context) Query(key string) string {
	if c.rawQuery == "" {
		return ""
	}
	if !c.queryCached {
		c.extended = true
		c.queryCache, _ = url.ParseQuery(c.rawQuery)
		c.queryCached = true
	}
	return c.queryCache.Get(key)
}

// QueryDefault returns the value of a query parameter, or the default if absent
// or empty.
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

// QueryInt64 returns a query parameter parsed as an int64.
// Returns the provided default value if the key is absent or not a valid integer.
func (c *Context) QueryInt64(key string, defaultValue int64) int64 {
	v := c.Query(key)
	if v == "" {
		return defaultValue
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return defaultValue
	}
	return n
}

// QueryBool returns a query parameter parsed as a bool.
// Returns the provided default value if the key is absent or not a valid bool.
// Recognizes "true", "1", "yes" as true and "false", "0", "no" as false.
func (c *Context) QueryBool(key string, defaultValue bool) bool {
	v := c.Query(key)
	if v == "" {
		return defaultValue
	}
	switch strings.ToLower(v) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	default:
		return defaultValue
	}
}

// QueryValues returns all values for the given query parameter key.
// Returns nil if the key is not present.
func (c *Context) QueryValues(key string) []string {
	if c.rawQuery == "" {
		return nil
	}
	if !c.queryCached {
		c.extended = true
		c.queryCache, _ = url.ParseQuery(c.rawQuery)
		c.queryCached = true
	}
	return c.queryCache[key]
}

// QueryParams returns all query parameters as url.Values.
func (c *Context) QueryParams() url.Values {
	if c.rawQuery == "" {
		return nil
	}
	if !c.queryCached {
		c.extended = true
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

// BodyCopy returns a copy of the request body that is safe to retain after
// the handler returns. Use this instead of Body() when the body must outlive
// the request lifecycle (e.g., for async processing or logging).
func (c *Context) BodyCopy() []byte {
	body := c.Body()
	if len(body) == 0 {
		return nil
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	return cp
}

// BodyReader returns an io.Reader for the request body. This wraps the
// already-received body bytes.
func (c *Context) BodyReader() io.Reader {
	return bytes.NewReader(c.Body())
}

// Bind auto-detects the request body format from the Content-Type header
// and deserializes into v. Supports application/json (default) and
// application/xml. Returns [ErrEmptyBody] if the body is empty, or the
// underlying encoding/json or encoding/xml error if deserialization fails.
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
// Returns [ErrEmptyBody] if the body is empty.
func (c *Context) BindJSON(v any) error {
	body := c.Body()
	if len(body) == 0 {
		return ErrEmptyBody
	}
	return json.Unmarshal(body, v)
}

// BindXML deserializes the XML request body into v.
// Returns [ErrEmptyBody] if the body is empty.
func (c *Context) BindXML(v any) error {
	body := c.Body()
	if len(body) == 0 {
		return ErrEmptyBody
	}
	return xml.Unmarshal(body, v)
}

// Cookie returns the value of the named cookie from the request, or
// ErrNoCookie if not found. Values are returned as-is without decoding.
func (c *Context) Cookie(name string) (string, error) {
	if !c.cookieCached {
		c.parseCookies()
	}
	for _, ck := range c.cookieCache {
		if ck[0] == name {
			return ck[1], nil
		}
	}
	return "", ErrNoCookie
}

func (c *Context) parseCookies() {
	c.extended = true
	c.cookieCached = true
	raw := c.Header("cookie")
	if raw == "" {
		return
	}
	for len(raw) > 0 {
		i := 0
		for i < len(raw) && (raw[i] == ' ' || raw[i] == ';') {
			i++
		}
		raw = raw[i:]
		if raw == "" {
			break
		}
		end := strings.IndexByte(raw, ';')
		var pair string
		if end < 0 {
			pair = raw
			raw = ""
		} else {
			pair = raw[:end]
			raw = raw[end+1:]
		}
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			continue
		}
		c.cookieCache = append(c.cookieCache, [2]string{pair[:eq], pair[eq+1:]})
	}
}

// Scheme returns the request scheme ("http" or "https"). If SetScheme has been
// called, the override value is returned. Otherwise it checks the
// X-Forwarded-Proto header first (set by reverse proxies), then falls back
// to the :scheme pseudo-header from the original request.
// Returns "http" if neither source provides a value.
func (c *Context) Scheme() string {
	if c.schemeOverride != "" {
		return c.schemeOverride
	}
	if proto := c.Header("x-forwarded-proto"); proto != "" {
		if proto == "https" || proto == "http" {
			return proto
		}
		return strings.ToLower(strings.TrimSpace(proto))
	}
	if scheme := c.Header(":scheme"); scheme != "" {
		return scheme
	}
	return "http"
}

// SetScheme overrides the value returned by Scheme. This is useful in
// middleware that determines the actual scheme from trusted proxy headers
// (e.g., X-Forwarded-Proto) and wants downstream handlers to see the
// canonical value without re-parsing headers.
func (c *Context) SetScheme(scheme string) {
	c.extended = true
	c.schemeOverride = strings.ToLower(strings.TrimSpace(scheme))
}

// ClientIP extracts the client IP. If SetClientIP has been called, the
// override value is returned. Otherwise it reads X-Forwarded-For or
// X-Real-Ip headers. Returns empty string if neither header is present.
// These headers can be spoofed by clients. In production behind a reverse proxy,
// ensure only trusted proxies set these headers.
func (c *Context) ClientIP() string {
	if c.clientIPOverride != "" {
		return c.clientIPOverride
	}
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

// SetClientIP overrides the value returned by ClientIP. This is useful in
// middleware that validates and extracts the real client IP from trusted proxy
// headers, so downstream handlers see the canonical IP.
func (c *Context) SetClientIP(ip string) {
	c.extended = true
	c.clientIPOverride = ip
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
	payload := auth[len(prefix):]
	var buf [128]byte
	if base64.StdEncoding.DecodedLen(len(payload)) > len(buf) {
		return
	}
	n, err := base64.StdEncoding.Decode(buf[:],
		unsafe.Slice(unsafe.StringData(payload), len(payload)))
	if err != nil {
		return
	}
	i := bytes.IndexByte(buf[:n], ':')
	if i < 0 {
		return
	}
	decoded := string(buf[:n])
	return decoded[:i], decoded[i+1:], true
}

// FormValue returns the first value for the named form field.
// Parses the request body on first call (url-encoded or multipart).
func (c *Context) FormValue(name string) string {
	if err := c.parseForm(); err != nil {
		return ""
	}
	return c.formValues.Get(name)
}

// FormValueOK returns the first value for the named form field plus a boolean
// indicating whether the field was present. Unlike FormValue, callers can
// distinguish a missing field from an empty value.
func (c *Context) FormValueOK(name string) (string, bool) {
	if err := c.parseForm(); err != nil {
		return "", false
	}
	vs, ok := c.formValues[name]
	if !ok || len(vs) == 0 {
		return "", false
	}
	return vs[0], true
}

// FormValueOk is a deprecated alias for [Context.FormValueOK].
//
// Deprecated: Use [Context.FormValueOK] instead.
func (c *Context) FormValueOk(name string) (string, bool) {
	return c.FormValueOK(name)
}

// FormValues returns all values for the named form field.
func (c *Context) FormValues(name string) []string {
	if err := c.parseForm(); err != nil {
		return nil
	}
	return c.formValues[name]
}

// FormFile returns the first file for the named form field.
// Returns [HTTPError] with status 400 if the request is not multipart or
// the field is missing.
func (c *Context) FormFile(name string) (multipart.File, *multipart.FileHeader, error) {
	if err := c.parseForm(); err != nil {
		return nil, nil, err
	}
	if c.multipartForm == nil {
		return nil, nil, NewHTTPError(400, "celeris: request is not multipart")
	}
	files := c.multipartForm.File[name]
	if len(files) == 0 {
		return nil, nil, NewHTTPError(400, "celeris: file not found")
	}
	f, err := files[0].Open()
	if err != nil {
		return nil, nil, err
	}
	return f, files[0], nil
}

// MultipartForm returns the parsed multipart form, including file uploads.
// Returns [HTTPError] with status 400 if the request is not multipart.
func (c *Context) MultipartForm() (*multipart.Form, error) {
	if err := c.parseForm(); err != nil {
		return nil, err
	}
	if c.multipartForm == nil {
		return nil, NewHTTPError(400, "celeris: request is not multipart")
	}
	return c.multipartForm, nil
}

func (c *Context) parseForm() error {
	if c.formParsed {
		return nil
	}
	c.extended = true
	c.formParsed = true
	c.formValues = make(url.Values) // always init before error paths
	ct := c.Header("content-type")
	mediaType, mparams, _ := mime.ParseMediaType(ct)
	body := c.Body()
	switch mediaType {
	case "multipart/form-data":
		boundary := mparams["boundary"]
		if boundary == "" {
			return NewHTTPError(400, "celeris: missing multipart boundary")
		}
		r := multipart.NewReader(bytes.NewReader(body), boundary)
		limit := c.maxFormSize
		if limit < 0 {
			limit = math.MaxInt64
		}
		form, err := r.ReadForm(limit)
		if err != nil {
			return NewHTTPError(400, "celeris: invalid multipart form").WithError(err)
		}
		c.multipartForm = form
		for k, vs := range form.Value {
			c.formValues[k] = vs
		}
	case "application/x-www-form-urlencoded":
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			return NewHTTPError(400, "celeris: invalid form data").WithError(err)
		}
		c.formValues = vals
	}
	return nil
}

// RequestHeaders returns all request headers as key-value pairs.
// The returned slice is a copy safe for concurrent use.
func (c *Context) RequestHeaders() [][2]string {
	return c.stream.GetHeaders()
}

// RemoteAddr returns the TCP peer address (e.g. "192.168.1.1:54321").
// Returns empty string if unavailable.
func (c *Context) RemoteAddr() string { return c.stream.RemoteAddr }

// Host returns the request host from the :authority pseudo-header (HTTP/2)
// or the Host header (HTTP/1.1). If SetHost has been called, the override
// value is returned instead.
func (c *Context) Host() string {
	if c.hostOverride != "" {
		return c.hostOverride
	}
	if h := c.Header(":authority"); h != "" {
		return h
	}
	return c.Header("host")
}

// SetHost overrides the value returned by Host. This is useful in middleware
// that normalizes the host (e.g., stripping port, applying X-Forwarded-Host).
func (c *Context) SetHost(host string) {
	c.extended = true
	c.hostOverride = host
}

// IsWebSocket returns true if the request is a WebSocket upgrade.
func (c *Context) IsWebSocket() bool {
	return strings.EqualFold(c.Header("upgrade"), "websocket")
}

// IsTLS returns true if the request was made over TLS.
func (c *Context) IsTLS() bool {
	return c.Scheme() == "https"
}

// Protocol returns the HTTP protocol version: "1.1" for HTTP/1.1 or "2"
// for HTTP/2. Values match the OTel network.protocol.version convention.
func (c *Context) Protocol() string {
	if c.stream.ProtoMajor() == 1 {
		return "1.1"
	}
	return "2"
}

// AcceptsEncodings returns the best matching encoding from the Accept-Encoding
// header, or empty string if none match.
func (c *Context) AcceptsEncodings(offers ...string) string {
	return negotiate.Accept(c.Header("accept-encoding"), offers)
}

// AcceptsLanguages returns the best matching language from the Accept-Language
// header, or empty string if none match.
func (c *Context) AcceptsLanguages(offers ...string) string {
	return negotiate.Accept(c.Header("accept-language"), offers)
}
