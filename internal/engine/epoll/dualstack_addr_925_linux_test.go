//go:build linux

package epoll

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/std"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#925: an IPv4 client of a dual-stack "[::]:port" listener reaches the
// engine as an AF_INET6 peer with an IPv4-mapped address. The handler must see
// it as the IPv4 address with no brackets, exactly as std reports it, and the
// listener's own address must read "[::]:port" with no zone "0".

// remoteProbe925 records what the handler and OnConnect see for each request.
type remoteProbe925 struct {
	mu      sync.Mutex
	handler []string
	connect []string
}

type remoteHandler925 struct{ p *remoteProbe925 }

func (h remoteHandler925) HandleStream(_ context.Context, s *stream.Stream) error {
	h.p.mu.Lock()
	h.p.handler = append(h.p.handler, s.RemoteAddr)
	h.p.mu.Unlock()
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}

// listenAddrer925 is what both the native engine and std expose here.
type listenAddrer925 interface {
	Listen(context.Context) error
	Addr() net.Addr
}

// runDualStackRemote925 serves one request from 127.0.0.1 to a listener on
// "[::]:port", with serve as the engine under test, and returns the client's
// own address, the address the handler saw, OnConnect's address and the
// listener's address.
func runDualStackRemote925(t *testing.T, serve func(resource.Config, stream.Handler) (listenAddrer925, error)) (client, handler, connect, listen string) {
	t.Helper()
	ln, err := net.Listen("tcp", "[::]:0")
	if err != nil {
		requireDualStack925(t, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	p := &remoteProbe925{}
	e, err := serve(resource.Config{
		Addr:      fmt.Sprintf("[::]:%d", port),
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
		Logger:    slog.New(slog.DiscardHandler),
		OnConnect: func(addr string) {
			p.mu.Lock()
			p.connect = append(p.connect, addr)
			p.mu.Unlock()
		},
	}, remoteHandler925{p: p})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("engine did not stop within 10s")
		}
	})

	var addr net.Addr
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); {
		if addr = e.Addr(); addr != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if addr == nil {
		t.Fatal("engine never bound")
	}
	listen = addr.String()
	if got := addr.(*net.TCPAddr).Port; got != port {
		t.Fatalf("Addr port = %d, want %d", got, port)
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	client = conn.LocalAddr().String()
	if _, err := fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("body: %v", err)
	}
	_ = resp.Body.Close()

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.handler) != 1 || len(p.connect) < 1 {
		t.Fatalf("handler saw %d requests and OnConnect %d peers; want 1 and at least 1", len(p.handler), len(p.connect))
	}
	t.Logf("925 probe: client=%q handler=%q onconnect=%q listen=%q", client, p.handler[0], p.connect[0], listen)
	return client, p.handler[0], p.connect[0], listen
}

// TestDualStackRemoteAddrMatchesStd925 is the native engine's arm: the handler
// and OnConnect must report the IPv4 client exactly as the client's own
// address, which is what std reports, and the listener must not carry a zone.
func TestDualStackRemoteAddrMatchesStd925(t *testing.T) {
	client, handler, connect, listen := runDualStackRemote925(t, func(cfg resource.Config, h stream.Handler) (listenAddrer925, error) {
		return New(cfg, h)
	})
	if handler != client {
		t.Errorf("handler RemoteAddr = %q, want %q (the client's address)", handler, client)
	}
	if connect != client {
		t.Errorf("OnConnect addr = %q, want %q (the client's address)", connect, client)
	}
	if want := "[::]:" + portOf925(t, listen); listen != want {
		t.Errorf("Addr() = %q, want %q", listen, want)
	}
}

// TestDualStackRemoteAddrStdControl925 is the control: std reports the same
// client as the client's own address and its listener as "[::]:port". It
// passes on main, which is what the native arm must match.
func TestDualStackRemoteAddrStdControl925(t *testing.T) {
	client, handler, connect, listen := runDualStackRemote925(t, func(cfg resource.Config, h stream.Handler) (listenAddrer925, error) {
		return std.New(cfg, h)
	})
	if handler != client || connect != client {
		t.Fatalf("std handler=%q OnConnect=%q, want client %q", handler, connect, client)
	}
	if want := "[::]:" + portOf925(t, listen); listen != want {
		t.Errorf("std Addr() = %q, want %q", listen, want)
	}
}

// portOf925 returns the port of a "host:port" string.
func portOf925(t *testing.T, hostport string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(hostport)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", hostport, err)
	}
	return port
}

// requireDualStack925 ends a test whose IPv6 listener could not be bound. It
// skips on a host without IPv6, unless CELERIS_REQUIRE_DUALSTACK=1, where it
// fails, as the dual-stack arms of the websocket test do (celeris#721): a
// skip there would be an unlisted SKIP line in CI.
func requireDualStack925(t *testing.T, err error) {
	t.Helper()
	msg := "no IPv6 listener on this host: " + err.Error()
	if os.Getenv("CELERIS_REQUIRE_DUALSTACK") == "1" {
		t.Fatal(msg + " -- CELERIS_REQUIRE_DUALSTACK=1 forbids skipping")
	}
	t.Skip(msg)
}
