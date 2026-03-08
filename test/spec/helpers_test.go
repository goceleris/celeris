// Package spec provides protocol compliance tests using h2spec and raw-TCP h1spec.
package spec

import (
	"bufio"
	"context"
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
	cfg := resource.Config{
		Addr:     fmt.Sprintf("127.0.0.1:%d", port),
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
	t.Cleanup(func() {
		cancel()
		<-errCh
	})
	return fmt.Sprintf("127.0.0.1:%d", port)
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
