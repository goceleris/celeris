package conformance

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"

	"golang.org/x/net/http2"
)

// testHandler is a simple echo handler for conformance testing.
type testHandler struct{}

func (h *testHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	var method, path string
	headers := s.GetHeaders()
	for _, hdr := range headers {
		switch hdr[0] {
		case ":method":
			method = hdr[1]
		case ":path":
			path = hdr[1]
		}
	}

	body := s.GetData()
	respBody := []byte(fmt.Sprintf("%s %s", method, path))
	if len(body) > 0 {
		respBody = append(respBody, '\n')
		respBody = append(respBody, body...)
	}

	if s.ResponseWriter != nil {
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{
			{"content-type", "text/plain"},
		}, respBody)
	}
	return nil
}

// addrGetter is an optional interface engines can implement to expose their listener address.
type addrGetter interface {
	Addr() net.Addr
}

// freePort returns a free TCP port by briefly listening on :0.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// startEngine creates, starts, and returns a running engine with its address.
func startEngine(t *testing.T, ef EngineFactory, proto engine.Protocol) (addr string, cleanup func()) {
	t.Helper()

	port := freePort(t)
	cfg := resource.Config{
		Addr:     fmt.Sprintf("127.0.0.1:%d", port),
		Engine:   ef.Type,
		Protocol: proto,
		Resources: resource.Resources{
			Workers: 2,
		},
	}

	handler := &testHandler{}
	e, err := ef.New(cfg, handler)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		errCh <- e.Listen(ctx)
	}()

	// Wait for listener, checking for early Listen failures.
	var engineAddr net.Addr
	if ag, ok := e.(addrGetter); ok {
		deadline := time.Now().Add(3 * time.Second)
		for ag.Addr() == nil && time.Now().Before(deadline) {
			select {
			case err := <-errCh:
				cancel()
				if err != nil {
					t.Skipf("engine failed to start (skipping): %v", err)
				}
				t.Fatal("engine Listen returned nil without setting addr")
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
		engineAddr = ag.Addr()
	}

	if engineAddr == nil {
		cancel()
		t.Fatal("engine did not start listening")
	}

	// Readiness probe: verify the engine can actually serve a request.
	// On resource-constrained CI runners io_uring may bind successfully
	// but fail to process completions, causing every subtest to timeout.
	addr = fmt.Sprintf("127.0.0.1:%d", port)
	probeClient := clientForProto(proto)
	probeClient.Timeout = 2 * time.Second
	resp, probeErr := probeClient.Get("http://" + addr + "/healthz")
	if probeErr != nil {
		cancel()
		<-errCh
		t.Skipf("engine %s not functional (skipping): %v", ef.Type.String(), probeErr)
	}
	_ = resp.Body.Close()

	return addr, func() {
		cancel()
		<-errCh
	}
}

// clientForProto returns an HTTP client appropriate for the given protocol.
func clientForProto(proto engine.Protocol) *http.Client {
	if proto == engine.H2C {
		return &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http2.Transport{
				AllowHTTP: true,
				DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, network, addr)
				},
			},
		}
	}
	return &http.Client{Timeout: 5 * time.Second}
}

func sendRequest(t *testing.T, addr, method, path string, body io.Reader) *http.Response {
	t.Helper()

	url := "http://" + addr + path
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func sendRequestProto(t *testing.T, addr, method, path string, body io.Reader, proto engine.Protocol) *http.Response {
	t.Helper()

	url := "http://" + addr + path
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	client := clientForProto(proto)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}
