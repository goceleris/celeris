package spec

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
)

// TestH1Spec runs raw-TCP HTTP/1.1 compliance tests (RFC 9112) against every available engine.
func TestH1Spec(t *testing.T) {
	for _, se := range specEngines {
		t.Run(se.name, func(t *testing.T) {
			t.Parallel()
			addr := startSpecEngine(t, se, engine.HTTP1)
			runH1Spec(t, addr)
		})
	}
}

func runH1Spec(t *testing.T, addr string) {
	t.Run("RFC9112", func(t *testing.T) {
		t.Run("3_RequestLine", func(t *testing.T) {
			testRequestLine(t, addr)
		})
		t.Run("5_HeaderFields", func(t *testing.T) {
			testHeaderFields(t, addr)
		})
		t.Run("6_MessageBody", func(t *testing.T) {
			testMessageBody(t, addr)
		})
		t.Run("7_HostValidation", func(t *testing.T) {
			testHostValidation(t, addr)
		})
		t.Run("9_ConnectionManagement", func(t *testing.T) {
			testConnectionManagement(t, addr)
		})
	})
}

// --- §3 Request Line ---

func testRequestLine(t *testing.T, addr string) {
	t.Run("ValidGET", func(t *testing.T) {
		resp := rawSendRecv(t, addr, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		assertStatus(t, resp, 200)
		if !strings.Contains(string(body), "GET /") {
			t.Errorf("body %q does not echo 'GET /'", body)
		}
	})

	t.Run("ValidPOST", func(t *testing.T) {
		resp := rawSendRecv(t, addr,
			"POST /submit HTTP/1.1\r\nHost: localhost\r\nContent-Length: 5\r\n\r\nhello")
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		assertStatus(t, resp, 200)
		if !strings.Contains(string(body), "hello") {
			t.Errorf("body %q does not echo payload", body)
		}
	})

	methods := []string{"GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS", "PATCH"}
	for _, method := range methods {
		t.Run("Method_"+method, func(t *testing.T) {
			var req string
			switch method {
			case "POST", "PUT", "PATCH":
				req = fmt.Sprintf("%s / HTTP/1.1\r\nHost: localhost\r\nContent-Length: 4\r\n\r\ntest", method)
			default:
				req = fmt.Sprintf("%s / HTTP/1.1\r\nHost: localhost\r\n\r\n", method)
			}
			resp := rawSendRecv(t, addr, req)
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.ReadAll(resp.Body)
			assertStatus(t, resp, 200)
		})
	}
}

// --- §5 Header Fields ---

func testHeaderFields(t *testing.T, addr string) {
	t.Run("CaseInsensitiveNames", func(t *testing.T) {
		resp := rawSendRecv(t, addr,
			"GET / HTTP/1.1\r\nHost: localhost\r\nX-Custom-Header: value\r\n\r\n")
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.ReadAll(resp.Body)
		assertStatus(t, resp, 200)
	})

	t.Run("OWSAroundValue", func(t *testing.T) {
		resp := rawSendRecv(t, addr,
			"GET / HTTP/1.1\r\nHost:   localhost  \r\n\r\n")
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.ReadAll(resp.Body)
		assertStatus(t, resp, 200)
	})

	t.Run("EmptyHeaderValue", func(t *testing.T) {
		resp := rawSendRecv(t, addr,
			"GET / HTTP/1.1\r\nHost: localhost\r\nX-Empty:\r\n\r\n")
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.ReadAll(resp.Body)
		assertStatus(t, resp, 200)
	})
}

// --- §6 Message Body ---

func testMessageBody(t *testing.T, addr string) {
	t.Run("ContentLength", func(t *testing.T) {
		payload := "Hello, World!"
		req := fmt.Sprintf("POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: %d\r\n\r\n%s",
			len(payload), payload)
		resp := rawSendRecv(t, addr, req)
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		assertStatus(t, resp, 200)
		if !strings.Contains(string(body), payload) {
			t.Errorf("body %q does not contain payload %q", body, payload)
		}
	})

	t.Run("ChunkedEncoding", func(t *testing.T) {
		req := "POST / HTTP/1.1\r\nHost: localhost\r\nTransfer-Encoding: chunked\r\n\r\n" +
			"5\r\nhello\r\n" +
			"1\r\n \r\n" +
			"5\r\nworld\r\n" +
			"0\r\n\r\n"
		resp := rawSendRecv(t, addr, req)
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		assertStatus(t, resp, 200)
		if !strings.Contains(string(body), "hello world") {
			t.Errorf("chunked body %q does not contain 'hello world'", body)
		}
	})

	t.Run("LargeBody_1MB", func(t *testing.T) {
		payload := strings.Repeat("x", 1024*1024)
		req := fmt.Sprintf("POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: %d\r\n\r\n%s",
			len(payload), payload)

		conn := rawConnect(t, addr)
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

		if _, err := conn.Write([]byte(req)); err != nil {
			t.Fatalf("write: %v", err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.ReadAll(resp.Body)
		assertStatus(t, resp, 200)
	})
}

// --- §7 Host Validation ---

func testHostValidation(t *testing.T, addr string) {
	t.Run("MissingHostHeader", func(t *testing.T) {
		// HTTP/1.1 requires Host header; server MUST respond 400.
		resp := rawSendRecv(t, addr, "GET / HTTP/1.1\r\n\r\n")
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.ReadAll(resp.Body)
		if resp.StatusCode != 400 {
			t.Errorf("missing Host: status %d, want 400", resp.StatusCode)
		}
	})
}

// --- §9 Connection Management ---

func testConnectionManagement(t *testing.T, addr string) {
	t.Run("KeepAlive", func(t *testing.T) {
		conn := rawConnect(t, addr)
		defer func() { _ = conn.Close() }()
		reader := bufio.NewReader(conn)

		for i := range 5 {
			req := fmt.Sprintf("GET /%d HTTP/1.1\r\nHost: localhost\r\n\r\n", i)
			if _, err := conn.Write([]byte(req)); err != nil {
				t.Fatalf("request %d: write: %v", i, err)
			}
			resp, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatalf("request %d: read: %v", i, err)
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			assertStatus(t, resp, 200)
		}
	})

	t.Run("ConnectionClose", func(t *testing.T) {
		conn := rawConnect(t, addr)
		defer func() { _ = conn.Close() }()

		if _, err := conn.Write([]byte(
			"GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		assertStatus(t, resp, 200)

		// Server must close the connection after responding.
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 1)
		if _, err := conn.Read(buf); err == nil {
			t.Error("expected connection closed after Connection: close")
		}
	})

	t.Run("Pipelining", func(t *testing.T) {
		conn := rawConnect(t, addr)
		defer func() { _ = conn.Close() }()

		pipelined := "GET /first HTTP/1.1\r\nHost: localhost\r\n\r\n" +
			"GET /second HTTP/1.1\r\nHost: localhost\r\n\r\n"
		if _, err := conn.Write([]byte(pipelined)); err != nil {
			t.Fatalf("write: %v", err)
		}

		reader := bufio.NewReader(conn)

		resp1, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("first response: %v", err)
		}
		body1, _ := io.ReadAll(resp1.Body)
		_ = resp1.Body.Close()

		resp2, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("second response: %v", err)
		}
		body2, _ := io.ReadAll(resp2.Body)
		_ = resp2.Body.Close()

		assertStatus(t, resp1, 200)
		assertStatus(t, resp2, 200)

		if !strings.Contains(string(body1), "/first") {
			t.Errorf("first response body: %q, want /first", body1)
		}
		if !strings.Contains(string(body2), "/second") {
			t.Errorf("second response body: %q, want /second", body2)
		}
	})
}

func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Errorf("status: got %d, want %d", resp.StatusCode, want)
	}
}
