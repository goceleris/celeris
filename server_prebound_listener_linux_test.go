package celeris

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestStartWithListener_DefaultEngine is celeris#614 at the documented entry
// point: Server.StartWithListener on the DEFAULT engine, which on Linux is
// adaptive. Every adaptive cell of the nightly died here — the adaptive
// engine ignored the supplied listener, invented its own port, and the
// sub-engine constructor rejected the mismatch ("ambiguous configuration:
// Addr=... but Listener is bound to ..."), so the server never started.
//
// The config is built exactly the way the validation reference apps build it:
// Addr carries the "<host>:0" from -bind and the caller binds that itself
// before handing celeris the listener.
func TestStartWithListener_DefaultEngine(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", Workers: 2}) // no Engine: the default is adaptive on Linux
	s.GET("/ping", func(c *Context) error { return c.String(http.StatusOK, "ok") })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-bind listener: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- s.StartWithListenerAndContext(ctx, ln) }()

	// A bounded client matters here: when the engine fails to start, the
	// caller's listener is left open and unowned, so the kernel still
	// completes the TCP handshake from its backlog and an unbounded GET would
	// block forever instead of letting the loop report the start error.
	client := &http.Client{Timeout: 2 * time.Second}

	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case err := <-startErr:
			t.Fatalf("server failed to start on a pre-bound listener: %v", err)
		default:
		}
		resp, err := client.Get("http://" + addr + "/ping")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || string(body) != "ok" {
				t.Fatalf("GET /ping = %d %q, want 200 \"ok\"", resp.StatusCode, body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never served on the pre-bound address %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// It must be serving on the listener's OWN address, not one it picked.
	if info := s.EngineInfo(); info == nil || info.Type != Adaptive {
		t.Fatalf("engine = %v, want adaptive (the Linux default)", info)
	}

	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	_ = s.Shutdown(sctx)
	select {
	case <-startErr:
	case <-time.After(15 * time.Second):
		t.Fatal("StartWithListenerAndContext did not return after shutdown")
	}
}
