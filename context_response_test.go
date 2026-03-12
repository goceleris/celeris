package celeris

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

func TestContextJSON(t *testing.T) {
	s, rw := newTestStream("GET", "/json")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	data := map[string]string{"hello": "world"}
	if err := c.JSON(200, data); err != nil {
		t.Fatal(err)
	}

	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}

	var result map[string]string
	if err := json.Unmarshal(rw.body, &result); err != nil {
		t.Fatal(err)
	}
	if result["hello"] != "world" {
		t.Fatalf("expected world, got %s", result["hello"])
	}
}

func TestContextString(t *testing.T) {
	s, rw := newTestStream("GET", "/str")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.String(200, "hello %s", "world"); err != nil {
		t.Fatal(err)
	}
	if string(rw.body) != "hello world" {
		t.Fatalf("expected 'hello world', got '%s'", string(rw.body))
	}
}

func TestContextBlob(t *testing.T) {
	s, rw := newTestStream("GET", "/blob")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.Blob(201, "application/octet-stream", []byte{0x01, 0x02}); err != nil {
		t.Fatal(err)
	}
	if rw.status != 201 {
		t.Fatalf("expected 201, got %d", rw.status)
	}
	if len(rw.body) != 2 {
		t.Fatalf("expected 2 bytes, got %d", len(rw.body))
	}
}

func TestContextNoContent(t *testing.T) {
	s, rw := newTestStream("DELETE", "/item")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.NoContent(204); err != nil {
		t.Fatal(err)
	}
	if rw.status != 204 {
		t.Fatalf("expected 204, got %d", rw.status)
	}
	if len(rw.body) != 0 {
		t.Fatalf("expected empty body, got %d bytes", len(rw.body))
	}
}

func TestContextStatusCode(t *testing.T) {
	s, _ := newTestStream("GET", "/status")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.StatusCode() != 200 {
		t.Fatalf("expected default 200, got %d", c.StatusCode())
	}

	_ = c.String(201, "created")
	if c.StatusCode() != 201 {
		t.Fatalf("expected 201, got %d", c.StatusCode())
	}
}

func TestContextRedirect(t *testing.T) {
	s, rw := newTestStream("GET", "/old")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.Redirect(302, "/new"); err != nil {
		t.Fatal(err)
	}

	if rw.status != 302 {
		t.Fatalf("expected 302, got %d", rw.status)
	}

	var found bool
	for _, h := range rw.headers {
		if h[0] == "location" && h[1] == "/new" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected location: /new header")
	}
}

func TestContextRedirectCRLFInjection(t *testing.T) {
	s, rw := newTestStream("GET", "/redirect-crlf")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// Simulate user-controlled redirect target with CRLF.
	_ = c.Redirect(302, "/safe\r\nX-Injected: bad")

	for _, h := range rw.headers {
		if h[0] == "x-injected" {
			t.Fatal("CRLF injection via Redirect")
		}
	}
}

func TestRedirectInvalidCode(t *testing.T) {
	s, _ := newTestStream("GET", "/bad-redirect")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// 200 is not a valid redirect code.
	err := c.Redirect(200, "/dest")
	if err == nil {
		t.Fatal("expected error for non-3xx redirect code")
	}
	if !errors.Is(err, ErrInvalidRedirectCode) {
		t.Fatalf("expected ErrInvalidRedirectCode, got %v", err)
	}

	// 404 is not a valid redirect code.
	err = c.Redirect(404, "/dest")
	if err == nil {
		t.Fatal("expected error for 404 redirect code")
	}
	if !errors.Is(err, ErrInvalidRedirectCode) {
		t.Fatalf("expected ErrInvalidRedirectCode, got %v", err)
	}
}

func TestContextSetHeaderCRLFInjection(t *testing.T) {
	s, rw := newTestStream("GET", "/crlf")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// Attempt header injection via \r\n in value.
	c.SetHeader("location", "http://evil.com\r\nSet-Cookie: admin=true")
	_ = c.NoContent(302)

	// The injected \r\n must be stripped — no Set-Cookie header should appear.
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			t.Fatal("CRLF injection: Set-Cookie header was injected via location value")
		}
	}

	// Verify the location value has CRLF stripped.
	for _, h := range rw.headers {
		if h[0] == "location" {
			if h[1] != "http://evil.comSet-Cookie: admin=true" {
				t.Fatalf("expected CRLF stripped, got %q", h[1])
			}
			return
		}
	}
	t.Fatal("expected location header")
}

