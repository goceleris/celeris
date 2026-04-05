package celeris

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/textproto"
	"strings"
	"testing"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

func TestContextBind(t *testing.T) {
	s, _ := newTestStream("POST", "/bind")
	s.GetBuf().Write([]byte(`{"name":"test"}`))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	var v struct{ Name string }
	if err := c.Bind(&v); err != nil {
		t.Fatal(err)
	}
	if v.Name != "test" {
		t.Fatalf("expected test, got %s", v.Name)
	}
}

func TestContextHeader(t *testing.T) {
	s, _ := newTestStream("GET", "/headers")
	s.Headers = append(s.Headers, [2]string{"x-custom", "val"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.Header("x-custom") != "val" {
		t.Fatalf("expected val, got %s", c.Header("x-custom"))
	}
	if c.Header("x-missing") != "" {
		t.Fatal("expected empty for missing header")
	}
}

func TestContextParam(t *testing.T) {
	s, _ := newTestStream("GET", "/users/42")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.params = Params{{Key: "id", Value: "42"}}
	if c.Param("id") != "42" {
		t.Fatalf("expected 42, got %s", c.Param("id"))
	}
	if c.Param("missing") != "" {
		t.Fatal("expected empty for missing param")
	}
}

func TestContextParamInt(t *testing.T) {
	s, _ := newTestStream("GET", "/users/42")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.params = append(c.params, Param{Key: "id", Value: "42"})

	v, err := c.ParamInt("id")
	if err != nil {
		t.Fatal(err)
	}
	if v != 42 {
		t.Fatalf("expected 42, got %d", v)
	}

	_, err = c.ParamInt("missing")
	if err == nil {
		t.Fatal("expected error for missing param")
	}

	c.params = append(c.params, Param{Key: "bad", Value: "abc"})
	_, err = c.ParamInt("bad")
	if err == nil {
		t.Fatal("expected error for non-integer param")
	}
}

func TestContextParamInt64(t *testing.T) {
	s, _ := newTestStream("GET", "/items/9999999999")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.params = append(c.params, Param{Key: "id", Value: "9999999999"})

	v, err := c.ParamInt64("id")
	if err != nil {
		t.Fatal(err)
	}
	if v != 9999999999 {
		t.Fatalf("expected 9999999999, got %d", v)
	}

	_, err = c.ParamInt64("missing")
	if err == nil {
		t.Fatal("expected error for missing param")
	}
}

func TestContextQueryCaching(t *testing.T) {
	s, _ := newTestStream("GET", "/search?q=hello&page=2")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// First call parses and caches.
	if c.Query("q") != "hello" {
		t.Fatalf("expected hello, got %s", c.Query("q"))
	}

	// Second call should use cache (same result).
	if c.Query("page") != "2" {
		t.Fatalf("expected 2, got %s", c.Query("page"))
	}

	if c.Query("missing") != "" {
		t.Fatal("expected empty for missing query param")
	}

	// Verify cache is cleared on reset.
	releaseContext(c)
	if c.queryCached {
		t.Fatal("expected queryCached to be false after reset")
	}
}

func TestContextQueryValues(t *testing.T) {
	s, _ := newTestStream("GET", "/multi?color=red&color=blue&color=green")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	vals := c.QueryValues("color")
	if len(vals) != 3 {
		t.Fatalf("expected 3 values, got %d", len(vals))
	}
	if vals[0] != "red" || vals[1] != "blue" || vals[2] != "green" {
		t.Fatalf("expected [red blue green], got %v", vals)
	}

	// Non-existent key returns nil.
	vals = c.QueryValues("missing")
	if vals != nil {
		t.Fatalf("expected nil for missing key, got %v", vals)
	}
}

func TestContextQueryParams(t *testing.T) {
	s, _ := newTestStream("GET", "/all?a=1&b=2&c=3")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	params := c.QueryParams()
	if params == nil {
		t.Fatal("expected non-nil params")
	}
	if params.Get("a") != "1" {
		t.Fatalf("expected a=1, got %s", params.Get("a"))
	}
	if params.Get("b") != "2" {
		t.Fatalf("expected b=2, got %s", params.Get("b"))
	}
	if params.Get("c") != "3" {
		t.Fatalf("expected c=3, got %s", params.Get("c"))
	}
}

func TestContextQueryDefault(t *testing.T) {
	s, _ := newTestStream("GET", "/search?q=hello")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.QueryDefault("q", "fallback") != "hello" {
		t.Fatalf("expected hello, got %s", c.QueryDefault("q", "fallback"))
	}
	if c.QueryDefault("missing", "fallback") != "fallback" {
		t.Fatalf("expected fallback, got %s", c.QueryDefault("missing", "fallback"))
	}
}

func TestContextQueryInt(t *testing.T) {
	s, _ := newTestStream("GET", "/list?page=3&bad=abc")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.QueryInt("page", 1) != 3 {
		t.Fatalf("expected 3, got %d", c.QueryInt("page", 1))
	}
	if c.QueryInt("missing", 1) != 1 {
		t.Fatalf("expected 1 (default), got %d", c.QueryInt("missing", 1))
	}
	if c.QueryInt("bad", 1) != 1 {
		t.Fatalf("expected 1 (default for non-int), got %d", c.QueryInt("bad", 1))
	}
}

func TestBindJSON(t *testing.T) {
	s, _ := newTestStream("POST", "/bind-json")
	s.GetBuf().Write([]byte(`{"name":"test"}`))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	var v struct{ Name string }
	if err := c.BindJSON(&v); err != nil {
		t.Fatal(err)
	}
	if v.Name != "test" {
		t.Fatalf("expected test, got %s", v.Name)
	}
}

func TestBindXML(t *testing.T) {
	s, _ := newTestStream("POST", "/bind-xml")
	s.GetBuf().Write([]byte(`<Item><Name>test</Name></Item>`))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	type Item struct {
		Name string `xml:"Name"`
	}
	var v Item
	if err := c.BindXML(&v); err != nil {
		t.Fatal(err)
	}
	if v.Name != "test" {
		t.Fatalf("expected test, got %s", v.Name)
	}
}

func TestBindContentTypeDetection(t *testing.T) {
	// JSON (default, no Content-Type).
	s, _ := newTestStream("POST", "/bind-auto")
	s.GetBuf().Write([]byte(`{"name":"json"}`))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	var v struct{ Name string }
	if err := c.Bind(&v); err != nil {
		t.Fatal(err)
	}
	if v.Name != "json" {
		t.Fatalf("expected json, got %s", v.Name)
	}

	// XML with Content-Type header.
	s2, _ := newTestStream("POST", "/bind-auto-xml")
	s2.Headers = append(s2.Headers, [2]string{"content-type", "application/xml"})
	s2.GetBuf().Write([]byte(`<Item><Name>xml</Name></Item>`))
	defer s2.Release()

	c2 := acquireContext(s2)
	defer releaseContext(c2)

	type Item struct {
		Name string `xml:"Name"`
	}
	var v2 Item
	if err := c2.Bind(&v2); err != nil {
		t.Fatal(err)
	}
	if v2.Name != "xml" {
		t.Fatalf("expected xml, got %s", v2.Name)
	}
}

func TestBindEmptyBody(t *testing.T) {
	s, _ := newTestStream("POST", "/bind-empty")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	var v struct{ Name string }
	err := c.Bind(&v)
	if err == nil {
		t.Fatal("expected error for empty body")
	}

	err = c.BindJSON(&v)
	if err == nil {
		t.Fatal("expected error for empty body")
	}

	err = c.BindXML(&v)
	if err == nil {
		t.Fatal("expected error for empty body")
	}
}

// --- BasicAuth tests ---

func TestContextBasicAuth(t *testing.T) {
	s, _ := newTestStream("GET", "/auth")
	s.Headers = append(s.Headers, [2]string{"authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	user, pass, ok := c.BasicAuth()
	if !ok {
		t.Fatal("expected ok")
	}
	if user != "alice" {
		t.Fatalf("expected alice, got %s", user)
	}
	if pass != "s3cret" {
		t.Fatalf("expected s3cret, got %s", pass)
	}
}

func TestContextBasicAuthMissing(t *testing.T) {
	s, _ := newTestStream("GET", "/auth")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, _, ok := c.BasicAuth()
	if ok {
		t.Fatal("expected not ok for missing header")
	}
}

func TestContextBasicAuthNonBasic(t *testing.T) {
	s, _ := newTestStream("GET", "/auth")
	s.Headers = append(s.Headers, [2]string{"authorization", "Bearer token123"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, _, ok := c.BasicAuth()
	if ok {
		t.Fatal("expected not ok for Bearer scheme")
	}
}

func TestContextBasicAuthMalformedBase64(t *testing.T) {
	s, _ := newTestStream("GET", "/auth")
	s.Headers = append(s.Headers, [2]string{"authorization", "Basic !!!invalid!!!"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, _, ok := c.BasicAuth()
	if ok {
		t.Fatal("expected not ok for malformed base64")
	}
}

func TestContextBasicAuthNoColon(t *testing.T) {
	s, _ := newTestStream("GET", "/auth")
	s.Headers = append(s.Headers, [2]string{"authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte("nocolon"))})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, _, ok := c.BasicAuth()
	if ok {
		t.Fatal("expected not ok for missing colon")
	}
}

func TestContextBasicAuthEmptyUsername(t *testing.T) {
	s, _ := newTestStream("GET", "/auth")
	s.Headers = append(s.Headers, [2]string{"authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(":password"))})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	user, pass, ok := c.BasicAuth()
	if !ok {
		t.Fatal("expected ok")
	}
	if user != "" {
		t.Fatalf("expected empty username, got %s", user)
	}
	if pass != "password" {
		t.Fatalf("expected password, got %s", pass)
	}
}

func TestContextBasicAuthPasswordWithColons(t *testing.T) {
	s, _ := newTestStream("GET", "/auth")
	s.Headers = append(s.Headers, [2]string{"authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass:word:more"))})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	user, pass, ok := c.BasicAuth()
	if !ok {
		t.Fatal("expected ok")
	}
	if user != "user" {
		t.Fatalf("expected user, got %s", user)
	}
	if pass != "pass:word:more" {
		t.Fatalf("expected pass:word:more, got %s", pass)
	}
}

// --- Form handling tests ---

func TestContextFormValueURLEncoded(t *testing.T) {
	s, _ := newTestStream("POST", "/form")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.GetBuf().Write([]byte("name=alice&age=30"))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.FormValue("name") != "alice" {
		t.Fatalf("expected alice, got %s", c.FormValue("name"))
	}
	if c.FormValue("age") != "30" {
		t.Fatalf("expected 30, got %s", c.FormValue("age"))
	}
	if c.FormValue("missing") != "" {
		t.Fatal("expected empty for missing field")
	}
}

func TestContextFormValuesMultiple(t *testing.T) {
	s, _ := newTestStream("POST", "/form")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.GetBuf().Write([]byte("color=red&color=blue&color=green"))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	vals := c.FormValues("color")
	if len(vals) != 3 {
		t.Fatalf("expected 3 values, got %d", len(vals))
	}
	if vals[0] != "red" || vals[1] != "blue" || vals[2] != "green" {
		t.Fatalf("expected [red blue green], got %v", vals)
	}
}

func TestContextMultipartFormValue(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("name", "bob")
	_ = w.WriteField("age", "25")
	_ = w.Close()

	s, _ := newTestStream("POST", "/upload")
	s.Headers = append(s.Headers, [2]string{"content-type", w.FormDataContentType()})
	s.GetBuf().Write(buf.Bytes())
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.FormValue("name") != "bob" {
		t.Fatalf("expected bob, got %s", c.FormValue("name"))
	}
	if c.FormValue("age") != "25" {
		t.Fatalf("expected 25, got %s", c.FormValue("age"))
	}
}

func TestContextFormFile(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="test.txt"`)
	h.Set("Content-Type", "text/plain")
	part, err := w.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("file content"))
	_ = w.Close()

	s, _ := newTestStream("POST", "/upload")
	s.Headers = append(s.Headers, [2]string{"content-type", w.FormDataContentType()})
	s.GetBuf().Write(buf.Bytes())
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	f, fh, err := c.FormFile("file")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	if fh.Filename != "test.txt" {
		t.Fatalf("expected test.txt, got %s", fh.Filename)
	}

	data, _ := io.ReadAll(f)
	if string(data) != "file content" {
		t.Fatalf("expected 'file content', got '%s'", string(data))
	}
}

func TestContextFormFileNonMultipart(t *testing.T) {
	s, _ := newTestStream("POST", "/upload")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.GetBuf().Write([]byte("key=value"))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, _, err := c.FormFile("file")
	if err == nil {
		t.Fatal("expected error for non-multipart request")
	}
}

func TestContextFormEmptyBody(t *testing.T) {
	s, _ := newTestStream("POST", "/empty")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.FormValue("key") != "" {
		t.Fatal("expected empty for empty body")
	}
}

func TestContextFormCaching(t *testing.T) {
	s, _ := newTestStream("POST", "/form")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.GetBuf().Write([]byte("a=1&b=2"))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// First call parses.
	if c.FormValue("a") != "1" {
		t.Fatalf("expected 1, got %s", c.FormValue("a"))
	}
	// Second call uses cache.
	if c.FormValue("b") != "2" {
		t.Fatalf("expected 2, got %s", c.FormValue("b"))
	}
}

func TestContextFormResetClearsForm(t *testing.T) {
	s, _ := newTestStream("POST", "/form")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.GetBuf().Write([]byte("key=value"))
	defer s.Release()

	c := acquireContext(s)
	_ = c.FormValue("key") // trigger parse
	releaseContext(c)

	if c.formParsed {
		t.Fatal("expected formParsed to be false after reset")
	}
	if c.formValues != nil {
		t.Fatal("expected formValues to be nil after reset")
	}
}

func TestContextMaxFormSizeUnlimited(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("data", strings.Repeat("x", 1024))
	_ = w.Close()

	s, _ := newTestStream("POST", "/form")
	s.Headers = append(s.Headers, [2]string{"content-type", w.FormDataContentType()})
	s.GetBuf().Write(buf.Bytes())
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)
	c.maxFormSize = -1

	if c.FormValue("data") == "" {
		t.Fatal("expected form data to be parsed with maxFormSize=-1")
	}
	if len(c.FormValue("data")) != 1024 {
		t.Fatalf("expected 1024 bytes, got %d", len(c.FormValue("data")))
	}
}

func TestContextBodyCopy(t *testing.T) {
	s, _ := newTestStream("POST", "/data")
	s.GetBuf().Write([]byte("original"))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	cp := c.BodyCopy()
	if string(cp) != "original" {
		t.Fatalf("expected 'original', got %q", cp)
	}
	// Verify independence: mutating the copy shouldn't affect Body().
	cp[0] = 'X'
	if c.Body()[0] == 'X' {
		t.Fatal("BodyCopy should return independent slice")
	}
}

func TestContextBodyCopyEmpty(t *testing.T) {
	s, _ := newTestStream("GET", "/empty")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if cp := c.BodyCopy(); cp != nil {
		t.Fatalf("expected nil for empty body, got %v", cp)
	}
}

func TestContextBodyReader(t *testing.T) {
	s, _ := newTestStream("POST", "/data")
	s.GetBuf().Write([]byte("read me"))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	data, err := io.ReadAll(c.BodyReader())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "read me" {
		t.Fatalf("expected 'read me', got %q", data)
	}
}

func TestContextClientIP(t *testing.T) {
	tests := []struct {
		name     string
		headers  [][2]string
		expected string
	}{
		{
			name: "x-forwarded-for single",
			headers: [][2]string{
				{":method", "GET"}, {":path", "/ip"},
				{"x-forwarded-for", "1.2.3.4"},
			},
			expected: "1.2.3.4",
		},
		{
			name: "x-forwarded-for multiple",
			headers: [][2]string{
				{":method", "GET"}, {":path", "/ip"},
				{"x-forwarded-for", "1.2.3.4, 5.6.7.8"},
			},
			expected: "1.2.3.4",
		},
		{
			name: "x-real-ip",
			headers: [][2]string{
				{":method", "GET"}, {":path", "/ip"},
				{"x-real-ip", "10.0.0.1"},
			},
			expected: "10.0.0.1",
		},
		{
			name: "x-forwarded-for takes precedence",
			headers: [][2]string{
				{":method", "GET"}, {":path", "/ip"},
				{"x-forwarded-for", "1.2.3.4"},
				{"x-real-ip", "10.0.0.1"},
			},
			expected: "1.2.3.4",
		},
		{
			name: "no proxy headers",
			headers: [][2]string{
				{":method", "GET"}, {":path", "/ip"},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := stream.NewStream(1)
			st.Headers = tt.headers
			rw := &mockResponseWriter{}
			st.ResponseWriter = rw
			defer st.Release()

			c := acquireContext(st)
			defer releaseContext(c)

			if got := c.ClientIP(); got != tt.expected {
				t.Fatalf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}

func TestContextScheme(t *testing.T) {
	// Default: falls back to :scheme pseudo-header.
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.Scheme() != "http" {
		t.Fatalf("expected http, got %s", c.Scheme())
	}

	// SetScheme override takes precedence.
	s2, _ := newTestStream("GET", "/test")
	defer s2.Release()

	c2 := acquireContext(s2)
	defer releaseContext(c2)

	c2.SetScheme("https")
	if c2.Scheme() != "https" {
		t.Fatalf("expected https, got %s", c2.Scheme())
	}

	// :scheme pseudo-header is respected when no override set.
	s3, _ := newTestStream("GET", "/test")
	// Replace the default :scheme "http" with "https".
	for i := range s3.Headers {
		if s3.Headers[i][0] == ":scheme" {
			s3.Headers[i][1] = "https"
			break
		}
	}
	defer s3.Release()

	c3 := acquireContext(s3)
	defer releaseContext(c3)

	if c3.Scheme() != "https" {
		t.Fatalf("expected https from :scheme header, got %s", c3.Scheme())
	}

	// Raw X-Forwarded-Proto is NOT trusted without proxy middleware.
	s4, _ := newTestStream("GET", "/test")
	s4.Headers = append(s4.Headers, [2]string{"x-forwarded-proto", "https"})
	defer s4.Release()

	c4 := acquireContext(s4)
	defer releaseContext(c4)

	if c4.Scheme() != "http" {
		t.Fatalf("expected http (XFF should be ignored), got %s", c4.Scheme())
	}
}

func TestContextCookieRoundTrip(t *testing.T) {
	s, rw := newTestStream("GET", "/cookie")
	s.Headers = append(s.Headers, [2]string{"cookie", "session=abc123; theme=dark"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// Read cookies.
	v, err := c.Cookie("session")
	if err != nil {
		t.Fatal(err)
	}
	if v != "abc123" {
		t.Fatalf("expected abc123, got %s", v)
	}

	v, err = c.Cookie("theme")
	if err != nil {
		t.Fatal(err)
	}
	if v != "dark" {
		t.Fatalf("expected dark, got %s", v)
	}

	_, err = c.Cookie("missing")
	if err != ErrNoCookie {
		t.Fatalf("expected ErrNoCookie, got %v", err)
	}

	// Set cookie.
	c.SetCookie(&Cookie{
		Name:     "token",
		Value:    "xyz",
		Path:     "/api",
		MaxAge:   3600,
		HTTPOnly: true,
		Secure:   true,
	})
	_ = c.NoContent(200)

	var cookieHeader string
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			cookieHeader = h[1]
			break
		}
	}
	if cookieHeader == "" {
		t.Fatal("expected set-cookie header")
	}

	expected := "token=xyz; Path=/api; Max-Age=3600; HttpOnly; Secure"
	if cookieHeader != expected {
		t.Fatalf("expected %q, got %q", expected, cookieHeader)
	}
}

func TestContextCookieNoCookieHeader(t *testing.T) {
	s, _ := newTestStream("GET", "/no-cookie")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, err := c.Cookie("anything")
	if err != ErrNoCookie {
		t.Fatalf("expected ErrNoCookie, got %v", err)
	}
}

func TestCookieRawValue(t *testing.T) {
	// Cookie values should be returned as-is without URL-decoding.
	// '+' should NOT be converted to space.
	s, _ := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"cookie", "token=abc+def%20ghi"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	val, err := c.Cookie("token")
	if err != nil {
		t.Fatal(err)
	}
	if val != "abc+def%20ghi" {
		t.Fatalf("expected raw value %q, got %q", "abc+def%20ghi", val)
	}
}

func TestCookieRoundTripSpecialChars(t *testing.T) {
	s, rw := newTestStream("GET", "/cookie")
	s.Headers = append(s.Headers, [2]string{"cookie", "data=a=b&c"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// Read cookie with special chars.
	v, err := c.Cookie("data")
	if err != nil {
		t.Fatal(err)
	}
	if v != "a=b&c" {
		t.Fatalf("expected a=b&c, got %s", v)
	}

	// Set cookie with special chars and verify round-trip.
	c.SetCookie(&Cookie{
		Name:  "data",
		Value: "a=b&c",
	})
	_ = c.NoContent(200)

	var cookieHeader string
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			cookieHeader = h[1]
			break
		}
	}
	if cookieHeader != "data=a=b&c" {
		t.Fatalf("expected data=a=b&c, got %q", cookieHeader)
	}
}

func TestRemoteAddr(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	s.RemoteAddr = "192.168.1.1:54321"
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.RemoteAddr() != "192.168.1.1:54321" {
		t.Fatalf("expected 192.168.1.1:54321, got %s", c.RemoteAddr())
	}
}

func TestRemoteAddrEmpty(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.RemoteAddr() != "" {
		t.Fatalf("expected empty, got %s", c.RemoteAddr())
	}
}

func TestHost(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// :authority is "localhost" from newTestStream
	if c.Host() != "localhost" {
		t.Fatalf("expected localhost, got %s", c.Host())
	}
}

func TestHostHTTP1(t *testing.T) {
	s := stream.NewStream(1)
	s.Headers = [][2]string{
		{":method", "GET"},
		{":path", "/test"},
		{":scheme", "http"},
		{"host", "example.com"},
	}
	rw := &mockResponseWriter{}
	s.ResponseWriter = rw
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// No :authority, should fall back to host header
	if c.Host() != "example.com" {
		t.Fatalf("expected example.com, got %s", c.Host())
	}
}

func TestIsWebSocket(t *testing.T) {
	s, _ := newTestStream("GET", "/ws")
	s.Headers = append(s.Headers, [2]string{"upgrade", "websocket"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if !c.IsWebSocket() {
		t.Fatal("expected IsWebSocket to return true")
	}
}

func TestIsWebSocketFalse(t *testing.T) {
	s, _ := newTestStream("GET", "/ws")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.IsWebSocket() {
		t.Fatal("expected IsWebSocket to return false")
	}
}

func TestIsWebSocketCaseInsensitive(t *testing.T) {
	s, _ := newTestStream("GET", "/ws")
	s.Headers = append(s.Headers, [2]string{"upgrade", "WebSocket"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if !c.IsWebSocket() {
		t.Fatal("expected IsWebSocket to be case-insensitive")
	}
}

func TestIsTLS(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)
	c.SetScheme("https")

	if !c.IsTLS() {
		t.Fatal("expected IsTLS to return true for https")
	}
}

func TestIsTLSFalse(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.IsTLS() {
		t.Fatal("expected IsTLS to return false for http")
	}
}

func TestAcceptsEncodings(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"accept-encoding", "gzip, br;q=0.8, identity;q=0.5"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	got := c.AcceptsEncodings("br", "gzip", "identity")
	if got != "gzip" {
		t.Fatalf("expected gzip (highest q), got %q", got)
	}
}

func TestAcceptsEncodingsNoMatch(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"accept-encoding", "gzip"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	got := c.AcceptsEncodings("br", "zstd")
	if got != "" {
		t.Fatalf("expected empty string for no match, got %q", got)
	}
}

func TestAcceptsLanguages(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"accept-language", "fr;q=0.9, en;q=1.0, de;q=0.7"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	got := c.AcceptsLanguages("de", "en", "fr")
	if got != "en" {
		t.Fatalf("expected en (highest q), got %q", got)
	}
}

func TestAcceptsLanguagesNoMatch(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"accept-language", "fr"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	got := c.AcceptsLanguages("en", "de")
	if got != "" {
		t.Fatalf("expected empty string for no match, got %q", got)
	}
}

// --- FormFile/MultipartForm HTTPError tests ---

func TestFormFileNotMultipart(t *testing.T) {
	s, _ := newTestStream("POST", "/upload")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.GetBuf().Write([]byte("key=value"))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, _, err := c.FormFile("file")
	if err == nil {
		t.Fatal("expected error for non-multipart request")
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected HTTPError, got %T: %v", err, err)
	}
	if he.Code != 400 {
		t.Fatalf("expected 400 code, got %d", he.Code)
	}
}

func TestMultipartFormNotMultipart(t *testing.T) {
	s, _ := newTestStream("POST", "/upload")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.GetBuf().Write([]byte("key=value"))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_, err := c.MultipartForm()
	if err == nil {
		t.Fatal("expected error for non-multipart request")
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected HTTPError, got %T: %v", err, err)
	}
	if he.Code != 400 {
		t.Fatalf("expected 400 code, got %d", he.Code)
	}
}

func TestRawQuery(t *testing.T) {
	s, _ := newTestStream("GET", "/search?q=hello&page=2")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.RawQuery() != "q=hello&page=2" {
		t.Fatalf("expected 'q=hello&page=2', got %q", c.RawQuery())
	}
}

func TestRawQueryEmpty(t *testing.T) {
	s, _ := newTestStream("GET", "/search")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.RawQuery() != "" {
		t.Fatalf("expected empty, got %q", c.RawQuery())
	}
}

func TestRequestHeaders(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"x-custom", "value"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	headers := c.RequestHeaders()
	if len(headers) != 5 { // :method, :path, :scheme, :authority, x-custom
		t.Fatalf("expected 5 headers, got %d", len(headers))
	}

	found := false
	for _, h := range headers {
		if h[0] == "x-custom" && h[1] == "value" {
			found = true
		}
	}
	if !found {
		t.Fatal("x-custom header not found")
	}

	// Verify it's a copy (modifying returned slice shouldn't affect stream)
	headers[0][0] = "modified"
	origHeaders := c.RequestHeaders()
	if origHeaders[0][0] == "modified" {
		t.Fatal("RequestHeaders should return a copy")
	}
}

func TestContextFormValueOK(t *testing.T) {
	body := "name=alice&empty="
	s, _ := newTestStream("POST", "/form")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.GetBuf().Write([]byte(body))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	v, ok := c.FormValueOK("name")
	if !ok || v != "alice" {
		t.Fatalf("expected (alice, true), got (%s, %v)", v, ok)
	}

	v, ok = c.FormValueOK("empty")
	if !ok {
		t.Fatal("expected field 'empty' to be present")
	}
	if v != "" {
		t.Fatalf("expected empty string, got %q", v)
	}

	_, ok = c.FormValueOK("missing")
	if ok {
		t.Fatal("expected missing field to return false")
	}
}

func TestContextHTML(t *testing.T) {
	s, rw := newTestStream("GET", "/page")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.HTML(200, "<h1>Hello</h1>"); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	if string(rw.body) != "<h1>Hello</h1>" {
		t.Fatalf("expected <h1>Hello</h1>, got %s", string(rw.body))
	}
	var ct string
	for _, h := range rw.headers {
		if h[0] == "content-type" {
			ct = h[1]
			break
		}
	}
	if ct != "text/html; charset=utf-8" {
		t.Fatalf("expected text/html; charset=utf-8, got %s", ct)
	}
}

func TestContextContentLength(t *testing.T) {
	s, _ := newTestStream("POST", "/body")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// No content-length header
	if cl := c.ContentLength(); cl != -1 {
		t.Fatalf("expected -1 for missing content-length, got %d", cl)
	}

	// Valid content-length
	s.Headers = append(s.Headers, [2]string{"content-length", "42"})
	if cl := c.ContentLength(); cl != 42 {
		t.Fatalf("expected 42, got %d", cl)
	}
}

func TestContextContentLengthInvalid(t *testing.T) {
	s, _ := newTestStream("POST", "/body")
	s.Headers = append(s.Headers, [2]string{"content-length", "notanumber"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if cl := c.ContentLength(); cl != -1 {
		t.Fatalf("expected -1 for invalid content-length, got %d", cl)
	}
}

func TestContextContentLengthNegative(t *testing.T) {
	s, _ := newTestStream("POST", "/body")
	s.Headers = append(s.Headers, [2]string{"content-length", "-1"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if cl := c.ContentLength(); cl != -1 {
		t.Fatalf("expected -1 for negative content-length, got %d", cl)
	}
}

func TestContextSetRawQuery(t *testing.T) {
	s, _ := newTestStream("GET", "/test?foo=bar")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.RawQuery() != "foo=bar" {
		t.Fatalf("expected foo=bar, got %s", c.RawQuery())
	}

	// Cache query params
	if v := c.Query("foo"); v != "bar" {
		t.Fatalf("expected bar, got %s", v)
	}

	// Override query string — should invalidate cache
	c.SetRawQuery("baz=qux")
	if c.RawQuery() != "baz=qux" {
		t.Fatalf("expected baz=qux, got %s", c.RawQuery())
	}
	if v := c.Query("foo"); v != "" {
		t.Fatalf("expected empty after override, got %s", v)
	}
	if v := c.Query("baz"); v != "qux" {
		t.Fatalf("expected qux, got %s", v)
	}
}

func TestContextSetClientIP(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"x-forwarded-for", "1.2.3.4"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if ip := c.ClientIP(); ip != "1.2.3.4" {
		t.Fatalf("expected 1.2.3.4, got %s", ip)
	}

	c.SetClientIP("10.0.0.1")
	if ip := c.ClientIP(); ip != "10.0.0.1" {
		t.Fatalf("expected override 10.0.0.1, got %s", ip)
	}
}

func TestContextSetScheme(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if scheme := c.Scheme(); scheme != "http" {
		t.Fatalf("expected http, got %s", scheme)
	}

	c.SetScheme("https")
	if scheme := c.Scheme(); scheme != "https" {
		t.Fatalf("expected override https, got %s", scheme)
	}
}

func TestContextSetHost(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if host := c.Host(); host != "localhost" {
		t.Fatalf("expected localhost, got %s", host)
	}

	c.SetHost("example.com")
	if host := c.Host(); host != "example.com" {
		t.Fatalf("expected override example.com, got %s", host)
	}
}

func TestContextOverridesResetOnReuse(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	c.SetClientIP("10.0.0.1")
	c.SetScheme("https")
	c.SetHost("example.com")
	releaseContext(c)

	// Reacquire and verify overrides are cleared
	c2 := acquireContext(s)
	defer releaseContext(c2)

	if c2.clientIPOverride != "" {
		t.Fatal("expected clientIPOverride to be cleared")
	}
	if c2.schemeOverride != "" {
		t.Fatal("expected schemeOverride to be cleared")
	}
	if c2.hostOverride != "" {
		t.Fatal("expected hostOverride to be cleared")
	}
}

func TestQueryBool(t *testing.T) {
	tests := []struct {
		query    string
		key      string
		def      bool
		expected bool
	}{
		{"debug=true", "debug", false, true},
		{"debug=1", "debug", false, true},
		{"debug=yes", "debug", false, true},
		{"debug=false", "debug", true, false},
		{"debug=0", "debug", true, false},
		{"debug=no", "debug", true, false},
		{"debug=TRUE", "debug", false, true},
		{"debug=FALSE", "debug", true, false},
		{"debug=Yes", "debug", false, true},
		{"debug=No", "debug", true, false},
		{"debug=invalid", "debug", true, true},
		{"debug=invalid", "debug", false, false},
		{"", "debug", true, true},
		{"", "debug", false, false},
		{"other=1", "debug", false, false},
		{"other=1", "debug", true, true},
	}
	for _, tt := range tests {
		name := tt.query + "_" + tt.key
		if tt.def {
			name += "_def=true"
		} else {
			name += "_def=false"
		}
		t.Run(name, func(t *testing.T) {
			path := "/test"
			if tt.query != "" {
				path += "?" + tt.query
			}
			s, _ := newTestStream("GET", path)
			defer s.Release()

			c := acquireContext(s)
			defer releaseContext(c)

			got := c.QueryBool(tt.key, tt.def)
			if got != tt.expected {
				t.Fatalf("QueryBool(%q, %v) = %v, want %v", tt.key, tt.def, got, tt.expected)
			}
		})
	}
}

func TestQueryInt64(t *testing.T) {
	tests := []struct {
		query    string
		key      string
		def      int64
		expected int64
	}{
		{"page=42", "page", 0, 42},
		{"page=9999999999", "page", 0, 9999999999},
		{"page=-100", "page", 0, -100},
		{"page=0", "page", 99, 0},
		{"page=abc", "page", 10, 10},
		{"", "page", 5, 5},
		{"other=1", "page", 7, 7},
		{"page=9223372036854775807", "page", 0, 9223372036854775807},
	}
	for _, tt := range tests {
		name := tt.query + "_" + tt.key
		t.Run(name, func(t *testing.T) {
			path := "/test"
			if tt.query != "" {
				path += "?" + tt.query
			}
			s, _ := newTestStream("GET", path)
			defer s.Release()

			c := acquireContext(s)
			defer releaseContext(c)

			got := c.QueryInt64(tt.key, tt.def)
			if got != tt.expected {
				t.Fatalf("QueryInt64(%q, %d) = %d, want %d", tt.key, tt.def, got, tt.expected)
			}
		})
	}
}

func TestParamDefault(t *testing.T) {
	s, _ := newTestStream("GET", "/users/alice")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.params = Params{
		{Key: "name", Value: "alice"},
		{Key: "empty", Value: ""},
	}

	if got := c.ParamDefault("name", "fallback"); got != "alice" {
		t.Fatalf("expected alice, got %s", got)
	}
	if got := c.ParamDefault("empty", "fallback"); got != "fallback" {
		t.Fatalf("expected fallback for empty param, got %s", got)
	}
	if got := c.ParamDefault("missing", "fallback"); got != "fallback" {
		t.Fatalf("expected fallback for missing param, got %s", got)
	}
}

func TestFormValueOkDeprecated(t *testing.T) {
	body := "name=alice&empty="
	s, _ := newTestStream("POST", "/form")
	s.Headers = append(s.Headers, [2]string{"content-type", "application/x-www-form-urlencoded"})
	s.GetBuf().Write([]byte(body))
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// FormValueOk (deprecated) should return the same results as FormValueOK.
	v1, ok1 := c.FormValueOK("name")
	v2, ok2 := c.FormValueOk("name")
	if v1 != v2 || ok1 != ok2 {
		t.Fatalf("FormValueOk != FormValueOK: (%q,%v) vs (%q,%v)", v1, ok1, v2, ok2)
	}
	if !ok1 || v1 != "alice" {
		t.Fatalf("expected (alice, true), got (%s, %v)", v1, ok1)
	}

	v1, ok1 = c.FormValueOK("empty")
	v2, ok2 = c.FormValueOk("empty")
	if v1 != v2 || ok1 != ok2 {
		t.Fatalf("FormValueOk != FormValueOK for empty: (%q,%v) vs (%q,%v)", v1, ok1, v2, ok2)
	}

	_, ok1 = c.FormValueOK("missing")
	_, ok2 = c.FormValueOk("missing")
	if ok1 != ok2 {
		t.Fatalf("FormValueOk != FormValueOK for missing: %v vs %v", ok1, ok2)
	}
	if ok1 {
		t.Fatal("expected missing field to return false")
	}
}

func TestContextProtocolH2(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if p := c.Protocol(); p != "2" {
		t.Fatalf("expected \"2\" for default H2 stream, got %q", p)
	}
}

func TestContextProtocolH1(t *testing.T) {
	s := stream.NewH1Stream(1)
	s.Headers = [][2]string{
		{":method", "GET"},
		{":path", "/test"},
		{":scheme", "http"},
		{":authority", "localhost"},
	}
	rw := &mockResponseWriter{}
	s.ResponseWriter = rw
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if p := c.Protocol(); p != "1.1" {
		t.Fatalf("expected \"1.1\" for H1 stream, got %q", p)
	}
}

func TestClientIPTrustedProxies(t *testing.T) {
	_, trusted, _ := net.ParseCIDR("10.0.0.0/8")
	_, trusted2, _ := net.ParseCIDR("192.168.0.0/16")
	nets := []*net.IPNet{trusted, trusted2}

	st := stream.NewStream(1)
	st.Headers = [][2]string{
		{":method", "GET"},
		{":path", "/ip"},
		{"x-forwarded-for", "203.0.113.50, 10.0.0.1, 192.168.1.5"},
	}
	st.RemoteAddr = "10.0.0.2:12345"
	rw := &mockResponseWriter{}
	st.ResponseWriter = rw
	defer st.Release()

	c := acquireContext(st)
	defer releaseContext(c)
	c.trustedNets = nets

	got := c.ClientIP()
	if got != "203.0.113.50" {
		t.Fatalf("expected 203.0.113.50, got %q", got)
	}
}

func TestClientIPTrustedProxiesAllTrusted(t *testing.T) {
	_, trusted, _ := net.ParseCIDR("10.0.0.0/8")
	nets := []*net.IPNet{trusted}

	st := stream.NewStream(1)
	st.Headers = [][2]string{
		{":method", "GET"},
		{":path", "/ip"},
		{"x-forwarded-for", "10.0.0.1, 10.0.0.2"},
	}
	st.RemoteAddr = "10.0.0.3:12345"
	rw := &mockResponseWriter{}
	st.ResponseWriter = rw
	defer st.Release()

	c := acquireContext(st)
	defer releaseContext(c)
	c.trustedNets = nets

	got := c.ClientIP()
	if got != "10.0.0.3" {
		t.Fatalf("expected fallback to RemoteAddr 10.0.0.3, got %q", got)
	}
}

func TestClientIPNoTrustedProxies(t *testing.T) {
	st := stream.NewStream(1)
	st.Headers = [][2]string{
		{":method", "GET"},
		{":path", "/ip"},
		{"x-forwarded-for", "203.0.113.50, 10.0.0.1"},
	}
	rw := &mockResponseWriter{}
	st.ResponseWriter = rw
	defer st.Release()

	c := acquireContext(st)
	defer releaseContext(c)
	// trustedNets is nil — legacy behavior: return leftmost entry.
	got := c.ClientIP()
	if got != "203.0.113.50" {
		t.Fatalf("expected 203.0.113.50 (leftmost), got %q", got)
	}
}
