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

// TestProcessor_CanRunInlineGateOrdering pins the gate ordering inside
// canRunInline: (1) END_STREAM check, (2) continuationActive check,
// (3) per-route async check. Specifically a stream WITH end_stream that
// the processor would otherwise consider for inline must still return
// false while CONTINUATION is active, regardless of the per-route async
// decision. Prevents a refactor that flips the order of the gates.
func TestProcessor_CanRunInlineGateOrdering(t *testing.T) {
	h := &asyncRouteTestHandler{asyncPaths: map[string]bool{"/db": true}}
	p := NewProcessor(h, newTestFrameWriter(), newTestResponseWriter())

	// Helper: a stream that meets the EndStream precondition.
	mkStream := func(path string) *Stream {
		return &Stream{
			EndStream: true,
			Headers:   [][2]string{{":method", "GET"}, {":path", path}},
		}
	}

	// (a) sync route, no CONTINUATION → inline-eligible.
	if !p.canRunInline(mkStream("/cpu")) {
		t.Fatal("sync route, no continuation: want canRunInline=true")
	}
	// (b) async route, no CONTINUATION → NOT inline (per-route gate).
	if p.canRunInline(mkStream("/db")) {
		t.Fatal("async route: want canRunInline=false")
	}
	// (c) sync route, CONTINUATION active → NOT inline (continuation gate
	// must fire before the per-route gate; otherwise async routes that
	// arrive during a CONTINUATION storm could be falsely promoted).
	p.continuationActive.Store(true)
	if p.canRunInline(mkStream("/cpu")) {
		t.Fatal("continuation active: want canRunInline=false for sync route")
	}
	// (d) async route, CONTINUATION active → NOT inline (either gate
	// is sufficient; both firing is the safe overlap case).
	if p.canRunInline(mkStream("/db")) {
		t.Fatal("continuation active + async route: want canRunInline=false")
	}
	p.continuationActive.Store(false)

	// (e) confirm no EndStream returns false regardless of route.
	noEnd := &Stream{EndStream: false, Headers: [][2]string{{":method", "POST"}, {":path", "/cpu"}}}
	if p.canRunInline(noEnd) {
		t.Fatal("no EndStream: want canRunInline=false")
	}
}

// TestProcessor_StreamRouteAsyncStripsQuery pins the path-query strip in
// streamRouteAsync — /db?id=5 must resolve as /db so a route registered
// without a query string is matched correctly.
func TestProcessor_StreamRouteAsyncStripsQuery(t *testing.T) {
	h := &asyncRouteTestHandler{asyncPaths: map[string]bool{"/db": true}}
	p := NewProcessor(h, newTestFrameWriter(), newTestResponseWriter())

	withQuery := &Stream{Headers: [][2]string{{":method", "GET"}, {":path", "/db?id=5"}}}
	if !p.streamRouteAsync(withQuery) {
		t.Fatal("/db?id=5 must strip to /db and resolve async")
	}
	without := &Stream{Headers: [][2]string{{":method", "GET"}, {":path", "/cpu?x"}}}
	if p.streamRouteAsync(without) {
		t.Fatal("/cpu?x must strip to /cpu and resolve sync")
	}
}

// TestProcessor_StreamRouteAsyncMissingPseudoHeaders verifies that a
// missing :path or :method makes streamRouteAsync return false (=
// inline-eligible) so the inline path still runs and the malformed
// request fails normally inside the handler chain.
func TestProcessor_StreamRouteAsyncMissingPseudoHeaders(t *testing.T) {
	h := &asyncRouteTestHandler{asyncPaths: map[string]bool{"/db": true}}
	p := NewProcessor(h, newTestFrameWriter(), newTestResponseWriter())

	noPath := &Stream{Headers: [][2]string{{":method", "GET"}}}
	if p.streamRouteAsync(noPath) {
		t.Fatal("missing :path: want streamRouteAsync=false")
	}
	noMethod := &Stream{Headers: [][2]string{{":path", "/db"}}}
	if p.streamRouteAsync(noMethod) {
		t.Fatal("missing :method: want streamRouteAsync=false")
	}
}