func TestContextSetHeaderLowercase(t *testing.T) {
	s, rw := newTestStream("GET", "/headers")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.SetHeader("Content-Type", "application/json")
	c.SetHeader("X-Custom-Header", "value")
	_ = c.NoContent(200)

	for _, h := range rw.headers {
		if h[0] == "Content-Type" {
			t.Fatal("Content-Type should be lowercased to content-type")
		}
		if h[0] == "X-Custom-Header" {
			t.Fatal("X-Custom-Header should be lowercased to x-custom-header")
		}
	}

	var found bool
	for _, h := range rw.headers {
		if h[0] == "content-type" && h[1] == "application/json" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected lowercased content-type header")
	}
}

func TestContextSetHeaderReplaces(t *testing.T) {
	s, rw := newTestStream("GET", "/replace")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.SetHeader("x-foo", "a")
	c.SetHeader("x-foo", "b")
	_ = c.NoContent(200)

	count := 0
	var val string
	for _, h := range rw.headers {
		if h[0] == "x-foo" {
			count++
			val = h[1]
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 x-foo header, got %d", count)
	}
	if val != "b" {
		t.Fatalf("expected value 'b', got %q", val)
	}
}

func TestContextAddHeaderAppends(t *testing.T) {
	s, rw := newTestStream("GET", "/append")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.AddHeader("set-cookie", "a=1")
	c.AddHeader("set-cookie", "b=2")
	_ = c.NoContent(200)

	count := 0
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected 2 set-cookie headers, got %d", count)
	}
}

func TestContextDoubleWriteGuard(t *testing.T) {
	s, _ := newTestStream("GET", "/double")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// First write should succeed.
	err := c.JSON(200, map[string]string{"ok": "true"})
	if err != nil {
		t.Fatalf("first write should succeed, got %v", err)
	}

	// Second write should return ErrResponseWritten.
	err = c.JSON(200, map[string]string{"bad": "true"})
	if err != ErrResponseWritten {
		t.Fatalf("expected ErrResponseWritten, got %v", err)
	}

	// NoContent should also be guarded.
	err = c.NoContent(204)
	if err != ErrResponseWritten {
		t.Fatalf("expected ErrResponseWritten from NoContent, got %v", err)
	}
}

func TestContextDoubleWriteStringBlob(t *testing.T) {
	s, _ := newTestStream("GET", "/double2")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	err := c.String(200, "first")
	if err != nil {
		t.Fatal(err)
	}

	err = c.Blob(200, "text/plain", []byte("second"))
	if err != ErrResponseWritten {
		t.Fatalf("expected ErrResponseWritten, got %v", err)
	}
}

