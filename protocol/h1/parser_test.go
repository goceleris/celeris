package h1

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseRequest_AllMethods(t *testing.T) {
	methods := []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			raw := method + " /path HTTP/1.1\r\nHost: example.com\r\n\r\n"
			p := NewParser()
			p.Reset([]byte(raw))
			var req Request
			n, err := p.ParseRequest(&req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n != len(raw) {
				t.Fatalf("consumed %d, want %d", n, len(raw))
			}
			if req.Method != method {
				t.Fatalf("method = %q, want %q", req.Method, method)
			}
			if req.Path != "/path" {
				t.Fatalf("path = %q, want /path", req.Path)
			}
			if req.Version != "HTTP/1.1" {
				t.Fatalf("version = %q, want HTTP/1.1", req.Version)
			}
			if req.Host != "example.com" {
				t.Fatalf("host = %q, want example.com", req.Host)
			}
			if !req.HeadersComplete {
				t.Fatal("headers not complete")
			}
		})
	}
}

func TestParseRequest_HTTP10(t *testing.T) {
	raw := "GET / HTTP/1.0\r\nHost: example.com\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	n, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(raw) {
		t.Fatalf("consumed %d, want %d", n, len(raw))
	}
	if req.Version != "HTTP/1.0" {
		t.Fatalf("version = %q, want HTTP/1.0", req.Version)
	}
	if req.KeepAlive {
		t.Fatal("HTTP/1.0 should default to keep-alive=false")
	}
}

func TestParseRequest_HTTP11KeepAlive(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !req.KeepAlive {
		t.Fatal("HTTP/1.1 should default to keep-alive=true")
	}
}

func TestParseRequest_ConnectionClose(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.KeepAlive {
		t.Fatal("connection: close should set keep-alive=false")
	}
}

func TestParseRequest_ConnectionKeepAlive(t *testing.T) {
	raw := "GET / HTTP/1.0\r\nHost: example.com\r\nConnection: keep-alive\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !req.KeepAlive {
		t.Fatal("connection: keep-alive should set keep-alive=true")
	}
}

func TestParseRequest_ContentLength(t *testing.T) {
	raw := "POST /data HTTP/1.1\r\nHost: example.com\r\nContent-Length: 13\r\n\r\nHello, World!"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.ContentLength != 13 {
		t.Fatalf("content-length = %d, want 13", req.ContentLength)
	}
	body := p.GetBody(req.ContentLength)
	if string(body) != "Hello, World!" {
		t.Fatalf("body = %q, want Hello, World!", body)
	}
}

func TestParseRequest_ChunkedEncoding(t *testing.T) {
	raw := "POST /upload HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !req.ChunkedEncoding {
		t.Fatal("chunked encoding not detected")
	}
	if req.ContentLength != -1 {
		t.Fatalf("content-length = %d, want -1 for chunked", req.ContentLength)
	}
}

func TestParseRequest_Pipelining(t *testing.T) {
	raw := "GET /first HTTP/1.1\r\nHost: example.com\r\n\r\n" +
		"GET /second HTTP/1.1\r\nHost: example.com\r\n\r\n" +
		"POST /third HTTP/1.1\r\nHost: example.com\r\nContent-Length: 4\r\n\r\nbody"
	p := NewParser()
	p.Reset([]byte(raw))

	var req Request

	// First request
	n1, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("request 1: %v", err)
	}
	if req.Path != "/first" {
		t.Fatalf("request 1 path = %q, want /first", req.Path)
	}
	if n1 == 0 {
		t.Fatal("request 1 consumed 0 bytes")
	}

	// Second request
	req.Reset()
	n2, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("request 2: %v", err)
	}
	if req.Path != "/second" {
		t.Fatalf("request 2 path = %q, want /second", req.Path)
	}
	if n2 == 0 {
		t.Fatal("request 2 consumed 0 bytes")
	}

	// Third request
	req.Reset()
	_, err = p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("request 3: %v", err)
	}
	if req.Path != "/third" {
		t.Fatalf("request 3 path = %q, want /third", req.Path)
	}
	if req.ContentLength != 4 {
		t.Fatalf("request 3 content-length = %d, want 4", req.ContentLength)
	}
	body := p.GetBody(req.ContentLength)
	if string(body) != "body" {
		t.Fatalf("request 3 body = %q, want body", body)
	}
}

