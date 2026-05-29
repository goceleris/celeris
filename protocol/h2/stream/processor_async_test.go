package stream

import (
	"context"
	"testing"
	"time"
)

// asyncRouteTestHandler implements both Handler and AsyncRouteResolver so
// the processor's per-route inline-vs-pool decision can be exercised.
type asyncRouteTestHandler struct {
	asyncPaths map[string]bool
	ran        chan string
}

func (h *asyncRouteTestHandler) HandleStream(_ context.Context, s *Stream) error {
	var path string
	for _, hh := range s.Headers {
		if hh[0] == ":path" {
			path = hh[1]
			break
		}
	}
	select {
	case h.ran <- path:
	case <-time.After(time.Second):
	}
	return nil
}

func (h *asyncRouteTestHandler) RouteAsync(_, path string) bool { return h.asyncPaths[path] }
func (h *asyncRouteTestHandler) HasAsyncRoutes() bool           { return len(h.asyncPaths) > 0 }

// TestProcessor_PerRouteAsyncDispatch verifies a sync route runs inline on
// the event loop (InlineCount increments) while an async route is dispatched
// to the worker pool (InlineCount unchanged), with both producing the right
// handler invocation.
func TestProcessor_PerRouteAsyncDispatch(t *testing.T) {
	h := &asyncRouteTestHandler{
		asyncPaths: map[string]bool{"/db": true},
		ran:        make(chan string, 4),
	}
	fw := newTestFrameWriter()
	rw := newTestResponseWriter()
	p := NewProcessor(h, fw, rw)
	if !p.perRouteAsync {
		t.Fatal("processor should detect per-route async (HasAsyncRoutes=true)")
	}
	ctx := context.Background()

	// Sync route → inline.
	syncHeaders := encodeHeaders(t, [][2]string{
		{":method", "GET"}, {":scheme", "https"}, {":path", "/cpu"}, {":authority", "x"},
	})
	if err := p.ProcessFrame(ctx, makeHeadersFrame(t, 1, true, true, syncHeaders)); err != nil {
		t.Fatalf("sync HEADERS: %v", err)
	}
	if got := waitPath(t, h.ran); got != "/cpu" {
		t.Fatalf("sync handler ran for %q, want /cpu", got)
	}
	if p.InlineCount != 1 {
		t.Fatalf("sync route should run inline: InlineCount=%d, want 1", p.InlineCount)
	}

	// Async route → pool (InlineCount must not advance).
	asyncHeaders := encodeHeaders(t, [][2]string{
		{":method", "GET"}, {":scheme", "https"}, {":path", "/db"}, {":authority", "x"},
	})
	if err := p.ProcessFrame(ctx, makeHeadersFrame(t, 3, true, true, asyncHeaders)); err != nil {
		t.Fatalf("async HEADERS: %v", err)
	}
	if got := waitPath(t, h.ran); got != "/db" {
		t.Fatalf("async handler ran for %q, want /db", got)
	}
	if p.InlineCount != 1 {
		t.Fatalf("async route must NOT run inline: InlineCount=%d, want still 1", p.InlineCount)
	}
}

// TestProcessor_NoAsyncRoutesSkipsResolution verifies a server with zero
// async routes keeps the prior characteristic-based inline behavior (the
// fast-out: perRouteAsync=false).
func TestProcessor_NoAsyncRoutesSkipsResolution(t *testing.T) {
	h := &asyncRouteTestHandler{asyncPaths: map[string]bool{}, ran: make(chan string, 2)}
	fw := newTestFrameWriter()
	rw := newTestResponseWriter()
	p := NewProcessor(h, fw, rw)
	if p.perRouteAsync {
		t.Fatal("processor should NOT enable per-route async when HasAsyncRoutes=false")
	}
	hdrs := encodeHeaders(t, [][2]string{
		{":method", "GET"}, {":scheme", "https"}, {":path", "/anything"}, {":authority", "x"},
	})
	if err := p.ProcessFrame(context.Background(), makeHeadersFrame(t, 1, true, true, hdrs)); err != nil {
		t.Fatalf("HEADERS: %v", err)
	}
	if got := waitPath(t, h.ran); got != "/anything" {
		t.Fatalf("handler ran for %q, want /anything", got)
	}
	if p.InlineCount != 1 {
		t.Fatalf("GET with END_STREAM should run inline: InlineCount=%d, want 1", p.InlineCount)
	}
}

func waitPath(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handler to run")
		return ""
	}
}
