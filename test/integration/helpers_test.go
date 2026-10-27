//go:build linux

package integration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
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
	stop, err := tryStartEngine(t, e)
	if err != nil {
		t.Skipf("engine failed to start: %v", err)
	}
	t.Cleanup(stop)
}

// startEngineRetry builds an engine with mk and starts it, retrying both the
// build and the Listen for the window when they fail with the ring-setup ENOMEM
// (an io_uring engine's New reads a probe ring that hit the same ENOMEM as "not
// available", its Listen the ring setup itself; celeris#1104). Each try builds a
// fresh engine. A build error that is not a ring ENOMEM, or still is after the
// window, goes to onNewErr (which skips or fails the test, as the caller's
// build error did); a Listen error is a skip, as in startEngine.
func startEngineRetry(t *testing.T, mk func() (engine.Engine, error), onNewErr func(error)) engine.Engine {
	t.Helper()
	var (
		e      engine.Engine
		stop   func()
		newErr error
	)
	tries, err := retryRingENOMEM(func() error {
		var err error
		if e, err = mk(); err != nil {
			newErr = err
			return err
		}
		newErr = nil
		stop, err = tryStartEngine(t, e)
		return err
	}, ringENOMEMWindow)
	if tries > 1 {
		t.Logf("engine start retried on ring ENOMEM: %d tries", tries)
	}
	switch {
	case newErr != nil:
		onNewErr(newErr)
		t.FailNow() // onNewErr skips or fails; never continue without an engine
	case err != nil:
		t.Skipf("engine failed to start: %v", err)
	}
	t.Cleanup(stop)
	return e
}

// tryStartEngine runs e.Listen, waits for it to bind and probes /healthz. It
// returns the engine's stop function, or the error Listen returned, which the
// caller decides on (startEngine skips on it, startEngineRetry retries a ring
// ENOMEM). The other outcomes skip or fail the test as startEngine always did.
func tryStartEngine(t *testing.T, e engine.Engine) (stop func(), listenErr error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		errCh <- e.Listen(ctx)
	}()

	if ag, ok := e.(addrGetter); ok {
		deadline := time.Now().Add(25 * time.Second)
		for ag.Addr() == nil && time.Now().Before(deadline) {
			select {
			case err := <-errCh:
				cancel()
				if err != nil {
					return nil, err
				}
				t.Fatal("engine Listen returned nil without addr")
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
		if ag.Addr() == nil {
			cancel()
			// Wait briefly for an error from Listen before giving up.
			select {
			case err := <-errCh:
				if err != nil {
					return nil, err
				}
			case <-time.After(5 * time.Second):
			}
			t.Fatal("engine did not start listening")
		}

		// Readiness probe: verify the engine can serve a request.
		// Use a non-pooling transport so the probe connection is closed
		// immediately, preventing reuse by test clients (which would see
		// RST on slow CI machines if the server recycles the connection).
		addr := ag.Addr().String()
		probeTransport := &http.Transport{DisableKeepAlives: true}
		client := &http.Client{Timeout: 2 * time.Second, Transport: probeTransport}
		resp, probeErr := client.Get("http://" + addr + "/healthz")
		if probeErr != nil {
			cancel()
			<-errCh
			t.Skipf("engine not functional (skipping): %v", probeErr)
		}
		_ = resp.Body.Close()
	}

	return func() {
		cancel()
		<-errCh
	}, nil
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
