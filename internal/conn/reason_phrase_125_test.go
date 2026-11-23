package conn

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#889 item 125: the native HTTP/1 engines sent the reason phrase
// "Unknown" for 405, 206, 408, 412, 416 and 429. Every status line they write
// carries the standard text, net/http's http.StatusText, for every status code
// that has one. These tests run each H1 response writer over every registered
// code (100 to 599) and compare the line with http.StatusText.

// registered125 returns every status code in [lo, hi] that http.StatusText names.
func registered125(lo, hi int) []int {
	var codes []int
	for c := lo; c <= hi; c++ {
		if http.StatusText(c) != "" {
			codes = append(codes, c)
		}
	}
	return codes
}

// statusLine125 returns the first line of a response head, without its CRLF.
func statusLine125(t *testing.T, out []byte) string {
	t.Helper()
	line, _, ok := bytes.Cut(out, []byte("\r\n"))
	if !ok {
		t.Fatalf("no status line in %q", out)
	}
	return string(line)
}

func want125(code int) string {
	return fmt.Sprintf("HTTP/1.1 %d %s", code, http.StatusText(code))
}

func TestStatusTextIsNetHTTPs125(t *testing.T) {
	codes := registered125(100, 599)
	if len(codes) < 50 {
		t.Fatalf("only %d registered codes: the table is not covering the range", len(codes))
	}
	for _, c := range codes {
		if got, want := statusText(c), http.StatusText(c); got != want {
			t.Errorf("statusText(%d) = %q, want %q", c, got, want)
		}
	}
	// The six codes the issue names.
	for _, c := range []int{405, 206, 408, 412, 416, 429} {
		if got := statusText(c); got == "Unknown" || got != http.StatusText(c) {
			t.Errorf("statusText(%d) = %q", c, got)
		}
	}
	// A code http.StatusText has no text for keeps a non-empty phrase.
	for _, c := range []int{299, 399, 499, 599} {
		if http.StatusText(c) != "" {
			continue
		}
		if got := statusText(c); got != "Unknown" {
			t.Errorf("statusText(%d) = %q, want Unknown for an unregistered code", c, got)
		}
	}
}

func TestH1StatusLineEveryWriter125(t *testing.T) {
	var s *stream.Stream
	run := func(t *testing.T, codes []int, do func(a *h1ResponseAdapter, code int) error) {
		t.Helper()
		for _, c := range codes {
			var out bytes.Buffer
			a := &h1ResponseAdapter{write: func(p []byte) { out.Write(p) }, keepAlive: true}
			if err := do(a, c); err != nil {
				t.Fatalf("%d: %v", c, err)
			}
			if got, want := statusLine125(t, out.Bytes()), want125(c); got != want {
				t.Errorf("%d: status line %q, want %q", c, got, want)
			}
		}
	}
	all := registered125(100, 599)
	final := registered125(200, 599)

	t.Run("appendStatusLine", func(t *testing.T) {
		for _, c := range all {
			if got, want := strings.TrimSuffix(string(appendStatusLine(nil, c)), "\r\n"), want125(c); got != want {
				t.Errorf("%d: %q, want %q", c, got, want)
			}
		}
	})
	t.Run("WriteResponse/blob-fast-path", func(t *testing.T) {
		run(t, final, func(a *h1ResponseAdapter, c int) error {
			return a.WriteResponse(s, c, [][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
		})
	})
	t.Run("WriteResponse/general-path", func(t *testing.T) {
		run(t, final, func(a *h1ResponseAdapter, c int) error {
			return a.WriteResponse(s, c, [][2]string{{"content-type", "text/plain"}, {"x-a", "1"}, {"x-b", "2"}}, []byte("ok"))
		})
	})
	t.Run("WriteResponse/no-headers-no-body", func(t *testing.T) {
		run(t, final, func(a *h1ResponseAdapter, c int) error {
			return a.WriteResponse(s, c, nil, nil)
		})
	})
	t.Run("WriteResponse/interim", func(t *testing.T) {
		run(t, registered125(100, 199), func(a *h1ResponseAdapter, c int) error {
			return a.WriteResponse(s, c, nil, nil)
		})
	})
	t.Run("WriteResponse/head", func(t *testing.T) {
		run(t, final, func(a *h1ResponseAdapter, c int) error {
			a.isHEAD = true
			return a.WriteResponse(s, c, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
		})
	})
	t.Run("WriteResponse/closing", func(t *testing.T) {
		run(t, final, func(a *h1ResponseAdapter, c int) error {
			a.keepAlive = false
			return a.WriteResponse(s, c, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
		})
	})
	t.Run("WriteHeader/stream", func(t *testing.T) {
		run(t, all, func(a *h1ResponseAdapter, c int) error {
			return a.WriteHeader(s, c, [][2]string{{"content-type", "text/plain"}})
		})
	})
	t.Run("appendFileResponseHead", func(t *testing.T) {
		for _, c := range final {
			a := &h1ResponseAdapter{keepAlive: true}
			out := a.appendFileResponseHead(nil, c, [][2]string{{"content-type", "text/plain"}}, 10)
			if got, want := statusLine125(t, out), want125(c); got != want {
				t.Errorf("%d: status line %q, want %q", c, got, want)
			}
		}
	})
	t.Run("writeErrorResponse", func(t *testing.T) {
		for _, c := range registered125(400, 599) {
			var out bytes.Buffer
			writeErrorResponse(func(p []byte) { out.Write(p) }, c, "message")
			if got, want := statusLine125(t, out.Bytes()), want125(c); got != want {
				t.Errorf("%d: status line %q, want %q", c, got, want)
			}
		}
	})
}

// The same over ProcessH1, the request parser and handler loop the epoll,
// io_uring and adaptive engines all run: a handler answering each code through
// the response writer, and the 400 and 413 the parser itself writes.
func TestProcessH1StatusLineEveryCode125(t *testing.T) {
	for _, c := range registered125(200, 599) {
		h := stream.HandlerFunc(func(_ context.Context, s *stream.Stream) error {
			return s.ResponseWriter.WriteResponse(s, c, [][2]string{{"content-type", "text/plain"}}, []byte("x"))
		})
		var buf bytes.Buffer
		err := ProcessH1(context.Background(), []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"), NewH1State(), h, func(b []byte) { buf.Write(b) })
		if err != nil {
			t.Fatalf("%d: ProcessH1: %v", c, err)
		}
		if got, want := statusLine125(t, buf.Bytes()), want125(c); got != want {
			t.Errorf("%d: status line %q, want %q", c, got, want)
		}
	}

	// A malformed request line: the parser's own 400.
	h := stream.HandlerFunc(func(_ context.Context, _ *stream.Stream) error { return nil })
	var buf bytes.Buffer
	_ = ProcessH1(context.Background(), []byte("GARBAGE\r\n\r\n"), NewH1State(), h, func(b []byte) { buf.Write(b) })
	if got, want := statusLine125(t, buf.Bytes()), want125(400); got != want {
		t.Errorf("parser 400: status line %q, want %q", got, want)
	}
}
