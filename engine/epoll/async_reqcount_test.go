//go:build linux

package epoll

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// reqCountHandler answers every request with a fixed two-byte body and
// declares /async as an async-dispatch route. The first request to /async
// therefore makes ProcessH1 bail with ErrAsyncDispatch on the worker's
// inline path, which promotes the connection to its per-conn dispatch
// goroutine (celeris #300). /sync stays on the inline path.
type reqCountHandler struct{}

func (h *reqCountHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}},
		[]byte("ok"))
}

func (h *reqCountHandler) RouteAsync(_, path string) bool { return path == "/async" }
func (h *reqCountHandler) HasAsyncRoutes() bool           { return true }
func (h *reqCountHandler) AsyncRouteCount() int           { return 1 }

// startReqCountEngine boots an epoll engine with AsyncHandlers=true and the
// per-route async resolver wired, and hands back the bound address, the
// engine (for Metrics) and a cleanup func.
func startReqCountEngine(t *testing.T) (string, *Engine, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := resource.Config{
		Addr:     addr,
		Protocol: engine.HTTP1,
		Resources: resource.Resources{
			Workers: 2, // Config.Validate requires Workers >= 2 when set
		},
		AsyncHandlers: true,
	}
	e, err := New(cfg, &reqCountHandler{})
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if a := e.Addr(); a != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		cancel()
		<-errCh
		t.Fatal("engine did not bind in time")
	}
	return e.Addr().String(), e, func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
		}
	}
}

// settleRequestCount waits for the engine's request counter to stop moving
// and returns the settled value. The counter is batched per event-loop
// iteration (l.reqBatch → l.reqCount), so a read taken the instant the
// client sees its response can be one iteration stale. Deliberately NOT
// biased toward an expected value: it waits for quiescence, so it catches
// an over-count just as well as an under-count.
func settleRequestCount(t *testing.T, e *Engine) uint64 {
	t.Helper()
	last := e.Metrics().RequestCount
	stableSince := time.Now()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		v := e.Metrics().RequestCount
		if v != last {
			last = v
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= 300*time.Millisecond {
			return v
		}
	}
	return last
}

// doReq sends one HTTP/1.1 keep-alive request and fully consumes the
// response, so the next request is a fresh recv on the wire rather than a
// pipelined one.
func doReq(t *testing.T, c net.Conn, br *bufio.Reader, path string) {
	t.Helper()
	if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response for %s: %v", path, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body for %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("unexpected response for %s: %d %q", path, resp.StatusCode, body)
	}
}

// TestAsyncPromotedConnRequestCount is the regression test for celeris#626.
//
// Once a connection is async-promoted its later requests are appended to
// cs.asyncInBuf and served by runAsyncHandler on the dispatch goroutine.
// Before the fix, the only reqBatch++ site sat on the worker's inline path
// (loop.go:1197), which a promoted connection never reaches — so
// EngineMetrics.RequestCount stopped advancing on exactly the busiest
// connections, and everything derived from it (the adaptive controller's
// ThroughputRPS and BytesPerReq) went with it.
func TestAsyncPromotedConnRequestCount(t *testing.T) {
	addr, e, cleanup := startReqCountEngine(t)
	defer cleanup()

	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)

	base := settleRequestCount(t, e)
	basePromoted := e.Metrics().AsyncPromotedConns

	const n = 8
	for range n {
		doReq(t, c, br, "/async")
	}

	got := settleRequestCount(t, e) - base
	promoted := e.Metrics().AsyncPromotedConns - basePromoted

	// Negative control: if the connection was never promoted, the test
	// never touched the dispatch path and proves nothing either way.
	if promoted == 0 {
		t.Fatalf("CONTROL FAILED: AsyncPromotedConns delta = 0, so the conn stayed on the inline path and this test did not exercise the defect")
	}
	if got != n {
		t.Fatalf("ASSERT FAILED: RequestCount delta = %d, want %d (%d keep-alive requests sent on an async-promoted conn, AsyncPromotedConns delta = %d)",
			got, n, n, promoted)
	}
	t.Logf("ASSERT OK: RequestCount delta = %d over %d requests (AsyncPromotedConns delta = %d)", got, n, promoted)
}

// TestAsyncPromotionBoundaryCountsRequestOnce guards the double-count hazard
// at the promotion boundary. The request that triggers promotion is parsed
// inline (ProcessH1 returns ErrAsyncDispatch without running the handler),
// stashed, and replayed on the dispatch goroutine. It must be counted
// exactly once: 0 is the celeris#626 under-count, 2 would be a worse bug
// than the one being fixed.
//
// Measures three phases on ONE connection so each path is attributed
// separately:
//
//	phase 1  one /sync request   inline on the worker      want delta 1
//	phase 2  one /async request  the promotion boundary    want delta 1
//	phase 3  two /async requests the dispatch feed path    want delta 2
func TestAsyncPromotionBoundaryCountsRequestOnce(t *testing.T) {
	addr, e, cleanup := startReqCountEngine(t)
	defer cleanup()

	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)

	b0 := settleRequestCount(t, e)
	basePromoted := e.Metrics().AsyncPromotedConns

	doReq(t, c, br, "/sync")
	b1 := settleRequestCount(t, e)
	inlineDelta := b1 - b0

	doReq(t, c, br, "/async")
	b2 := settleRequestCount(t, e)
	promotionDelta := b2 - b1
	promoted := e.Metrics().AsyncPromotedConns - basePromoted

	doReq(t, c, br, "/async")
	doReq(t, c, br, "/async")
	b3 := settleRequestCount(t, e)
	feedDelta := b3 - b2

	t.Logf("phase deltas: inline=%d promotion=%d feed=%d (want 1 / 1 / 2), AsyncPromotedConns delta=%d",
		inlineDelta, promotionDelta, feedDelta, promoted)

	if promoted != 1 {
		t.Fatalf("CONTROL FAILED: AsyncPromotedConns delta = %d, want exactly 1 promotion at the /async request", promoted)
	}
	if inlineDelta != 1 {
		t.Errorf("ASSERT FAILED: inline (/sync) request counted %d times, want 1", inlineDelta)
	}
	if promotionDelta != 1 {
		t.Errorf("ASSERT FAILED: promotion-boundary request counted %d times, want exactly 1 (0 = the celeris#626 under-count, 2 = double count)", promotionDelta)
	}
	if feedDelta != 2 {
		t.Errorf("ASSERT FAILED: 2 requests on the promoted conn counted %d times, want 2", feedDelta)
	}
}
