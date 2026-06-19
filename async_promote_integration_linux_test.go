//go:build linux

package celeris

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestAdaptiveImmediatePromote_Epoll verifies improvement #3 end-to-end on the
// real epoll engine (which runs HandleStream's inline timing): under
// AsyncHandlers=true, an UNMARKED route whose handler blocks for longer than
// adaptiveBlockingThreshold is promoted to async dispatch on the FIRST request,
// not after adaptivePromoteStreak (8) of them. A fast route used only for
// readiness must NOT promote.
func TestAdaptiveImmediatePromote_Epoll(t *testing.T) {
	s := New(Config{Engine: Epoll, AsyncHandlers: true})
	s.GET("/ping", func(c *Context) error { return c.String(http.StatusOK, "ok") })
	s.GET("/slow", func(c *Context) error {
		time.Sleep(3 * time.Millisecond) // > adaptiveBlockingThreshold (2ms)
		return c.String(http.StatusOK, "slow")
	})
	if !s.router.adaptiveRoutes["/slow"] || !s.router.adaptiveRoutes["/ping"] {
		t.Fatal("both routes must be adaptive under AsyncHandlers=true")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = s.StartWithListener(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	base := "http://" + ln.Addr().String()
	client := &http.Client{}
	// Readiness on the FAST route only (so /slow is untouched until our 1 probe).
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		if resp, err := client.Get(base + "/ping"); err == nil {
			_ = resp.Body.Close()
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("server did not become ready")
	}

	if s.router.isPromoted("/slow") {
		t.Fatal("/slow must not be promoted before any request to it")
	}
	// Exactly ONE request to the blocking route.
	resp, err := client.Get(base + "/slow")
	if err != nil {
		t.Fatalf("GET /slow: %v", err)
	}
	_ = resp.Body.Close()

	if !s.router.isPromoted("/slow") {
		t.Fatal("#3: a single >2ms inline run must promote /slow immediately (got not-promoted)")
	}
	// The fast route hammered for readiness must never promote.
	if s.router.isPromoted("/ping") {
		t.Fatal("/ping (fast) must not be promoted")
	}
}