func TestParseRequest_MalformedRequestLine(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		err  error
	}{
		{"no spaces", "GETHTTP/1.1\r\nHost: x\r\n\r\n", ErrInvalidRequestLine},
		{"one space", "GET /path\r\nHost: x\r\n\r\n", ErrInvalidRequestLine},
		{"bad version", "GET / HTTP/2.0\r\nHost: x\r\n\r\n", ErrUnsupportedVersion},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewParser()
			p.Reset([]byte(tc.raw))
			var req Request
			_, err := p.ParseRequest(&req)
			if !errors.Is(err, tc.err) {
				t.Fatalf("got error %v, want %v", err, tc.err)
			}
		})
	}
}

func TestParseRequest_MissingHost(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nContent-Length: 0\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if !errors.Is(err, ErrMissingHost) {
		t.Fatalf("got error %v, want %v", err, ErrMissingHost)
	}
}

func TestParseRequest_InvalidHeader(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nBadHeader\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if !errors.Is(err, ErrInvalidHeader) {
		t.Fatalf("got error %v, want %v", err, ErrInvalidHeader)
	}
}

func TestParseRequest_InvalidContentLength(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: example.com\r\nContent-Length: abc\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if !errors.Is(err, ErrInvalidContentLength) {
		t.Fatalf("got error %v, want %v", err, ErrInvalidContentLength)
	}
}

func TestParseRequest_CaseInsensitiveHeaders(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHOST: example.com\r\nCONTENT-LENGTH: 5\r\ntRANSFER-eNCODING: identity\r\nCONNECTION: keep-alive\r\n\r\nhello"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Host != "example.com" {
		t.Fatalf("host = %q, want example.com", req.Host)
	}
	if req.ContentLength != 5 {
		t.Fatalf("content-length = %d, want 5", req.ContentLength)
	}
	if !req.KeepAlive {
		t.Fatal("keep-alive should be true")
	}
}

func TestParseRequest_PartialData(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: exa"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	n, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error on partial data: %v", err)
	}
	if n != 0 {
		t.Fatalf("consumed = %d, want 0 for partial", n)
	}
}

func TestParseRequest_BufferExhausted(t *testing.T) {
	p := NewParser()
	p.Reset([]byte{})
	var req Request
	_, err := p.ParseRequest(&req)
	if !errors.Is(err, ErrBufferExhausted) {
		t.Fatalf("got error %v, want %v", err, ErrBufferExhausted)
	}
}

func TestParseRequest_ManyHeaders(t *testing.T) {
	var b strings.Builder
	b.WriteString("GET / HTTP/1.1\r\n")
	b.WriteString("Host: example.com\r\n")
	for i := 0; i < 100; i++ {
		b.WriteString(fmt.Sprintf("X-Header-%d: value-%d\r\n", i, i))
	}
	b.WriteString("\r\n")
	raw := b.String()

	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.Headers) != 101 {
		t.Fatalf("header count = %d, want 101", len(req.Headers))
	}
}

func TestParseRequest_InternedRoot(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Path != "/" {
		t.Fatalf("path = %q, want /", req.Path)
	}
}

func TestParseRequest_RawHeaders(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: example.com\r\nX-Custom: value\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.RawHeaders) != 2 {
		t.Fatalf("raw header count = %d, want 2", len(req.RawHeaders))
	}
	if string(req.RawHeaders[1][0]) != "X-Custom" {
		t.Fatalf("raw header name = %q, want X-Custom", req.RawHeaders[1][0])
	}
	if string(req.RawHeaders[1][1]) != "value" {
		t.Fatalf("raw header value = %q, want value", req.RawHeaders[1][1])
	}
}

func TestParseRequest_Remaining(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\n\r\nhello"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Remaining() != 5 {
		t.Fatalf("remaining = %d, want 5", p.Remaining())
	}
}

func TestParseRequest_ConsumeBody(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\n\r\nhelloextra"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p.ConsumeBody(5)
	if p.Remaining() != 5 {
		t.Fatalf("remaining after consume = %d, want 5", p.Remaining())
	}
}

