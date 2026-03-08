package conformance

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/goceleris/celeris/engine"
)

func testBasicMethods(t *testing.T, ef EngineFactory, proto engine.Protocol) {
	addr, cleanup := startEngine(t, ef, proto)
	defer cleanup()

	methods := []string{"GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS"}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			var body io.Reader
			if method == "POST" || method == "PUT" {
				body = bytes.NewReader([]byte("hello"))
			}

			resp := sendRequestProto(t, addr, method, "/test", body, proto)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != 200 {
				t.Errorf("%s: status %d", method, resp.StatusCode)
			}
		})
	}
}

func testLargeBody(t *testing.T, ef EngineFactory, proto engine.Protocol) {
	addr, cleanup := startEngine(t, ef, proto)
	defer cleanup()

	largeBody := strings.Repeat("x", 1024*1024)
	resp := sendRequestProto(t, addr, "POST", "/upload", bytes.NewReader([]byte(largeBody)), proto)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Errorf("large body: status %d", resp.StatusCode)
	}
}

func testKeepAlive(t *testing.T, ef EngineFactory, proto engine.Protocol) {
	addr, cleanup := startEngine(t, ef, proto)
	defer cleanup()

	client := clientForProto(proto)
	for range 5 {
		resp, err := client.Get("http://" + addr + "/keepalive")
		if err != nil {
			t.Fatalf("keepalive: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("keepalive: status %d", resp.StatusCode)
		}
	}
}

func testErrorHandling(t *testing.T, ef EngineFactory, proto engine.Protocol) {
	addr, cleanup := startEngine(t, ef, proto)
	defer cleanup()

	resp := sendRequestProto(t, addr, "GET", "/", nil, proto)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Errorf("error handling: status %d", resp.StatusCode)
	}
}
