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
