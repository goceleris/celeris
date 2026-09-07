package conn

import (
	"context"
	"errors"
	"testing"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

// TestProcessH1DetachedNonWebSocketDoesNotParse is the celeris#497 guard.
//
// ProcessH1 used to short-circuit a detached connection only when
// WSDataDelivery was set (WebSocket). On a detached connection WITHOUT that
// sink -- Server-Sent Events, or any handler that took the connection over --
// further client bytes were parsed as a NEW request on the same
// per-connection cached stream and Context. That reuses a Context the live
// handler still owns: responses interleave with the SSE body, a second hit on
// the same route overwrites OnError/OnDetachClose and loses the first
// stream's cancel, and recoverAndRelease starts a second detachDone waiter so
// the Context is released twice.
//
// A second request on a single-response stream is a contract violation, so
// the connection must be closed rather than parsed.
func TestProcessH1DetachedNonWebSocketDoesNotParse(t *testing.T) {
	// Per-subtest counter: a shared one makes the second subtest fail for the
	// first subtest's reason.
	newHandler := func(calls *int) stream.Handler {
		return stream.HandlerFunc(func(_ context.Context, _ *stream.Stream) error {
			*calls++
			return nil
		})
	}

	t.Run("SSE-shaped detach closes instead of parsing", func(t *testing.T) {
		handlerCalls := 0
		state := NewH1State()
		state.Detached.Store(true)
		state.WSDataDelivery = nil // detached, but not a WebSocket

		req := []byte("GET /second HTTP/1.1\r\nHost: x\r\n\r\n")
		err := ProcessH1(context.Background(), req, state, newHandler(&handlerCalls), func([]byte) {})

		if !errors.Is(err, errConnectionClose) {
			t.Fatalf("a second request on a detached non-WS conn must close it, got err=%v", err)
		}
		if handlerCalls != 0 {
			t.Errorf("the bytes must not be parsed as a request: handler ran %d time(s)", handlerCalls)
		}
	})

	t.Run("WebSocket detach still delivers raw bytes", func(t *testing.T) {
		handlerCalls := 0
		state := NewH1State()
		state.Detached.Store(true)
		var delivered []byte
		state.WSDataDelivery = func(b []byte) { delivered = append(delivered, b...) }

		payload := []byte{0x81, 0x03, 'a', 'b', 'c'}
		if err := ProcessH1(context.Background(), payload, state, newHandler(&handlerCalls), func([]byte) {}); err != nil {
			t.Fatalf("WebSocket delivery must not error: %v", err)
		}
		if string(delivered) != string(payload) {
			t.Errorf("WebSocket bytes must reach the sink verbatim, got %q", delivered)
		}
		if handlerCalls != 0 {
			t.Errorf("WebSocket bytes must not be parsed: handler ran %d time(s)", handlerCalls)
		}
	})
}
