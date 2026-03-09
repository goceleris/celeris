//go:build linux

package integration

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/adaptive"
	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/epoll"

	"golang.org/x/net/http2"
)

func TestAdaptiveAutoProtocol(t *testing.T) {
	port := freePort(t)
	cfg := defaultTestConfig(port, engine.Auto)
	cfg.Engine = engine.Adaptive
	cfg.Resources.Workers = 2

	e, err := adaptive.New(cfg, &echoHandler{})
	if err != nil {
		t.Skipf("adaptive engine not available: %v", err)
	}

	startEngine(t, e)

	addr := e.Addr().String()

	// H1 requests.
	h1Client := &http.Client{Timeout: 3 * time.Second}
	for range 5 {
		resp, err := h1Client.Get("http://" + addr + "/h1test")
		if err != nil {
			t.Fatalf("H1 request failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("H1: got status %d, body: %s", resp.StatusCode, body)
		}
	}

	// H2C requests.
	h2c := h2cClient(addr)
	for range 5 {
		resp, err := h2c.Get("http://" + addr + "/h2test")
		if err != nil {
			t.Fatalf("H2C request failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("H2C: got status %d, body: %s", resp.StatusCode, body)
		}
	}

	// Mixed parallel.
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 5 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			resp, err := h1Client.Get("http://" + addr + "/mixed-h1")
			if err != nil {
				errs <- fmt.Errorf("H1 parallel: %w", err)
				return
			}
			_ = resp.Body.Close()
		}()
		go func() {
			defer wg.Done()
			resp, err := h2c.Get("http://" + addr + "/mixed-h2")
			if err != nil {
				errs <- fmt.Errorf("H2C parallel: %w", err)
				return
			}
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestAdaptiveAutoSingleWorker(t *testing.T) {
	port := freePort(t)
	cfg := defaultTestConfig(port, engine.Auto)
	cfg.Engine = engine.Adaptive
	cfg.Resources.Workers = 0 // will default based on NumCPU
	cfg.Resources.SQERingSize = 1024

	e, err := adaptive.New(cfg, &echoHandler{})
	if err != nil {
		t.Skipf("adaptive engine not available: %v", err)
	}

	startEngine(t, e)

	addr := e.Addr().String()

	// H1 request.
	h1Client := &http.Client{Timeout: 3 * time.Second}
	resp, err := h1Client.Get("http://" + addr + "/single-h1")
	if err != nil {
		t.Fatalf("H1 request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("H1: got status %d", resp.StatusCode)
	}

	// H2C request.
	h2c := h2cClient(addr)
	resp, err = h2c.Get("http://" + addr + "/single-h2")
	if err != nil {
		t.Fatalf("H2C request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("H2C: got status %d", resp.StatusCode)
	}
}

func TestAdaptiveSwitchUnderLoad(t *testing.T) {
	port := freePort(t)
	cfg := defaultTestConfig(port, engine.HTTP1)
	cfg.Engine = engine.Adaptive
	cfg.Resources.Workers = 2

	e, err := adaptive.New(cfg, &echoHandler{})
	if err != nil {
		t.Skipf("adaptive engine not available: %v", err)
	}

	startEngine(t, e)

	addr := e.Addr().String()

	// Open persistent connections.
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 10,
			DisableKeepAlives:   false,
		},
	}

	// Send traffic to establish connections.
	for range 10 {
		resp, err := client.Get("http://" + addr + "/pre-switch")
		if err != nil {
			t.Fatalf("pre-switch request failed: %v", err)
		}
		_ = resp.Body.Close()
	}

	initialType := e.ActiveEngine().Type()

	// Close old connections before switching — they're on the active engine's
	// sockets and will never drain otherwise (keep-alive).
	client.CloseIdleConnections()
	time.Sleep(100 * time.Millisecond)

	// Force switch.
	e.ForceSwitch()

	newType := e.ActiveEngine().Type()
	if newType == initialType {
		t.Error("expected engine type to change after ForceSwitch")
	}

	// Use a fresh transport — old connections were on the now-standby engine.
	// Our H1 response writer sends Connection: close, so every request opens
	// a new TCP connection. Retry all requests to handle the brief transition
	// window where listen sockets are being transferred between engines.
	postClient := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}

	successCount := 0
	deadline := time.Now().Add(10 * time.Second)
	for successCount < 5 && time.Now().Before(deadline) {
		resp, err := postClient.Get("http://" + addr + "/post-switch")
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("post-switch: got status %d", resp.StatusCode)
		}
		successCount++
	}
	if successCount < 5 {
		t.Fatalf("only %d/5 post-switch requests succeeded within deadline", successCount)
	}
}

func TestAdaptiveResourceCleanup(t *testing.T) {
	port := freePort(t)
	cfg := defaultTestConfig(port, engine.HTTP1)
	cfg.Engine = engine.Adaptive
	cfg.Resources.Workers = 2

	ctx, cancel := context.WithCancel(t.Context())
	e, err := adaptive.New(cfg, &echoHandler{})
	if err != nil {
		t.Skipf("adaptive engine not available: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()

	// Wait for engine to be ready.
	deadline := time.Now().Add(5 * time.Second)
	for e.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		cancel()
		t.Fatal("engine did not start")
	}

	// Readiness probe.
	addr := e.Addr().String()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, probeErr := client.Get("http://" + addr + "/probe")
	if probeErr != nil {
		cancel()
		<-errCh
		t.Skipf("engine not functional: %v", probeErr)
	}
	_ = resp.Body.Close()

	// Send some traffic.
	for range 5 {
		resp, err := client.Get("http://" + addr + "/cleanup-test")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		_ = resp.Body.Close()
	}

	// Close transport to release connections.
	client.CloseIdleConnections()

	// Allow connections to drain.
	time.Sleep(100 * time.Millisecond)

	// Shutdown.
	cancel()
	<-errCh

	// Verify no negative active connections (which would indicate double-decrement).
	m := e.Metrics()
	if m.ActiveConnections < 0 {
		t.Errorf("negative active connections after shutdown: %d", m.ActiveConnections)
	}
}

func TestAdaptiveConstrainedRing(t *testing.T) {
	port := freePort(t)
	cfg := defaultTestConfig(port, engine.HTTP1)
	cfg.Engine = engine.Adaptive
	cfg.Resources.Workers = 2
	cfg.Resources.SQERingSize = 1024

	e, err := adaptive.New(cfg, &echoHandler{})
	if err != nil {
		t.Skipf("adaptive engine not available: %v", err)
	}

	startEngine(t, e)

	addr := e.Addr().String()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/constrained")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("got status %d", resp.StatusCode)
	}
}

func TestEpollPauseResume(t *testing.T) {
	port := freePort(t)
	cfg := defaultTestConfig(port, engine.HTTP1)

	e, err := epoll.New(cfg, &echoHandler{})
	if err != nil {
		t.Fatalf("epoll engine: %v", err)
	}

	startEngine(t, e)
	addr := e.Addr().String()

	// Verify it works initially.
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/before-pause")
	if err != nil {
		t.Fatalf("before pause: %v", err)
	}
	_ = resp.Body.Close()
	t.Log("before pause: OK")

	// Pause.
	_ = e.PauseAccept()
	client.CloseIdleConnections()
	t.Log("paused, waiting for loops to suspend...")
	time.Sleep(2 * time.Second)

	// Resume.
	_ = e.ResumeAccept()
	t.Log("resumed, waiting for listen sockets...")
	time.Sleep(1 * time.Second)

	// Try again with fresh transport.
	postClient := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{}}
	resp, err = postClient.Get("http://" + addr + "/after-resume")
	if err != nil {
		t.Fatalf("after resume: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("after resume: got status %d", resp.StatusCode)
	}
	t.Log("after resume: OK")
}

func h2cClient(addr string) *http.Client {
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, _ string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
}
