//go:build linux

package integration

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/epoll"
)

// TestHTTP1PipeliningAsync asserts that the async-dispatch path on the
// epoll engine preserves HTTP/1.1 pipelining ordering. We open one TCP
// conn, send 10 pipelined GETs back-to-back without waiting for any
// response, and assert every response comes back in request order.
//
// Runs against both AsyncHandlers=false (inline dispatch) and
// AsyncHandlers=true (goroutine-per-conn dispatch) to catch ordering
// regressions introduced by W3.
func TestHTTP1PipeliningAsync(t *testing.T) {
	for _, tc := range []struct {
		name  string
		async bool
	}{
		{"inline", false},
		{"async", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := freePort(t)
			cfg := defaultTestConfig(port, engine.HTTP1)
			cfg.AsyncHandlers = tc.async
			eng, err := epoll.New(cfg, &echoHandler{})
			if err != nil {
				t.Skipf("epoll engine unavailable: %v", err)
			}
			startEngine(t, eng)

			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

			const n = 10
			var buf strings.Builder
			for i := 0; i < n; i++ {
				fmt.Fprintf(&buf, "GET /r%d HTTP/1.1\r\nHost: x\r\n\r\n", i)
			}
			if _, werr := conn.Write([]byte(buf.String())); werr != nil {
				t.Fatalf("write: %v", werr)
			}

			br := bufio.NewReader(conn)
			for i := 0; i < n; i++ {
				resp, rerr := http.ReadResponse(br, nil)
				if rerr != nil {
					t.Fatalf("response %d: %v", i, rerr)
				}
				if resp.StatusCode != 200 {
					t.Fatalf("response %d: status %d", i, resp.StatusCode)
				}
				body := make([]byte, 32)
				bn, _ := resp.Body.Read(body)
				_ = resp.Body.Close()
				got := string(body[:bn])
				want := fmt.Sprintf("GET /r%d", i)
				if got != want {
					t.Fatalf("response %d: got %q, want %q", i, got, want)
				}
			}

			// Ensure the conn survived pipelining (keep-alive intact).
			if _, werr := conn.Write([]byte("GET /final HTTP/1.1\r\nHost: x\r\n\r\n")); werr != nil {
				t.Fatalf("write final: %v", werr)
			}
			resp, rerr := http.ReadResponse(br, nil)
			if rerr != nil {
				t.Fatalf("final response: %v", rerr)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("final: status %d", resp.StatusCode)
			}
			_ = resp.Body.Close()
		})
	}
}
