package h1

import (
	"errors"
	"strings"
	"testing"
)

// celeris#889, items 40 and 45: the request line. The method is a token (RFC
// 9110 section 5.6.2) and the request-target holds no control byte; a request
// line that breaks either is invalid (RFC 9112 section 3), a 400.

func parseLine889(line string) (Request, error) {
	p := NewParser()
	p.Reset([]byte(line + "\r\nHost: x\r\n\r\n"))
	var req Request
	_, err := p.ParseRequest(&req)
	return req, err
}

func TestParseRequest_MethodIsAToken889(t *testing.T) {
	bad := []struct{ name, line string }{
		{"empty method", " / HTTP/1.1"},
		{"leading LF", "\nGET / HTTP/1.1"},
		{"leading NUL", "\x00GET / HTTP/1.1"},
		{"NUL inside", "G\x00T / HTTP/1.1"},
		{"bare CR at the end", "GET\r / HTTP/1.1"},
		{"HTAB at the end", "GET\t / HTTP/1.1"},
		{"delimiter paren", "G(T / HTTP/1.1"},
		{"delimiter comma", "GET, / HTTP/1.1"},
		{"delimiter quote", `"GET" / HTTP/1.1`},
		{"delimiter slash", "GE/T / HTTP/1.1"},
		{"obs-text", "G\xc3\xa9T / HTTP/1.1"},
		{"DEL", "GE\x7fT / HTTP/1.1"},
		// A method that looks like an interned one but is not: the interning
		// compares every byte, so none of these may take the fast path.
		{"GET plus NUL", "GET\x00 / HTTP/1.1"},
		{"POST plus LF", "POST\n / HTTP/1.1"},
	}
	for _, tc := range bad {
		t.Run("rejects/"+tc.name, func(t *testing.T) {
			if _, err := parseLine889(tc.line); !errors.Is(err, ErrInvalidRequestLine) {
				t.Fatalf("err = %v, want ErrInvalidRequestLine", err)
			}
		})
	}
	good := []string{
		"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "CONNECT", "TRACE",
		"PROPFIND", "M-SEARCH", "X_CUSTOM", "get", "A", "a.b~c!d#e$f%g&h'i*j+k^l`m|n",
		strings.Repeat("X", 200),
	}
	for _, m := range good {
		t.Run("accepts/"+m[:min(len(m), 12)], func(t *testing.T) {
			req, err := parseLine889(m + " / HTTP/1.1")
			if err != nil {
				t.Fatalf("err = %v, want none", err)
			}
			if req.Method != m {
				t.Fatalf("Method = %q, want %q", req.Method, m)
			}
		})
	}
}

func TestParseRequest_TargetHasNoControlByte889(t *testing.T) {
	for _, c := range []byte{0x00, 0x01, '\t', '\n', 0x0b, '\r', 0x1f, 0x7f} {
		line := "GET /p" + string([]byte{c}) + "e=gzip HTTP/1.1"
		if _, err := parseLine889(line); !errors.Is(err, ErrInvalidRequestLine) {
			t.Errorf("target byte 0x%02x: err = %v, want ErrInvalidRequestLine", c, err)
		}
	}
	// The same byte at the start and at the end of the target, and as the
	// whole target.
	for _, tgt := range []string{"\x00", "\x00/", "/\x00", "/\r", "\n/", "*\x00", "http://h/\npath"} {
		if _, err := parseLine889("GET " + tgt + " HTTP/1.1"); !errors.Is(err, ErrInvalidRequestLine) {
			t.Errorf("target %q: err = %v, want ErrInvalidRequestLine", tgt, err)
		}
	}
	// What stays valid: every form of target (RFC 9112 section 3.2), a
	// percent-encoded NUL (it is text, not the byte), and bytes above 0x7f
	// (UTF-8 in a path is common and net/http serves it).
	for _, tgt := range []string{
		"/", "*", "/a/b?c=d&e=f", "/p%00e=gzip", "/p%0d%0a", "http://example.com/x", "example.com:443",
		"/caf\xc3\xa9", "/~user/a;b=c,d", "/\x21\x7e", "/" + strings.Repeat("a", 4000),
	} {
		req, err := parseLine889("GET " + tgt + " HTTP/1.1")
		if err != nil {
			t.Errorf("target %q: err = %v, want none", tgt, err)
			continue
		}
		if req.Path != tgt {
			t.Errorf("target %q: Path = %q", tgt, req.Path)
		}
	}
}

// The failing request is rejected whatever comes before it on the buffer:
// the leading-CRLF skip (RFC 9112 section 2.2) does not hide it.
func TestParseRequest_BadMethodAfterLeadingCRLF889(t *testing.T) {
	p := NewParser()
	p.Reset([]byte("\r\n\nGET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	var req Request
	if _, err := p.ParseRequest(&req); !errors.Is(err, ErrInvalidRequestLine) {
		t.Fatalf("err = %v, want ErrInvalidRequestLine", err)
	}
}
