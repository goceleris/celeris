//go:build linux

package celeris_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// routes835 registers the celeris#835 cases. Each one takes the StreamWriter
// first, as sse.New does before it runs OnConnect.
func routes835(async bool) func(*celeris.Server) {
	return func(s *celeris.Server) {
		reject := func(c *celeris.Context) error {
			if c.StreamWriter() == nil {
				return errors.New("no StreamWriter")
			}
			return celeris.NewHTTPError(403, "denied835")
		}
		rs := []*celeris.Route{
			s.GET("/reject835", reject),
			s.HEAD("/reject835", reject),
			s.GET("/panic835", func(c *celeris.Context) error {
				_ = c.StreamWriter()
				panic("celeris#835 test panic before the StreamWriter is used")
			}),
			// The control: the stream is committed (and ended) before the
			// error, so the client gets the stream and nothing after it.
			s.GET("/committed835", func(c *celeris.Context) error {
				sw := c.StreamWriter()
				if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}}); err != nil {
					return err
				}
				if _, err := sw.Write([]byte("part835")); err != nil {
					return err
				}
				if err := sw.Close(); err != nil {
					return err
				}
				return errors.New("an error after the stream ended")
			}),
		}
		if async {
			for _, r := range rs {
				r.Async()
			}
		}
	}
}

type case835 struct {
	method, path string
	status       int
	body         string
}

var cases835 = []case835{
	{"GET", "/reject835", 403, "denied835"},
	{"HEAD", "/reject835", 403, ""},
	{"GET", "/panic835", 500, "Internal Server Error"},
	{"GET", "/committed835", 200, "part835"},
}

// TestErrorBeforeStreamWriterUseReachesClient835 reads celeris#835 off the
// wire on every engine, HTTP/1.1 and h2c, sync and async: a handler that has
// taken the StreamWriter and returns an error (or panics) before it sends
// anything must still answer with that error. On main the response was
// dropped: no response at all on the native engines' HTTP/1.1 (the client
// waited for its timeout), an empty 200 on std and on HTTP/2 (the processor's
// fallback for a stream with no HEADERS). The HTTP/1.1 cases run on one
// keep-alive connection, each followed by /ping on it: an error written after a
// stream had started would show up there as the start of the next response.
func TestErrorBeforeStreamWriterUseReachesClient835(t *testing.T) {
	shapes := []struct {
		name  string
		async bool
	}{{"sync", false}, {"async", true}}
	for _, e := range engines761 {
		for _, sh := range shapes {
			t.Run(e.name+"/"+sh.name, func(t *testing.T) {
				addr := startServer761(t, e.eng, sh.async, routes835(sh.async))
				t.Run("h1", func(t *testing.T) {
					conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = conn.Close() }()
					br := bufio.NewReader(conn)
					for _, tc := range cases835 {
						desc := fmt.Sprintf("%s/%s h1 %s %s", e.name, sh.name, tc.method, tc.path)
						status, body, err := h1Do835(conn, br, tc.method, tc.path)
						if err != nil {
							t.Fatalf("%s: %v (want %d %q)", desc, err, tc.status, tc.body)
						}
						if status != tc.status || body != tc.body {
							t.Errorf("%s: %d %q, want %d %q", desc, status, body, tc.status, tc.body)
						}
						status, body, err = h1Do835(conn, br, "GET", "/ping")
						if err != nil || status != 200 || body != "ok" {
							t.Fatalf("%s: then /ping on the same connection: %d %q %v, want 200 \"ok\"", desc, status, body, err)
						}
					}
				})
				t.Run("h2c", func(t *testing.T) {
					p := new(http.Protocols)
					p.SetUnencryptedHTTP2(true)
					cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Protocols: p}}
					defer cl.CloseIdleConnections()
					for _, tc := range cases835 {
						desc := fmt.Sprintf("%s/%s h2c %s %s", e.name, sh.name, tc.method, tc.path)
						req, err := http.NewRequestWithContext(context.Background(), tc.method, "http://"+addr+tc.path, nil)
						if err != nil {
							t.Fatal(err)
						}
						resp, err := cl.Do(req)
						if err != nil {
							t.Errorf("%s: %v (want %d %q)", desc, err, tc.status, tc.body)
							continue
						}
						b, err := io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						if resp.ProtoMajor != 2 {
							t.Errorf("%s: answered over %s, want HTTP/2", desc, resp.Proto)
						}
						if err != nil || resp.StatusCode != tc.status || string(b) != tc.body {
							t.Errorf("%s: %d %q (read error %v), want %d %q", desc, resp.StatusCode, b, err, tc.status, tc.body)
						}
					}
				})
			})
		}
	}
}

// h1Do835 sends one request on conn and reads its response, with a 3 s
// deadline, so a response that never comes fails the case instead of hanging.
func h1Do835(conn net.Conn, br *bufio.Reader, method, path string) (int, string, error) {
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: x\r\n\r\n", method, path); err != nil {
		return 0, "", err
	}
	resp, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return 0, "", fmt.Errorf("no response within 3 s: %w", err)
		}
		return 0, "", err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return resp.StatusCode, string(b), fmt.Errorf("body: %w", err)
	}
	return resp.StatusCode, string(b), nil
}
