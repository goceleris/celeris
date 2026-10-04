package sse

import (
	"errors"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/internal/testhooks"
)

// TestHeadAnswersWithHeadersOnly421: since celeris#421 a HEAD request to an
// SSE GET route reaches the middleware. It sends the stream's headers and
// ends the response without running Handler. OnConnect still runs: an
// accepted HEAD runs OnDisconnect once, as a stream that ends at once would,
// and a rejection is returned as for GET (TestOnConnectReject). What a
// rejection puts on the wire is celeris#835, for GET and HEAD alike. The
// engines drop a HEAD body; TestSSEHeadGetsHeadersOnly421 checks them.
func TestHeadAnswersWithHeadersOnly421(t *testing.T) {
	newHead := func(t *testing.T) (*celeris.Context, *mockStreamer) {
		ctx, _ := celeristest.NewContextT(t, "HEAD", "/events")
		ms := &mockStreamer{}
		testhooks.Stream(ctx).ResponseWriter = ms
		return ctx, ms
	}

	t.Run("accepted", func(t *testing.T) {
		ctx, ms := newHead(t)
		var connects, disconnects int
		h := New(Config{
			HeartbeatInterval: -1,
			RetryInterval:     3000,
			Handler:           func(*Client) { t.Error("Handler ran for HEAD") },
			OnConnect:         func(*celeris.Context, *Client) error { connects++; return nil },
			OnDisconnect:      func(*celeris.Context, *Client) { disconnects++ },
		})
		if err := h(ctx); err != nil {
			t.Fatalf("HEAD: %v", err)
		}
		ms.mu.Lock()
		defer ms.mu.Unlock()
		ct := ""
		for _, kv := range ms.headers {
			if kv[0] == "content-type" {
				ct = kv[1]
			}
		}
		if ms.status != 200 || ct != "text/event-stream" || !ms.closed || len(ms.chunks) != 0 {
			t.Fatalf("HEAD: status %d content-type %q closed %v, %d chunks; want 200 text/event-stream, closed, no chunk (not even the retry line)",
				ms.status, ct, ms.closed, len(ms.chunks))
		}
		if connects != 1 || disconnects != 1 {
			t.Fatalf("OnConnect ran %d times, OnDisconnect %d; want 1 and 1", connects, disconnects)
		}
	})

	t.Run("rejected", func(t *testing.T) {
		ctx, ms := newHead(t)
		var disconnects int
		h := New(Config{
			HeartbeatInterval: -1,
			Handler:           func(*Client) { t.Error("Handler ran for HEAD") },
			OnConnect: func(*celeris.Context, *Client) error {
				return celeris.NewHTTPError(403, "forbidden")
			},
			OnDisconnect: func(*celeris.Context, *Client) { disconnects++ },
		})
		err := h(ctx)
		var he *celeris.HTTPError
		if !errors.As(err, &he) || he.Code != 403 {
			t.Fatalf("HEAD with OnConnect rejecting: err %v, want the 403 HTTPError", err)
		}
		ms.mu.Lock()
		defer ms.mu.Unlock()
		if ms.status != 0 || ms.closed || disconnects != 0 {
			t.Fatalf("rejected HEAD: status %d closed %v OnDisconnect %d; want no header written, not closed, 0", ms.status, ms.closed, disconnects)
		}
	})
}
