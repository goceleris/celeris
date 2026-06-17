//go:build linux

package iouring

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// revertResolverHandler is an AsyncRouteResolver whose async classification is
// flipped at runtime, simulating celeris#356 route promotion (true) and the
// celeris#364 TTL de-promotion (false) without waiting on the real router TTL.
type revertResolverHandler struct{ async atomic.Bool }

func (h *revertResolverHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}
func (h *revertResolverHandler) RouteAsync(_, _ string) bool { return h.async.Load() }
func (h *revertResolverHandler) HasAsyncRoutes() bool        { return true }

func startRevertEngine(t *testing.T, h stream.Handler) *Engine {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	e, err := New(resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		Resources:     resource.Resources{Workers: 4},
		AsyncHandlers: true,
	}, h)
	if err != nil {
		t.Skipf("iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
		}
	})
	dl := time.Now().Add(30 * time.Second)
	for time.Now().Before(dl) && e.Addr() == nil {
		select {
		case err := <-errCh:
			if err != nil && (strings.Contains(err.Error(), "cannot allocate memory") ||
				strings.Contains(err.Error(), "io_uring_setup") || strings.Contains(err.Error(), "tier")) {
				t.Skipf("io_uring unavailable on this runner: %v", err)
			}
			t.Fatalf("engine.Listen returned early: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind in time")
	}
	return e
}

// sendKeepAlive issues one keep-alive GET on an existing conn and reads the
// full 2-byte "ok" response, leaving the conn open for the next request.
func sendKeepAlive(c net.Conn, br *bufio.Reader, timeout time.Duration) error {
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write([]byte("GET /x HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read status: %w", err)
	}
	if len(statusLine) < 12 || statusLine[9:12] != "200" {
		return fmt.Errorf("bad status: %q", statusLine)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read header: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	body := make([]byte, 2)
	if _, err := io.ReadFull(br, body); err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if string(body) != "ok" {
		return fmt.Errorf("bad body: %q", body)
	}
	return nil
}

// TestAsyncConnRevertsOnRouteDepromotion is the celeris#364 conn-level revert
// regression: a keep-alive conn promoted to async dispatch must return to the
// inline fast path once its route de-promotes (RouteAsync flips false), proven
// by its ability to RE-promote afterwards (a still-async conn cannot re-promote
// — it never re-runs the inline ErrAsyncDispatch gate).
func TestAsyncConnRevertsOnRouteDepromotion(t *testing.T) {
	h := &revertResolverHandler{}
	h.async.Store(true)
	e := startRevertEngine(t, h)
	target := e.Addr().String()

	c, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	send := func(phase string) {
		if err := sendKeepAlive(c, br, 2*time.Second); err != nil {
			t.Fatalf("%s request failed: %v", phase, err)
		}
	}

	// Phase A — RouteAsync=true: the conn promotes to async dispatch.
	for i := 0; i < 20; i++ {
		send("promote")
	}
	p1 := e.Metrics().AsyncPromotedConns
	if p1 == 0 {
		t.Fatal("conn never promoted under RouteAsync=true")
	}

	// Phase B — RouteAsync=false: the dispatch goroutine must revert the conn
	// to inline at its next idle point. Requests must keep succeeding.
	h.async.Store(false)
	for i := 0; i < 30; i++ {
		send("revert")
		time.Sleep(time.Millisecond)
	}

	// Phase C — RouteAsync=true again: a reverted (now inline) conn re-promotes;
	// a conn still stuck async would NOT, so AsyncPromotedConns must increase.
	h.async.Store(true)
	for i := 0; i < 30; i++ {
		send("repromote")
	}
	p2 := e.Metrics().AsyncPromotedConns
	if p2 <= p1 {
		t.Fatalf("conn did not revert+re-promote (still stuck async): promoted before=%d after=%d", p1, p2)
	}
	t.Logf("promotions: after-A=%d after-C=%d (revert confirmed by re-promotion)", p1, p2)
}

// TestAsyncConnRevertRace hammers the promote/feed/revert/re-promote interaction
// across many keep-alive conns with RouteAsync flipping continuously. Run under
// -race it validates that the worker recv path and the dispatch goroutine never
// race on cs.asyncPromoted / asyncInBuf during a revert. Gated on -short.
func TestAsyncConnRevertRace(t *testing.T) {
	if testing.Short() {
		t.Skip("revert race test needs sustained toggling load; -short skips it")
	}
	h := &revertResolverHandler{}
	h.async.Store(true)
	e := startRevertEngine(t, h)
	target := e.Addr().String()

	const (
		concurrency = 64
		duration    = 10 * time.Second
	)
	stop := make(chan struct{})
	// Toggler: flip the route's async classification fast enough that conns are
	// constantly promoting and reverting.
	go func() {
		tk := time.NewTicker(3 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				h.async.Store(!h.async.Load())
			}
		}
	}()

	deadline := time.Now().Add(duration)
	var ok, failed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", target, 2*time.Second)
			if err != nil {
				failed.Add(1)
				return
			}
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			for time.Now().Before(deadline) {
				if err := sendKeepAlive(c, br, 2*time.Second); err != nil {
					failed.Add(1)
					return // conn broken; a corrupted response would surface here
				}
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	close(stop)

	if ok.Load() < 1000 {
		t.Fatalf("too few successful requests (ok=%d failed=%d) — server may have stalled", ok.Load(), failed.Load())
	}
	// Liveness epilogue.
	h.async.Store(false)
	c, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		t.Fatalf("engine not serving after revert churn: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := sendKeepAlive(c, bufio.NewReader(c), 2*time.Second); err != nil {
		t.Fatalf("engine not serving after revert churn: %v", err)
	}
	t.Logf("ok=%d failed=%d over %s", ok.Load(), failed.Load(), duration)
}