func TestXMLResponse(t *testing.T) {
	s, rw := newTestStream("GET", "/xml")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	type Item struct {
		Name string `xml:"name"`
	}
	err := c.XML(200, Item{Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}

	var ctFound bool
	for _, h := range rw.headers {
		if h[0] == "content-type" && h[1] == "application/xml" {
			ctFound = true
			break
		}
	}
	if !ctFound {
		t.Fatal("expected content-type: application/xml")
	}

	if !strings.Contains(string(rw.body), "<name>test</name>") {
		t.Fatalf("expected XML body, got %s", string(rw.body))
	}
}

func TestCaptureResponseJSON(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.CaptureResponse()
	err := c.JSON(200, map[string]string{"hello": "world"})
	if err != nil {
		t.Fatal(err)
	}

	body := c.ResponseBody()
	if body == nil {
		t.Fatal("expected captured body")
	}
	if c.ResponseContentType() != "application/json" {
		t.Fatalf("expected application/json, got %q", c.ResponseContentType())
	}
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["hello"] != "world" {
		t.Fatal("captured body mismatch")
	}
}

func TestCaptureResponseDisabled(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	err := c.JSON(200, map[string]string{"hello": "world"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ResponseBody() != nil {
		t.Fatal("expected nil when capture not enabled")
	}
}

func TestBufferResponseCompress(t *testing.T) {
	s, rw := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.BufferResponse()
	if err := c.JSON(200, map[string]string{"msg": "hello"}); err != nil {
		t.Fatal(err)
	}

	// Response should be buffered, not written
	if c.IsWritten() {
		t.Fatal("response should be buffered, not written")
	}
	if rw.status != 0 {
		t.Fatal("wire should not have been written")
	}

	// Read buffered body, "transform" it
	body := c.ResponseBody()
	if body == nil {
		t.Fatal("expected buffered body")
	}
	transformed := bytes.ToUpper(body)
	c.SetResponseBody(transformed)

	if err := c.FlushResponse(); err != nil {
		t.Fatal(err)
	}

	if !c.IsWritten() {
		t.Fatal("response should be written after flush")
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	if !bytes.Equal(rw.body, transformed) {
		t.Fatalf("expected transformed body, got %s", rw.body)
	}
}

func TestBufferResponseETag304(t *testing.T) {
	s, rw := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"if-none-match", `"abc123"`})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.BufferResponse()
	if err := c.JSON(200, map[string]string{"data": "value"}); err != nil {
		t.Fatal(err)
	}

	// Simulate ETag middleware: compute hash, set header, replace with 304
	c.SetHeader("etag", `"abc123"`)
	ifNoneMatch := c.Header("if-none-match")
	if ifNoneMatch == `"abc123"` {
		// Replace buffered response with 304
		c.capturedStatus = 304
		c.capturedBody = nil
		c.capturedType = ""
	}

	if err := c.FlushResponse(); err != nil {
		t.Fatal(err)
	}

	if rw.status != 304 {
		t.Fatalf("expected 304, got %d", rw.status)
	}
	if len(rw.body) != 0 {
		t.Fatal("304 should have no body")
	}
}

func TestBufferResponseComposition(t *testing.T) {
	s, rw := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// Outer middleware buffers
	c.BufferResponse()
	// Inner middleware buffers
	c.BufferResponse()

	if err := c.Blob(200, "text/plain", []byte("original")); err != nil {
		t.Fatal(err)
	}

	// Inner middleware transforms
	c.SetResponseBody([]byte("inner-transformed"))

	// Inner FlushResponse — depth goes from 2 to 1, no write
	if err := c.FlushResponse(); err != nil {
		t.Fatal(err)
	}
	if c.IsWritten() {
		t.Fatal("should still be buffered at depth 1")
	}

	// Outer middleware reads transformed body from inner
	body := c.ResponseBody()
	if string(body) != "inner-transformed" {
		t.Fatalf("expected inner-transformed, got %s", body)
	}
	c.SetResponseBody([]byte("outer-transformed"))

	// Outer FlushResponse — depth goes from 1 to 0, writes
	if err := c.FlushResponse(); err != nil {
		t.Fatal(err)
	}
	if !c.IsWritten() {
		t.Fatal("should be written at depth 0")
	}
	if string(rw.body) != "outer-transformed" {
		t.Fatalf("expected outer-transformed, got %s", rw.body)
	}
}

func TestBufferResponseAutoFlush(t *testing.T) {
	s, rw := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.BufferResponse()
	if err := c.Blob(200, "text/plain", []byte("autoflush")); err != nil {
		t.Fatal(err)
	}

	// Simulate safety net: middleware forgot to flush
	if c.buffered && !c.written {
		c.bufferDepth = 1
		_ = c.FlushResponse()
	}

	if !c.IsWritten() {
		t.Fatal("auto-flush should have written")
	}
	if string(rw.body) != "autoflush" {
		t.Fatalf("expected autoflush body, got %s", rw.body)
	}
}

func TestBufferResponseNoContent(t *testing.T) {
	s, rw := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.BufferResponse()
	c.SetHeader("location", "/new")
	if err := c.NoContent(302); err != nil {
		t.Fatal(err)
	}

	if c.IsWritten() {
		t.Fatal("should be buffered")
	}
	if c.ResponseStatus() != 302 {
		t.Fatalf("expected 302, got %d", c.ResponseStatus())
	}

	if err := c.FlushResponse(); err != nil {
		t.Fatal(err)
	}
	if rw.status != 302 {
		t.Fatalf("expected 302 on wire, got %d", rw.status)
	}
}

func TestBufferResponseNotUsed(t *testing.T) {
	s, rw := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// No BufferResponse — Blob should write directly
	if err := c.Blob(200, "text/plain", []byte("direct")); err != nil {
		t.Fatal(err)
	}
	if !c.IsWritten() {
		t.Fatal("should be written immediately")
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	if string(rw.body) != "direct" {
		t.Fatalf("expected direct, got %s", rw.body)
	}
}

func TestFlushResponseExtraCalls(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// FlushResponse without BufferResponse should be a safe no-op.
	err := c.FlushResponse()
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	// bufferDepth should not go negative.
	if c.bufferDepth != 0 {
		t.Fatalf("expected bufferDepth 0, got %d", c.bufferDepth)
	}

	// After a normal write, extra FlushResponse returns ErrResponseWritten.
	_ = c.Blob(200, "text/plain", []byte("ok"))
	err = c.FlushResponse()
	if err != ErrResponseWritten {
		t.Fatalf("expected ErrResponseWritten, got %v", err)
	}
}

func TestStreamOverflow(t *testing.T) {
	s, _ := newTestStream("GET", "/stream")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// Create a reader that exceeds 100MB.
	big := make([]byte, maxStreamBodySize+1)
	err := c.Stream(200, "application/octet-stream", bytes.NewReader(big))
	if err == nil {
		t.Fatal("expected error for stream overflow")
	}
	if !strings.Contains(err.Error(), "100MB") {
		t.Fatalf("expected 100MB error, got %v", err)
	}
}

func TestStreamWriterNilWithoutStreamer(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	sw := c.StreamWriter()
	if sw != nil {
		t.Fatal("expected nil StreamWriter from mock response writer")
	}
}

// --- File serving tests ---

func TestContextFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	_ = os.WriteFile(path, []byte("hello file"), 0644)

	s, rw := newTestStream("GET", "/file")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.File(path); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	if string(rw.body) != "hello file" {
		t.Fatalf("expected 'hello file', got '%s'", string(rw.body))
	}
}

func TestContextFileMissing(t *testing.T) {
	s, _ := newTestStream("GET", "/file")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	err := c.File("/nonexistent/file.txt")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestContextFileContentType(t *testing.T) {
	tests := []struct {
		name    string
		ext     string
		wantCT  string
		content string
	}{
		{"html", ".html", "text/html", "<h1>Hello</h1>"},
		{"css", ".css", "text/css", "body {}"},
		{"json", ".json", "application/json", `{"a":1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file"+tt.ext)
			_ = os.WriteFile(path, []byte(tt.content), 0644)

			st, rw := newTestStream("GET", "/file")
			defer st.Release()

			c := acquireContext(st)
			defer releaseContext(c)

			if err := c.File(path); err != nil {
				t.Fatal(err)
			}

			var ct string
			for _, h := range rw.headers {
				if h[0] == "content-type" {
					ct = h[1]
					break
				}
			}
			if !strings.HasPrefix(ct, tt.wantCT) {
				t.Fatalf("expected content-type starting with %s, got %s", tt.wantCT, ct)
			}
		})
	}
}

func TestContextFileRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "range.txt")
	_ = os.WriteFile(path, []byte("0123456789"), 0644)

	s, rw := newTestStream("GET", "/file")
	s.Headers = append(s.Headers, [2]string{"range", "bytes=2-5"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.File(path); err != nil {
		t.Fatal(err)
	}
	if rw.status != 206 {
		t.Fatalf("expected 206, got %d", rw.status)
	}
	if string(rw.body) != "2345" {
		t.Fatalf("expected '2345', got '%s'", string(rw.body))
	}

	var cr string
	for _, h := range rw.headers {
		if h[0] == "content-range" {
			cr = h[1]
			break
		}
	}
	if cr != "bytes 2-5/10" {
		t.Fatalf("expected 'bytes 2-5/10', got %q", cr)
	}
}

func TestContextStream(t *testing.T) {
	s, rw := newTestStream("GET", "/stream")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	r := bytes.NewReader([]byte("streamed data"))
	if err := c.Stream(200, "text/plain", r); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	if string(rw.body) != "streamed data" {
		t.Fatalf("expected 'streamed data', got '%s'", string(rw.body))
	}
}

func TestContextFileFromDir(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0644)

	s, rw := newTestStream("GET", "/files/file.txt")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if err := c.FileFromDir(dir, "file.txt"); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	if string(rw.body) != "hello" {
		t.Fatalf("expected hello, got %s", string(rw.body))
	}
}

func TestContextFileFromDirTraversal(t *testing.T) {
	dir := t.TempDir()

	s, _ := newTestStream("GET", "/files/../../../etc/passwd")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	err := c.FileFromDir(dir, "../../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for traversal attempt")
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.Code != 400 {
		t.Fatalf("expected HTTPError 400, got %v", err)
	}
}

func TestNegotiateJSON(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"accept", "application/json"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	got := c.Negotiate("application/json", "application/xml")
	if got != "application/json" {
		t.Fatalf("expected application/json, got %q", got)
	}
}

func TestNegotiateXML(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"accept", "application/xml"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	got := c.Negotiate("application/json", "application/xml")
	if got != "application/xml" {
		t.Fatalf("expected application/xml, got %q", got)
	}
}

func TestNegotiateWildcard(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"accept", "*/*"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	got := c.Negotiate("application/json", "text/plain")
	if got != "application/json" {
		t.Fatalf("expected first offer, got %q", got)
	}
}

func TestNegotiateQuality(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"accept", "text/plain;q=0.9, application/json;q=1.0"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	got := c.Negotiate("application/json", "text/plain")
	if got != "application/json" {
		t.Fatalf("expected application/json (higher q), got %q", got)
	}
}

func TestNegotiateNoAccept(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	got := c.Negotiate("application/json", "text/plain")
	if got != "application/json" {
		t.Fatalf("expected first offer as default, got %q", got)
	}
}

func TestNegotiateTieBreakAcceptOrder(t *testing.T) {
	// RFC 7231: equal quality values should prefer Accept header order.
	s, _ := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"accept", "application/xml, application/json"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	// Both have q=1.0, but XML appears first in Accept header.
	best := c.Negotiate("application/json", "application/xml")
	if best != "application/xml" {
		t.Fatalf("expected application/xml (first in Accept), got %q", best)
	}
}

func TestRespondJSON(t *testing.T) {
	s, rw := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"accept", "application/json"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	err := c.Respond(200, map[string]string{"key": "val"})
	if err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	var hasJSON bool
	for _, h := range rw.headers {
		if h[0] == "content-type" && h[1] == "application/json" {
			hasJSON = true
		}
	}
	if !hasJSON {
		t.Fatal("expected application/json content-type")
	}
}

func TestRespondXML(t *testing.T) {
	s, rw := newTestStream("GET", "/test")
	s.Headers = append(s.Headers, [2]string{"accept", "application/xml"})
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	type item struct {
		Name string
	}
	err := c.Respond(200, item{Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 {
		t.Fatalf("expected 200, got %d", rw.status)
	}
	var hasXML bool
	for _, h := range rw.headers {
		if h[0] == "content-type" && h[1] == "application/xml" {
			hasXML = true
		}
	}
	if !hasXML {
		t.Fatal("expected application/xml content-type")
	}
}

func TestContextSetCookieDeleteExpired(t *testing.T) {
	s, rw := newTestStream("GET", "/cookie-delete")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.SetCookie(&Cookie{
		Name:   "session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	_ = c.NoContent(200)

	var cookieHeader string
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			cookieHeader = h[1]
			break
		}
	}

	expected := "session=; Path=/; Max-Age=0"
	if cookieHeader != expected {
		t.Fatalf("expected %q, got %q", expected, cookieHeader)
	}
}

func TestContextSetCookieDomainAndSameSite(t *testing.T) {
	s, rw := newTestStream("GET", "/cookie-domain")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.SetCookie(&Cookie{
		Name:     "session",
		Value:    "abc",
		Path:     "/",
		Domain:   "example.com",
		MaxAge:   3600,
		HTTPOnly: true,
		Secure:   true,
		SameSite: SameSiteStrictMode,
	})
	_ = c.NoContent(200)

	var cookieHeader string
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			cookieHeader = h[1]
			break
		}
	}

	expected := "session=abc; Path=/; Domain=example.com; Max-Age=3600; HttpOnly; Secure; SameSite=Strict"
	if cookieHeader != expected {
		t.Fatalf("expected %q, got %q", expected, cookieHeader)
	}
}

func TestContextSetCookieSameSiteLax(t *testing.T) {
	s, rw := newTestStream("GET", "/cookie-lax")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.SetCookie(&Cookie{
		Name:     "pref",
		Value:    "dark",
		Path:     "/",
		SameSite: SameSiteLaxMode,
	})
	_ = c.NoContent(200)

	var cookieHeader string
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			cookieHeader = h[1]
			break
		}
	}

	expected := "pref=dark; Path=/; SameSite=Lax"
	if cookieHeader != expected {
		t.Fatalf("expected %q, got %q", expected, cookieHeader)
	}
}

func TestContextSetCookieSameSiteNone(t *testing.T) {
	s, rw := newTestStream("GET", "/cookie-none")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.SetCookie(&Cookie{
		Name:     "track",
		Value:    "x",
		Path:     "/",
		Secure:   true,
		SameSite: SameSiteNoneMode,
	})
	_ = c.NoContent(200)

	var cookieHeader string
	for _, h := range rw.headers {
		if h[0] == "set-cookie" {
			cookieHeader = h[1]
			break
		}
	}

	expected := "track=x; Path=/; Secure; SameSite=None"
	if cookieHeader != expected {
		t.Fatalf("expected %q, got %q", expected, cookieHeader)
	}
}

func TestHijackNotSupported(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	conn, err := c.Hijack()
	if conn != nil {
		t.Fatal("expected nil conn")
	}
	if !errors.Is(err, ErrHijackNotSupported) {
		t.Fatalf("expected ErrHijackNotSupported, got %v", err)
	}
}

func TestHijackAfterWrite(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_ = c.Blob(200, "text/plain", []byte("hello"))
	conn, err := c.Hijack()
	if conn != nil {
		t.Fatal("expected nil conn after write")
	}
	if err == nil || err.Error() != "celeris: cannot hijack after response written" {
		t.Fatalf("expected hijack-after-write error, got %v", err)
	}
}

func TestHijackWithHijacker(t *testing.T) {
	s := stream.NewStream(1)
	s.Headers = [][2]string{
		{":method", "GET"},
		{":path", "/ws"},
		{":scheme", "http"},
		{":authority", "localhost"},
	}
	defer s.Release()

	// Create a mock hijacker response writer
	hijacked := false
	rw := &mockHijackerResponseWriter{
		mockResponseWriter: mockResponseWriter{},
		hijackFn: func(_ *stream.Stream) (net.Conn, error) {
			hijacked = true
			// Return a pipe connection for testing
			c1, _ := net.Pipe()
			return c1, nil
		},
	}
	s.ResponseWriter = rw

	c := acquireContext(s)
	defer releaseContext(c)

	conn, err := c.Hijack()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn == nil {
		t.Fatal("expected non-nil conn")
	}
	_ = conn.Close()
	if !hijacked {
		t.Fatal("expected hijackFn to be called")
	}
	if !c.IsWritten() {
		t.Fatal("expected written=true after hijack")
	}
}

func TestIsWritten(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	if c.IsWritten() {
		t.Fatal("should be false before response")
	}
	_ = c.Blob(200, "text/plain", []byte("hello"))
	if !c.IsWritten() {
		t.Fatal("should be true after response")
	}
}

func TestBytesWritten(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	data := []byte(`{"key":"value"}`)
	_ = c.Blob(200, "application/json", data)
	if c.BytesWritten() != len(data) {
		t.Fatalf("expected %d bytes, got %d", len(data), c.BytesWritten())
	}
}

func TestBytesWrittenNoContent(t *testing.T) {
	s, _ := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	_ = c.NoContent(204)
	if c.BytesWritten() != 0 {
		t.Fatalf("expected 0 bytes for NoContent, got %d", c.BytesWritten())
	}
}

func TestResponseStatus(t *testing.T) {
	s, rw := newTestStream("GET", "/test")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.BufferResponse()
	_ = c.JSON(201, map[string]string{"id": "1"})

	if c.ResponseStatus() != 201 {
		t.Fatalf("expected 201, got %d", c.ResponseStatus())
	}

	_ = c.FlushResponse()
	if rw.status != 201 {
		t.Fatalf("expected 201 on wire, got %d", rw.status)
	}
}

// --- Convenience method tests ---

func TestAttachment(t *testing.T) {
	s, rw := newTestStream("GET", "/download")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.Attachment("report.pdf")
	_ = c.Blob(200, "application/pdf", []byte("content"))

	var cd string
	for _, h := range rw.headers {
		if h[0] == "content-disposition" {
			cd = h[1]
			break
		}
	}
	if cd != `attachment; filename="report.pdf"` {
		t.Fatalf("expected attachment with filename, got %q", cd)
	}
}

func TestAttachmentQuotedFilename(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     string
	}{
		{"filename with quotes", `report "final".pdf`, `attachment; filename="report \"final\".pdf"`},
		{"filename with backslash", `path\to\file.pdf`, `attachment; filename="path\\to\\file.pdf"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, rw := newTestStream("GET", "/download")
			defer s.Release()

			c := acquireContext(s)
			defer releaseContext(c)

			c.Attachment(tt.filename)
			_ = c.Blob(200, "application/pdf", []byte("content"))

			var cd string
			for _, h := range rw.headers {
				if h[0] == "content-disposition" {
					cd = h[1]
					break
				}
			}
			if cd != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, cd)
			}
		})
	}
}

