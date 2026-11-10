//go:build linux

package integration

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/adaptive"
	"github.com/goceleris/celeris/internal/engine"
)

// TestAdaptiveSwitchKeepsKeepAlive884 is the guard of the shutdown flag
// (celeris#884 / #885: every HTTP/1 response after Shutdown carries
// Connection: close). An engine switch is not a shutdown: the old engine is
// paused, not shut down, and the flag must stay down on both engines, so a
// keep-alive client of either keeps its conn open and sees no Connection
// header, before and after the switch, on a conn opened before it and on one
// opened after.
func TestAdaptiveSwitchKeepsKeepAlive884(t *testing.T) {
	port := freePort(t)
	cfg := defaultTestConfig(port, engine.HTTP1)
	cfg.Engine = engine.Adaptive
	cfg.Resources.Workers = 2
	logs := &syncBuf886{}
	cfg.Logger = slog.New(slog.NewJSONHandler(logs, nil))

	e, err := adaptive.New(cfg, &echoHandler{}, nil)
	if err != nil {
		t.Fatalf("adaptive.New: %v", err) // fails on config validation or a port hold, not on a host without io_uring (skip_routing_890_test.go)
	}
	startEngine(t, e)
	addr := e.Addr().String()

	type kaConn struct {
		c  net.Conn
		br *bufio.Reader
	}
	dial := func() kaConn {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return kaConn{c, bufio.NewReader(c)}
	}
	get := func(k kaConn, when string) {
		t.Helper()
		_ = k.c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(k.c, "GET /ka HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			t.Fatalf("%s: write: %v", when, err)
		}
		resp, err := http.ReadResponse(k.br, nil)
		if err != nil {
			t.Fatalf("%s: read: %v", when, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.Close || len(resp.Header.Values("Connection")) != 0 {
			t.Fatalf("%s: Connection %q, close=%v: a response with no shutdown must stay keep-alive", when, resp.Header.Values("Connection"), resp.Close)
		}
	}

	before := dial()
	get(before, "before the switch")
	initial := e.ActiveEngine().Type()
	e.ForceSwitch()
	if e.ActiveEngine().Type() == initial {
		skipOrFailIfStandbyAborted886(t, e.Metrics().AdaptiveSwitches, logs)
		t.Fatal("expected engine type to change after ForceSwitch")
	}
	get(before, "a conn opened before the switch, after it")
	get(before, "the same conn, a second request")
	after := dial()
	get(after, "a conn opened after the switch")
	get(after, "the conn opened after the switch, a second request")
}
