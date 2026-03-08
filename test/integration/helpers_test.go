//go:build linux

package integration

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// echoHandler responds to every request with "METHOD PATH".
type echoHandler struct{}

func (h *echoHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	var method, path string
	for _, hdr := range s.GetHeaders() {
		switch hdr[0] {
		case ":method":
			method = hdr[1]
		case ":path":
			path = hdr[1]
		}
	}
	resp := []byte(fmt.Sprintf("%s %s", method, path))
	if data := s.GetData(); len(data) > 0 {
		resp = append(resp, '\n')
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

func startEngine(t *testing.T, e engine.Engine) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		errCh <- e.Listen(ctx)
	}()

	if ag, ok := e.(addrGetter); ok {
		deadline := time.Now().Add(5 * time.Second)
		for ag.Addr() == nil && time.Now().Before(deadline) {
			select {
			case err := <-errCh:
				cancel()
				if err != nil {
					t.Skipf("engine failed to start: %v", err)
				}
				t.Fatal("engine Listen returned nil without addr")
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
}

func defaultTestConfig(port int, proto engine.Protocol) resource.Config {
	return resource.Config{
		Addr:     fmt.Sprintf("127.0.0.1:%d", port),
		Protocol: proto,
		Resources: resource.Resources{
			Workers: 2,
		},
	}
}
