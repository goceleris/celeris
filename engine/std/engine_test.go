package std

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

type echoHandler struct{}

func (h *echoHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	body := s.GetData()
	var method, path string
	for _, hdr := range s.GetHeaders() {
		switch hdr[0] {
		case ":method":
			method = hdr[1]
		case ":path":
			path = hdr[1]
		}
	}

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

func TestNewEngine(t *testing.T) {
	cfg := resource.Config{
		Addr:     ":8090",
		Engine:   engine.Std,
		Protocol: engine.Auto,
	}
	e, err := New(cfg, &echoHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.Type() != engine.Std {
		t.Errorf("Type: got %v, want Std", e.Type())
	}
}

func TestListenAndServe(t *testing.T) {
	cfg := resource.Config{
		Addr:     ":18091",
		Engine:   engine.Std,
		Protocol: engine.HTTP1,
	}
	e, err := New(cfg, &echoHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := t.Context()
	listenCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- e.Listen(listenCtx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for e.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("listener not ready")
	}

	addr := e.Addr().String()

	resp, err := http.Get("http://" + addr + "/hello")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "GET /hello" {
		t.Errorf("body: got %q, want %q", body, "GET /hello")
	}

	m := e.Metrics()
	if m.RequestCount < 1 {
		t.Errorf("RequestCount: got %d, want >= 1", m.RequestCount)
	}

	cancel()
	if err := <-errCh; err != nil && err != http.ErrServerClosed {
		t.Errorf("Listen error: %v", err)
	}
}

func TestMetricsConnectionTracking(t *testing.T) {
	cfg := resource.Config{
		Addr:     ":18092",
		Engine:   engine.Std,
		Protocol: engine.HTTP1,
	}
	e, err := New(cfg, &echoHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := t.Context()
	listenCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() { _ = e.Listen(listenCtx) }()

	deadline := time.Now().Add(2 * time.Second)
	for e.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("listener not ready")
	}

	resp, err := http.Get("http://" + e.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	m := e.Metrics()
	if m.RequestCount == 0 {
		t.Error("expected non-zero request count")
	}

	cancel()
}