func TestAttachmentNoFilename(t *testing.T) {
	s, rw := newTestStream("GET", "/download")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.Attachment("")
	_ = c.Blob(200, "application/pdf", []byte("content"))

	var cd string
	for _, h := range rw.headers {
		if h[0] == "content-disposition" {
			cd = h[1]
			break
		}
	}
	if cd != "attachment" {
		t.Fatalf("expected 'attachment', got %q", cd)
	}
}

func TestInline(t *testing.T) {
	s, rw := newTestStream("GET", "/view")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.Inline("image.png")
	_ = c.Blob(200, "image/png", []byte("content"))

	var cd string
	for _, h := range rw.headers {
		if h[0] == "content-disposition" {
			cd = h[1]
			break
		}
	}
	if cd != `inline; filename="image.png"` {
		t.Fatalf("expected inline with filename, got %q", cd)
	}
}

func TestInlineQuotedFilename(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     string
	}{
		{"filename with quotes", `doc "v2".pdf`, `inline; filename="doc \"v2\".pdf"`},
		{"filename with backslash", `path\to\file.pdf`, `inline; filename="path\\to\\file.pdf"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, rw := newTestStream("GET", "/view")
			defer s.Release()

			c := acquireContext(s)
			defer releaseContext(c)

			c.Inline(tt.filename)
			_ = c.Blob(200, "image/png", []byte("content"))

			var cd string
			for _, h := range rw.headers {
				if h[0] == "content-disposition" {
					cd = h[1]
					break
				}
			}
			if cd != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, cd)
			}
		})
	}
}

func TestInlineNoFilename(t *testing.T) {
	s, rw := newTestStream("GET", "/view")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	c.Inline("")
	_ = c.Blob(200, "image/png", []byte("content"))

	var cd string
	for _, h := range rw.headers {
		if h[0] == "content-disposition" {
			cd = h[1]
			break
		}
	}
	if cd != "inline" {
		t.Fatalf("expected 'inline', got %q", cd)
	}
}
