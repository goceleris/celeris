package conn

import (
	"context"
	"errors"
	"testing"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

// asyncDispatchHandler records the paths it actually handled, so a test can
// assert the handler did NOT run when ProcessH1 bails for async dispatch.
type asyncDispatchHandler struct{ handled []string }

func (h *asyncDispatchHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	h.handled = append(h.handled, s.Path)
	return nil
}

// TestProcessH1_InlineSyncRouteRunsInline verifies that in InlineMode a sync
// route runs the handler inline (no ErrAsyncDispatch).
func TestProcessH1_InlineSyncRouteRunsInline(t *testing.T) {
	state := NewH1State()
	state.InlineMode = true
	state.RouteAsync = func(_, path string) bool { return path == "/db" }
	h := &asyncDispatchHandler{}
	req := []byte("GET /cpu HTTP/1.1\r\nHost: x\r\n\r\n")
	err := ProcessH1(context.Background(), req, state, h, func([]byte) {})
	if err != nil {
		t.Fatalf("sync route: err = %v, want nil", err)
	}
	if len(h.handled) != 1 || h.handled[0] != "/cpu" {
		t.Fatalf("sync route handler should run inline; handled=%v", h.handled)
	}
	if state.buffer.Len() != 0 {
		t.Fatalf("sync route should leave no buffered bytes; got %d", state.buffer.Len())
	}
}

// TestProcessH1_InlineAsyncRouteBails verifies that in InlineMode an async
// route stops before the handler, returns ErrAsyncDispatch, and stashes the
// full request in state.buffer for the dispatch goroutine to re-run.
func TestProcessH1_InlineAsyncRouteBails(t *testing.T) {
	state := NewH1State()
	state.InlineMode = true
	state.RouteAsync = func(_, path string) bool { return path == "/db" }
	h := &asyncDispatchHandler{}
	req := "GET /db HTTP/1.1\r\nHost: x\r\n\r\n"
	err := ProcessH1(context.Background(), []byte(req), state, h, func([]byte) {})
	if !errors.Is(err, ErrAsyncDispatch) {
		t.Fatalf("async route: err = %v, want ErrAsyncDispatch", err)
	}
	if len(h.handled) != 0 {
		t.Fatalf("async route handler must NOT run inline; handled=%v", h.handled)
	}
	if got := state.buffer.String(); got != req {
		t.Fatalf("async route should stash the full request; buffer=%q want %q", got, req)
	}

	// Re-run as the dispatch goroutine would (InlineMode=false): the
	// buffered request must now run the handler to completion.
	state.InlineMode = false
	if err := ProcessH1(context.Background(), nil, state, h, func([]byte) {}); err != nil {
		t.Fatalf("dispatch re-run: err = %v, want nil", err)
	}
	if len(h.handled) != 1 || h.handled[0] != "/db" {
		t.Fatalf("dispatch re-run should handle /db; handled=%v", h.handled)
	}
}

// TestProcessH1_InlinePipelinedSyncThenAsync verifies that with two pipelined
// requests (sync then async) in one buffer, the sync one runs inline and the
// async one (plus its bytes) is stashed for dispatch.
func TestProcessH1_InlinePipelinedSyncThenAsync(t *testing.T) {
	state := NewH1State()
	state.InlineMode = true
	state.RouteAsync = func(_, path string) bool { return path == "/db" }
	h := &asyncDispatchHandler{}
	syncReq := "GET /cpu HTTP/1.1\r\nHost: x\r\n\r\n"
	asyncReq := "GET /db HTTP/1.1\r\nHost: x\r\n\r\n"
	err := ProcessH1(context.Background(), []byte(syncReq+asyncReq), state, h, func([]byte) {})
	if !errors.Is(err, ErrAsyncDispatch) {
		t.Fatalf("err = %v, want ErrAsyncDispatch", err)
	}
	if len(h.handled) != 1 || h.handled[0] != "/cpu" {
		t.Fatalf("first (sync) request should run inline; handled=%v", h.handled)
	}
	if got := state.buffer.String(); got != asyncReq {
		t.Fatalf("async request should be stashed; buffer=%q want %q", got, asyncReq)
	}
}

// TestProcessH1_NoInlineModeNoBail verifies that without InlineMode (the
// dispatch-goroutine path), an async route runs normally — no bail.
func TestProcessH1_NoInlineModeNoBail(t *testing.T) {
	state := NewH1State()
	state.InlineMode = false
	state.RouteAsync = func(_, _ string) bool { return true } // everything async
	h := &asyncDispatchHandler{}
	req := []byte("GET /db HTTP/1.1\r\nHost: x\r\n\r\n")
	if err := ProcessH1(context.Background(), req, state, h, func([]byte) {}); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(h.handled) != 1 {
		t.Fatalf("dispatch path should run handler; handled=%v", h.handled)
	}
}

// TestProcessH1_InlinePipelinedAsyncThenSync verifies the inverse-order
// pipelined case: when the FIRST request in the buffer is async,
// ProcessH1 bails before running any handler and stashes the entire
// buffer (both requests). The dispatch goroutine then runs both in
// order on the re-run.
func TestProcessH1_InlinePipelinedAsyncThenSync(t *testing.T) {
	state := NewH1State()
	state.InlineMode = true
	state.RouteAsync = func(_, path string) bool { return path == "/db" }
	h := &asyncDispatchHandler{}
	asyncReq := "GET /db HTTP/1.1\r\nHost: x\r\n\r\n"
	syncReq := "GET /cpu HTTP/1.1\r\nHost: x\r\n\r\n"
	err := ProcessH1(context.Background(), []byte(asyncReq+syncReq), state, h, func([]byte) {})
	if !errors.Is(err, ErrAsyncDispatch) {
		t.Fatalf("err = %v, want ErrAsyncDispatch", err)
	}
	if len(h.handled) != 0 {
		t.Fatalf("no handler should run inline; handled=%v", h.handled)
	}
	if got := state.buffer.String(); got != asyncReq+syncReq {
		t.Fatalf("buffer must hold both requests; got=%q", got)
	}
	state.InlineMode = false
	if err := ProcessH1(context.Background(), nil, state, h, func([]byte) {}); err != nil {
		t.Fatalf("dispatch re-run: err = %v, want nil", err)
	}
	if len(h.handled) != 2 || h.handled[0] != "/db" || h.handled[1] != "/cpu" {
		t.Fatalf("dispatch re-run should handle both in order; handled=%v", h.handled)
	}
}

// TestProcessH1_InlineBodyBearingAsync verifies that a POST with a
// fully-present body and an async route bails before running the handler
// AND stashes the complete request (headers + body) for the dispatch
// goroutine.
func TestProcessH1_InlineBodyBearingAsync(t *testing.T) {
	state := NewH1State()
	state.InlineMode = true
	state.RouteAsync = func(_, path string) bool { return path == "/upload" }
	h := &asyncDispatchHandler{}
	body := "name=foo&value=42"
	req := "POST /upload HTTP/1.1\r\nHost: x\r\nContent-Length: 17\r\n\r\n" + body
	err := ProcessH1(context.Background(), []byte(req), state, h, func([]byte) {})
	if !errors.Is(err, ErrAsyncDispatch) {
		t.Fatalf("err = %v, want ErrAsyncDispatch", err)
	}
	if len(h.handled) != 0 {
		t.Fatalf("body-bearing async handler must NOT run inline; handled=%v", h.handled)
	}
	if got := state.buffer.String(); got != req {
		t.Fatalf("buffer must hold full request including body; got=%q", got)
	}
	// Dispatch re-run handles the request to completion.
	state.InlineMode = false
	if err := ProcessH1(context.Background(), nil, state, h, func([]byte) {}); err != nil {
		t.Fatalf("dispatch re-run: err = %v, want nil", err)
	}
	if len(h.handled) != 1 || h.handled[0] != "/upload" {
		t.Fatalf("dispatch re-run should handle /upload; handled=%v", h.handled)
	}
}

// TestProcessH1_InlineModeNilResolverNoBail verifies graceful degradation:
// InlineMode=true but RouteAsync=nil (e.g., handler doesn't implement
// AsyncRouteResolver) must not crash and must run handlers inline as the
// non-async-route case.
func TestProcessH1_InlineModeNilResolverNoBail(t *testing.T) {
	state := NewH1State()
	state.InlineMode = true
	state.RouteAsync = nil
	h := &asyncDispatchHandler{}
	req := []byte("GET /anything HTTP/1.1\r\nHost: x\r\n\r\n")
	if err := ProcessH1(context.Background(), req, state, h, func([]byte) {}); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(h.handled) != 1 || h.handled[0] != "/anything" {
		t.Fatalf("nil resolver should fall through to inline handler run; handled=%v", h.handled)
	}
}

// TestProcessH1_InlineHasPendingDataPartialRequest verifies that an inline
// ProcessH1 returning nil with HasPendingData()==true (a partial request
// whose headers arrived but not the CRLFCRLF terminator) leaves the
// caller able to detect the partial-state condition. The engines key
// off this to promote the conn to the dispatch goroutine so the
// continuation parse runs off the inline path.
func TestProcessH1_InlineHasPendingDataPartialRequest(t *testing.T) {
	state := NewH1State()
	state.InlineMode = true
	state.RouteAsync = func(_, _ string) bool { return true }
	h := &asyncDispatchHandler{}
	// Partial headers — no terminating \r\n\r\n.
	partial := []byte("GET /db HTTP/1.1\r\nHost: x\r\n")
	if err := ProcessH1(context.Background(), partial, state, h, func([]byte) {}); err != nil {
		t.Fatalf("partial-headers: err = %v, want nil", err)
	}
	if len(h.handled) != 0 {
		t.Fatalf("partial-headers must not run handler; handled=%v", h.handled)
	}
	if !state.HasPendingData() {
		t.Fatalf("HasPendingData() must report true so engine knows to promote")
	}
}
