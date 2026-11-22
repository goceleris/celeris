package std

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#889: net/http runs the StateNew ConnState hook inline in
// Server.Serve, before it starts the connection's goroutine. An OnConnect
// that calls runtime.Goexit (t.FailNow in a test's callback) used to end the
// Serve goroutine, whose deferred listener close then stopped the engine
// accepting for good, with nothing logged. safeCallback's recover cannot stop
// a Goexit, so OnConnect runs on a helper goroutine that the hook joins.

type syncBuf889 struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf889) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf889) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestOnConnectGoexitKeepsAccepting889(t *testing.T) {
	var logs syncBuf889
	var mu sync.Mutex
	calls := 0
	e, err := New(resource.Config{
		Addr:     "127.0.0.1:0",
		Engine:   engine.Std,
		Protocol: engine.HTTP1,
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
		OnConnect: func(string) {
			mu.Lock()
			calls++
			mu.Unlock()
			runtime.Goexit()
		},
	}, &echoHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(10 * time.Second):
			t.Error("Listen did not return")
		}
	})
	for deadline := time.Now().Add(5 * time.Second); e.Addr() == nil; {
		if time.Now().After(deadline) {
			t.Fatal("listener not ready")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Each dial uses a fresh connection, so each one fires OnConnect (and its
	// Goexit); every one must be answered, not only the first.
	tr := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(tr.CloseIdleConnections)
	cl := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	for i := range 3 {
		resp, err := cl.Get("http://" + e.Addr().String() + "/x")
		if err != nil {
			t.Fatalf("request %d after an OnConnect Goexit: %v (the engine stopped accepting)", i+1, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "GET /x" {
			t.Fatalf("request %d: %d %q", i+1, resp.StatusCode, body)
		}
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 3 {
		t.Errorf("OnConnect ran %d times, want 3", n)
	}
	if !bytes.Contains([]byte(logs.String()), []byte("Goexit")) {
		t.Errorf("an OnConnect Goexit was not logged; log:\n%s", logs.String())
	}
}
