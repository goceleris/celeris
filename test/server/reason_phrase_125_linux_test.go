//go:build linux

package celeris_test

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#889 item 125: the native HTTP/1 engines wrote the reason phrase
// "Unknown" for 405, 206, 408, 412, 416 and 429. Over a real connection, on
// every engine (std answers through net/http, epoll, io_uring and adaptive
// through the native writers), the status line of a handler's answer, of the
// router's 405 and of the router's 404 carries http.StatusText.
func TestReasonPhraseEveryEngine125(t *testing.T) {
	var codes []int
	for c := 200; c <= 599; c++ {
		if http.StatusText(c) != "" {
			codes = append(codes, c)
		}
	}
	statusLine := func(t *testing.T, addr, req string) string {
		t.Helper()
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Write([]byte(req)); err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			t.Fatalf("reading the status line of %q: %v", req, err)
		}
		return line
	}
	for _, e := range engines761 {
		for _, route := range []string{"sync", "async-route"} {
			t.Run(e.name+"/"+route, func(t *testing.T) {
				addr := startServer421(t, e.eng, func(s *celeris.Server) {
					routes := []*celeris.Route{
						s.GET("/s/:code", func(c *celeris.Context) error {
							code, err := strconv.Atoi(c.Param("code"))
							if err != nil {
								return err
							}
							return c.Blob(code, "text/plain", []byte("x"))
						}),
						s.GET("/only-get", func(c *celeris.Context) error { return c.String(200, "ok") }),
					}
					if route == "async-route" {
						for _, r := range routes {
							r.Async()
						}
					}
				})
				for _, c := range codes {
					want := fmt.Sprintf("HTTP/1.1 %d %s\r\n", c, http.StatusText(c))
					req := fmt.Sprintf("GET /s/%d HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", c)
					if got := statusLine(t, addr, req); got != want {
						t.Errorf("handler answering %d: status line %q, want %q", c, got, want)
					}
				}
				if got, want := statusLine(t, addr, "DELETE /only-get HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"), "HTTP/1.1 405 Method Not Allowed\r\n"; got != want {
					t.Errorf("router 405: status line %q, want %q", got, want)
				}
				if got, want := statusLine(t, addr, "GET /nope HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"), "HTTP/1.1 404 Not Found\r\n"; got != want {
					t.Errorf("router 404: status line %q, want %q", got, want)
				}
			})
		}
	}
}