func TestRequest_Reset(t *testing.T) {
	req := &Request{
		Method:          "GET",
		Path:            "/",
		Version:         "HTTP/1.1",
		Headers:         make([][2]string, 5),
		RawHeaders:      make([][2][]byte, 5),
		Host:            "example.com",
		ContentLength:   42,
		ChunkedEncoding: true,
		KeepAlive:       true,
		HeadersComplete: true,
		BodyRead:        100,
	}
	req.Reset()
	if req.Method != "" || req.Path != "" || req.Version != "" {
		t.Fatal("strings not cleared")
	}
	if len(req.Headers) != 0 || len(req.RawHeaders) != 0 {
		t.Fatal("slices not cleared")
	}
	if req.Host != "" || req.ContentLength != 0 || req.ChunkedEncoding || req.KeepAlive || req.HeadersComplete || req.BodyRead != 0 {
		t.Fatal("fields not cleared")
	}
	if cap(req.Headers) != 5 {
		t.Fatal("headers backing array should be preserved")
	}
}

func TestParseRequest_NoStringHeadersMode(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 0\r\n\r\n"
	p := NewParser()
	p.noStringHeaders = true
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.Headers) != 0 {
		t.Fatalf("string headers should be empty in noStringHeaders mode, got %d", len(req.Headers))
	}
	if req.Host != "example.com" {
		t.Fatalf("host = %q, want example.com", req.Host)
	}
	if req.ContentLength != 0 {
		t.Fatalf("content-length = %d, want 0", req.ContentLength)
	}
}

func TestParseRequest_DefaultContentLengthMinusOne(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"
	p := NewParser()
	p.Reset([]byte(raw))
	var req Request
	_, err := p.ParseRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.ContentLength != -1 {
		t.Fatalf("default content-length = %d, want -1", req.ContentLength)
	}
}

// TestFindHeaderEnd_AllPositions places \r\n\r\n at every possible offset in
// buffers of varying sizes. This exercises AVX2 (32-byte), SSE2 (16-byte),
// and scalar code paths, including transition boundaries.
func TestFindHeaderEnd_AllPositions(t *testing.T) {
	// Sizes chosen to stress boundaries:
	//  4       — minimum, scalar only
	//  15      — just under one SSE2 block
	//  16      — exactly one SSE2 block (no room for \r\n\r\n verification)
	//  19      — first size that enters the SSE2 loop
	//  31      — just under one AVX2 block
	//  32      — exactly one AVX2 block
	//  35      — first size that enters the AVX2 loop
	//  48      — AVX2 + SSE2 tail
	//  64      — two full AVX2 iterations
	//  100     — AVX2 + SSE2 + scalar tail
	//  256     — multiple AVX2 iterations
	//  1024    — large buffer
	sizes := []int{4, 5, 15, 16, 17, 19, 20, 31, 32, 33, 35, 36, 48, 63, 64, 65, 100, 256, 1024}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("size=%d", size), func(t *testing.T) {
			for pos := 0; pos <= size-4; pos++ {
				buf := make([]byte, size)
				for i := range buf {
					buf[i] = 'A'
				}
				buf[pos] = '\r'
				buf[pos+1] = '\n'
				buf[pos+2] = '\r'
				buf[pos+3] = '\n'

				got := findHeaderEnd(buf)
				want := pos + 4
				if got != want {
					t.Fatalf("size=%d pos=%d: got %d, want %d", size, pos, got, want)
				}
			}
		})
	}
}

// TestFindHeaderEnd_NotFound verifies -1 is returned when no \r\n\r\n exists.
func TestFindHeaderEnd_NotFound(t *testing.T) {
	sizes := []int{0, 1, 2, 3, 4, 16, 32, 64, 128, 256}
	for _, size := range sizes {
		buf := make([]byte, size)
		for i := range buf {
			buf[i] = 'A'
		}
		got := findHeaderEnd(buf)
		if got != -1 {
			t.Fatalf("size=%d: got %d, want -1 (no \\r\\n\\r\\n present)", size, got)
		}
	}
}

