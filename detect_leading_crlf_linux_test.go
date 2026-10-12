//go:build linux

package celeris_test

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// Tests for the first bytes of an HTTP/1 request in Auto mode: an empty line
// before the request line (RFC 9112 section 2.2: a server SHOULD ignore at
// least one) and the methods of the IANA registry beyond the nine of RFC 9110.
// Both closed the conn, or on a detect that decided but a parser that refused,
// answered 400. net/http, the std engine, skips leading CR and LF only after a
// POST, so a fresh conn that starts with an empty line is the one case where
// the engines and std differ; the std arm of that test logs what std does and
// does not assert it.

var crlfEngines = []engine974{{"std", celeris.Std, false, false}, {"epoll", celeris.Epoll, false, false}, {"io_uring", celeris.IOUring, false, false},
	{"epoll-async", celeris.Epoll, true, false}, {"io_uring-async", celeris.IOUring, true, false}, {"io_uring-mshot", celeris.IOUring, false, true}}

func startCRLF(t *testing.T, e engine974) string {
	t.Helper()
	e.env974(t)
	cfg := celeris.Config{Engine: e.eng, Protocol: celeris.Auto, AsyncHandlers: e.async}
	return startServerConfig761(t, cfg, func(s *celeris.Server) {
		s.POST("/echo", func(c *celeris.Context) error { return c.String(200, "posted") })
	})
}

// readUntilClose reads c to its end (the request carries Connection: close, or
// the server closes) and returns what it got.
func readUntilClose(c net.Conn, limit time.Duration) string {
	_ = c.SetReadDeadline(time.Now().Add(limit))
	b, _ := io.ReadAll(c)
	return string(b)
}

// TestLeadingEmptyLineIsIgnored: the engines answer a request that is
// preceded by CRLF, in one segment or split at every point of the lead.
func TestLeadingEmptyLineIsIgnored(t *testing.T) {
	const get = "GET /ping HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"
	segments := []struct {
		name string
		segs []string
	}{
		{"one-segment", []string{"\r\n" + get}},
		{"crlf-then-request", []string{"\r\n", get}},
		{"cr-then-lf-then-request", []string{"\r", "\n", get}},
		{"cr-then-lf-and-request", []string{"\r", "\n" + get}},
		{"crlf-and-G-then-rest", []string{"\r\nG", get[1:]}},
	}
	for _, e := range crlfEngines {
		for _, sc := range segments {
			t.Run(e.name+"/"+sc.name, func(t *testing.T) {
				addr := startCRLF(t, e)
				c, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				for _, s := range sc.segs {
					_, _ = io.WriteString(c, s)
					time.Sleep(100 * time.Millisecond)
				}
				got := readUntilClose(c, 5*time.Second)
				first, _, _ := strings.Cut(got, "\r\n")
				if e.eng == celeris.Std {
					// net/http skips an empty line only after a POST.
					t.Logf("std, a fresh conn that starts with an empty line: %q", first)
					return
				}
				if !strings.HasPrefix(got, "HTTP/1.1 200") || !strings.HasSuffix(got, "ok") {
					t.Errorf("%s: got %q, want a 200 answer", sc.name, got)
				}
			})
		}
	}
}

// TestEmptyLineBetweenKeepAliveRequests: a client that sends an empty line after
// a POST body (older clients and some proxies do) is answered on the next
// request. std is the control here: net/http skips CR and LF after a POST.
func TestEmptyLineBetweenKeepAliveRequests(t *testing.T) {
	const (
		post = "POST /echo HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\nab"
		get  = "GET /ping HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"
	)
	segments := []struct {
		name string
		segs []string
	}{
		{"one-segment", []string{post + "\r\n" + get}},
		{"crlf-own-segment", []string{post, "\r\n", get}},
		{"crlf-with-next", []string{post, "\r\n" + get}},
	}
	for _, e := range crlfEngines {
		for _, sc := range segments {
			t.Run(e.name+"/"+sc.name, func(t *testing.T) {
				addr := startCRLF(t, e)
				c, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				for _, s := range sc.segs {
					_, _ = io.WriteString(c, s)
					time.Sleep(100 * time.Millisecond)
				}
				got := readUntilClose(c, 5*time.Second)
				if strings.Count(got, "HTTP/1.1 200") != 2 || !strings.HasSuffix(got, "ok") {
					t.Errorf("%s: got %q, want the answers to both requests", sc.name, got)
				}
			})
		}
	}
}

// TestRegisteredMethodsAreServed: a method of the IANA registry outside the
// nine of RFC 9110 reaches the router and is answered (a 404 or a 405, not a
// close).
func TestRegisteredMethodsAreServed(t *testing.T) {
	for _, e := range crlfEngines {
		for _, m := range []string{"PROPFIND", "MKCOL", "SEARCH", "QUERY", "LOCK", "REPORT", "ACL", "UNLOCK", "VERSION-CONTROL"} {
			t.Run(e.name+"/"+m, func(t *testing.T) {
				addr := startCRLF(t, e)
				c, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				_, _ = io.WriteString(c, m+" /dav HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
				got := readUntilClose(c, 5*time.Second)
				if !strings.HasPrefix(got, "HTTP/1.1 ") {
					t.Errorf("%s: got %q, want an HTTP/1.1 answer", m, got)
				}
			})
		}
	}
}

// TestRegisteredFirstLetterNeedsHTTP: a first letter of the registry no longer
// closes the conn at detection, so bytes that are not a request ("SSH-", which
// starts with the registered S) reach the HTTP/1 parser and are answered 400.
// Letters outside the registry (a TLS record header here) are still closed with
// no answer. std is the control for the 400: net/http answers it too.
func TestRegisteredFirstLetterNeedsHTTP(t *testing.T) {
	for _, e := range crlfEngines {
		t.Run(e.name, func(t *testing.T) {
			addr := startCRLF(t, e)
			ssh, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ssh.Close() }()
			_, _ = io.WriteString(ssh, "SSH-2.0-test\r\n\r\n")
			if got := readUntilClose(ssh, 5*time.Second); !strings.HasPrefix(got, "HTTP/1.1 400") {
				t.Errorf("SSH banner: got %q, want an HTTP/1.1 400 answer", got)
			}
			if e.eng == celeris.Std {
				return // net/http answers a TLS record header with its own 400
			}
			tls, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tls.Close() }()
			_, _ = tls.Write([]byte("\x16\x03\x01\x02\x00\x01\x00\x01\xfc"))
			if got := readUntilClose(tls, 5*time.Second); got != "" {
				t.Errorf("TLS record header: got %q, want the conn closed with no answer", got)
			}
		})
	}
}
