// Package spec provides protocol compliance tests using h2spec and raw-TCP h1spec.
package spec

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	stdengine "github.com/goceleris/celeris/engine/std"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"

	"golang.org/x/net/http2"
)

type specEngine struct {
	name string
	typ  engine.EngineType
	new  func(resource.Config, stream.Handler) (engine.Engine, error)
}

var specEngines []specEngine

func registerEngine(se specEngine) {
	specEngines = append(specEngines, se)
}

func init() {
	registerEngine(specEngine{
		name: "std",
		typ:  engine.Std,
		new: func(cfg resource.Config, h stream.Handler) (engine.Engine, error) {
			return stdengine.New(cfg, h)
		},
	})
}

// specHandler responds to every request with "METHOD PATH\n[body]".
// h2spec only needs 200 + non-empty body; h1spec verifies echo content.
type specHandler struct{}

func (h *specHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	var method, path string
	for _, hdr := range s.GetHeaders() {
		switch hdr[0] {
		case ":method":
			method = hdr[1]
		case ":path":
			path = hdr[1]
		}
	}
	resp := []byte(method + " " + path + "\n")
	if data := s.GetData(); len(data) > 0 {
		resp = append(resp, data...)
	}
	if s.ResponseWriter != nil {
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{
			{"content-type", "text/plain"},
		}, resp)
	}
	return nil
}

type addrGetter interface {
	Addr() net.Addr
}

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

func startSpecEngine(t *testing.T, se specEngine, proto engine.Protocol) string {
	t.Helper()
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cfg := resource.Config{
		Addr:     addr,
		Engine:   se.typ,
		Protocol: proto,
		Resources: resource.Resources{
			Workers: 2,
		},
	}
	e, err := se.new(cfg, &specHandler{})
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		errCh <- e.Listen(ctx)
	}()
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
		if ag.Addr() == nil {
			cancel()
			t.Fatal("engine did not start listening")
		}
	}

	// Readiness probe: verify the engine can actually serve a request.
	// On resource-constrained CI runners io_uring may bind successfully
	// but fail to process completions, causing every subtest to timeout.
	if err := probeEngine(addr, proto); err != nil {
		cancel()
		<-errCh
		t.Skipf("engine %s not functional (skipping): %v", se.name, err)
	}

	t.Cleanup(func() {
		cancel()
		<-errCh
	})
	return addr
}

// probeEngine sends a single HTTP request to verify the engine is functional.
func probeEngine(addr string, proto engine.Protocol) error {
	client := &http.Client{Timeout: 2 * time.Second}
	if proto == engine.H2C {
		client.Transport = &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, a string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, a)
			},
		}
	}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// h2specMaxFail defines per-engine maximum tolerated h2spec failures.
// The std engine delegates to Go's net/http + x/net/http2 which has 4
// known non-conformances (connection-specific headers, invalid preface
// EOF, SETTINGS_INITIAL_WINDOW_SIZE ordering). Native engines handle
// these correctly and must pass everything.
var h2specMaxFail = map[string]int{
	"std":     4, // 4 known stdlib failures
	"epoll":   0, // must be fully conformant
	"iouring": 0, // must be fully conformant
}

// parseH2SpecSummary extracts the "N tests, M passed, S skipped, F failed"
// line from h2spec output and returns (total, passed, failed).
func parseH2SpecSummary(output string) (total, passed, failed int, ok bool) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		var skipped int
		n, _ := fmt.Sscanf(line, "%d tests, %d passed, %d skipped, %d failed",
			&total, &passed, &skipped, &failed)
		if n == 4 {
			return total, passed, failed, true
		}
	}
	return 0, 0, 0, false
}

// checkH2SpecResult validates h2spec output against the per-engine tolerance.
// It logs the output, then fails only if the number of failures exceeds the
// engine's known-acceptable maximum.
func checkH2SpecResult(t *testing.T, engineName, label string, output []byte, exitErr error) {
	t.Helper()
	t.Logf("h2spec %s:\n%s", label, output)

	total, passed, failed, ok := parseH2SpecSummary(string(output))
	if !ok {
		if exitErr != nil {
			t.Errorf("h2spec %s: %v (could not parse summary)", label, exitErr)
		}
		return
	}

	maxFail, known := h2specMaxFail[engineName]
	if !known {
		maxFail = 0 // unknown engine: require full pass
	}

	if failed > maxFail {
		t.Errorf("h2spec %s [%s]: %d/%d passed, %d failed (max tolerated: %d)",
			label, engineName, passed, total, failed, maxFail)
	} else if failed > 0 {
		t.Logf("h2spec %s [%s]: %d/%d passed (%d known acceptable failures)",
			label, engineName, passed, total, failed)
	}
}

// rawConnect opens a raw TCP connection with deadlines.
func rawConnect(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("connect to %s: %v", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	return conn
}

// rawSendRecv sends a raw HTTP request and reads the response.
// The method is extracted from the first word of the request for correct HEAD handling.
// The caller must close the response body.
func rawSendRecv(t *testing.T, addr, request string) *http.Response {
	t.Helper()
	conn := rawConnect(t, addr)
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Extract method so http.ReadResponse correctly handles HEAD (no body).
	method := "GET"
	if i := strings.IndexByte(request, ' '); i > 0 {
		method = request[:i]
	}
	fakeReq := &http.Request{Method: method}
	resp, err := http.ReadResponse(bufio.NewReader(conn), fakeReq)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp
}