// TestFindHeaderEnd_FalsePositiveCR verifies that lone \r bytes do not
// cause false positives. The buffer is filled with \r but no \r\n\r\n exists.
func TestFindHeaderEnd_FalsePositiveCR(t *testing.T) {
	sizes := []int{16, 32, 64, 128}
	for _, size := range sizes {
		buf := make([]byte, size)
		for i := range buf {
			buf[i] = '\r'
		}
		// No \n follows any \r, so no valid \r\n\r\n sequence.
		got := findHeaderEnd(buf)
		if got != -1 {
			t.Fatalf("size=%d (all \\r): got %d, want -1", size, got)
		}
	}
}

// TestFindHeaderEnd_MultipleCR verifies correct behavior when multiple \r
// bytes exist before the actual \r\n\r\n sequence.
func TestFindHeaderEnd_MultipleCR(t *testing.T) {
	buf := make([]byte, 128)
	for i := range buf {
		buf[i] = '\r'
	}
	// Place the real terminator at offset 100.
	buf[100] = '\r'
	buf[101] = '\n'
	buf[102] = '\r'
	buf[103] = '\n'
	got := findHeaderEnd(buf)
	if got != 104 {
		t.Fatalf("got %d, want 104", got)
	}
}

func TestParseRequest_DuplicateContentLength_Conflicting(t *testing.T) {
	for _, zeroCopy := range []bool{false, true} {
		name := "standard"
		if zeroCopy {
			name = "zerocopy"
		}
		t.Run(name, func(t *testing.T) {
			raw := "POST / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 0\r\nContent-Length: 50\r\n\r\n"
			p := NewParser()
			if zeroCopy {
				p.noStringHeaders = true
			}
			p.Reset([]byte(raw))
			var req Request
			_, err := p.ParseRequest(&req)
			if !errors.Is(err, ErrDuplicateContentLength) {
				t.Fatalf("got error %v, want %v", err, ErrDuplicateContentLength)
			}
		})
	}
}

func TestParseRequest_DuplicateContentLength_Identical(t *testing.T) {
	for _, zeroCopy := range []bool{false, true} {
		name := "standard"
		if zeroCopy {
			name = "zerocopy"
		}
		t.Run(name, func(t *testing.T) {
			raw := "POST / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 42\r\nContent-Length: 42\r\n\r\n"
			p := NewParser()
			if zeroCopy {
				p.noStringHeaders = true
			}
			p.Reset([]byte(raw))
			var req Request
			_, err := p.ParseRequest(&req)
			if err != nil {
				t.Fatalf("identical CL should be accepted, got: %v", err)
			}
			if req.ContentLength != 42 {
				t.Fatalf("content-length = %d, want 42", req.ContentLength)
			}
		})
	}
}

func TestParseRequest_DuplicateContentLength_ChunkedIgnored(t *testing.T) {
	for _, zeroCopy := range []bool{false, true} {
		name := "standard"
		if zeroCopy {
			name = "zerocopy"
		}
		t.Run(name, func(t *testing.T) {
			raw := "POST / HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\nContent-Length: 0\r\nContent-Length: 50\r\n\r\n"
			p := NewParser()
			if zeroCopy {
				p.noStringHeaders = true
			}
			p.Reset([]byte(raw))
			var req Request
			_, err := p.ParseRequest(&req)
			if err != nil {
				t.Fatalf("conflicting CL with chunked TE should be ignored, got: %v", err)
			}
			if !req.ChunkedEncoding {
				t.Fatal("chunked encoding not detected")
			}
			if req.ContentLength != -1 {
				t.Fatalf("content-length = %d, want -1 for chunked", req.ContentLength)
			}
		})
	}
}

// TestFindHeaderEnd_PartialSequence ensures partial \r\n sequences
// (without the full \r\n\r\n) are not mistakenly matched.
func TestFindHeaderEnd_PartialSequence(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"cr_only", "AAAA\rAAAA"},
		{"crlf_only", "AAAA\r\nAAAA"},
		{"crlf_cr", "AAAA\r\n\rAAAA"},
		{"lf_crlf", "AAAA\n\r\nAAAA"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := findHeaderEnd([]byte(tc.data))
			if got != -1 {
				t.Fatalf("got %d, want -1 for partial sequence", got)
			}
		})
	}
}
