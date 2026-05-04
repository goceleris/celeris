package sse

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/store"
)

// TestRingBufferAppendThenSince — basic Append/Since round-trip on the
// in-memory store. Exit-criterion: Since returns events strictly after
// lastID in append order.
func TestRingBufferAppendThenSince(t *testing.T) {
	r := NewRingBuffer(8)
	ctx := context.Background()

	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		id, err := r.Append(ctx, Event{Data: strings.Repeat("x", i+1)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	got, err := r.Since(ctx, ids[1])
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("Since(ids[1]) returned %d events, want 3", len(got))
	}
	for i, e := range got {
		if e.ID != ids[i+2] {
			t.Errorf("event[%d].ID = %q, want %q", i, e.ID, ids[i+2])
		}
	}
}

// TestRingBufferRollover — issue #250 exit criterion: dropping the
// oldest event once N+1 is appended. Since("") returns the full
// retained tail; Since on the cursor of a still-retained event returns
// only the events strictly after it.
func TestRingBufferRollover(t *testing.T) {
	const ringSize = 4
	r := NewRingBuffer(ringSize)
	ctx := context.Background()

	for i := 0; i < ringSize+2; i++ {
		if _, err := r.Append(ctx, Event{Data: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := r.Since(ctx, "")
	if err != nil {
		t.Fatalf("Since(\"\"): %v", err)
	}
	if len(got) != ringSize {
		t.Errorf("Since(\"\") returned %d, want %d (cap of retained tail)", len(got), ringSize)
	}
	if got[0].ID != "3" {
		t.Errorf("oldest retained event ID = %q, want \"3\"", got[0].ID)
	}

	got, err = r.Since(ctx, "4")
	if err != nil {
		t.Fatalf("Since(\"4\"): %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Since(\"4\") returned %d events, want 2", len(got))
	}
	if got[0].ID != "5" {
		t.Errorf("Since(\"4\")[0].ID = %q, want \"5\"", got[0].ID)
	}
}

// TestRingBufferUnknownLastID — cursor older than retention returns
// ErrLastIDUnknown so the middleware can fall through to a fresh
// handler invocation.
func TestRingBufferUnknownLastID(t *testing.T) {
	r := NewRingBuffer(2)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, _ = r.Append(ctx, Event{Data: "x"})
	}

	_, err := r.Since(ctx, "1")
	if !errors.Is(err, ErrLastIDUnknown) {
		t.Errorf("Since(out-of-window) err = %v, want ErrLastIDUnknown", err)
	}

	_, err = r.Since(ctx, "not-a-number")
	if !errors.Is(err, ErrLastIDUnknown) {
		t.Errorf("Since(garbage) err = %v, want ErrLastIDUnknown", err)
	}
}

// TestRingBufferAppendAllocs — strict-alloc gate from issue #250: ≤ 1
// alloc/op. The single permitted alloc covers the canonical ID string.
func TestRingBufferAppendAllocs(t *testing.T) {
	if raceEnabled || testing.CoverMode() != "" || testing.Short() {
		t.Skip("alloc counts unstable under -race / coverage / -short")
	}
	r := NewRingBuffer(1024)
	ctx := context.Background()
	e := Event{Data: "x"}
	allocs := testing.AllocsPerRun(200, func() {
		_, _ = r.Append(ctx, e)
	})
	const budget = 1.0
	if allocs > budget {
		t.Fatalf("RingBuffer.Append: %.1f allocs/op (budget %.1f)", allocs, budget)
	}
}

// TestKVReplayStoreRoundTrip — Append+Since round-trip via the
// in-memory KV. Mirrors the production Redis path without needing the
// integration build tag.
func TestKVReplayStoreRoundTrip(t *testing.T) {
	kv := store.NewMemoryKV()
	r, err := NewKVReplayStore(kv, "test:", 0)
	if err != nil {
		t.Fatalf("NewKVReplayStore: %v", err)
	}
	ctx := context.Background()

	ids := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		id, err := r.Append(ctx, Event{Data: "v"})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		ids = append(ids, id)
	}

	got, err := r.Since(ctx, ids[1])
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Since returned %d events, want 2", len(got))
	}
	if len(got) >= 2 && (got[0].ID != ids[2] || got[1].ID != ids[3]) {
		t.Errorf("Since returned ids %v, want %v", []string{got[0].ID, got[1].ID}, ids[2:])
	}
}

// TestNewKVReplayStoreReturnsErrOnNilKV — defensive guard. Constructors
// that need a backing store return an error instead of panicking, so
// the misuse surfaces at the call site rather than as a confusing
// nil-pointer panic on the first Append.
func TestNewKVReplayStoreReturnsErrOnNilKV(t *testing.T) {
	r, err := NewKVReplayStore(nil, "x:", 0)
	if err == nil {
		t.Errorf("NewKVReplayStore(nil) returned nil error")
	}
	if r != nil {
		t.Errorf("NewKVReplayStore(nil) returned non-nil store: %T", r)
	}
}

// TestReplayMiddlewareReplaysMissedEvents — end-to-end exit criterion
// from issue #250: client connects with Last-Event-ID, receives only
// the events appended AFTER that ID, then any new events from Send.
func TestReplayMiddlewareReplaysMissedEvents(t *testing.T) {
	r := NewRingBuffer(16)
	ctx := context.Background()

	// Pre-populate the store as if a previous session had sent these.
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		id, err := r.Append(ctx, Event{Data: "old-" + string(rune('a'+i))})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	reqCtx, ms := newSSEContext(t, celeristest.WithHeader("last-event-id", ids[1]))

	handler := New(Config{
		HeartbeatInterval: -1,
		ReplayStore:       r,
		Handler: func(client *Client) {
			_ = client.Send(Event{Data: "fresh-1"})
		},
	})
	if err := handler(reqCtx); err != nil {
		t.Fatal(err)
	}
	wire := ms.allData()
	// Replay phase: only id 3 should appear (events appended after id 2).
	if !strings.Contains(wire, "data: old-c\n\n") {
		t.Errorf("missing replay event %q in wire %q", "old-c", wire)
	}
	if strings.Contains(wire, "data: old-a\n\n") || strings.Contains(wire, "data: old-b\n\n") {
		t.Errorf("wire replayed already-acked events: %q", wire)
	}
	// Fresh send: must carry the next sequential ID.
	if !strings.Contains(wire, "data: fresh-1\n\n") {
		t.Errorf("missing fresh event in wire %q", wire)
	}
	if !strings.Contains(wire, "id: 4\n") {
		t.Errorf("fresh event missing canonical id 4 in wire %q", wire)
	}
}

// TestReplayMiddlewareUnknownLastIDFallsThrough — the ErrLastIDUnknown
// path: middleware does NOT abort, it still invokes Handler. The
// original header value remains observable via client.LastEventID().
func TestReplayMiddlewareUnknownLastIDFallsThrough(t *testing.T) {
	r := NewRingBuffer(2)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		_, _ = r.Append(ctx, Event{Data: "x"})
	}

	reqCtx, ms := newSSEContext(t, celeristest.WithHeader("last-event-id", "1"))

	var sawHandlerCall bool
	var sawHeader string
	handler := New(Config{
		HeartbeatInterval: -1,
		ReplayStore:       r,
		Handler: func(client *Client) {
			sawHandlerCall = true
			sawHeader = client.LastEventID()
			_ = client.Send(Event{Data: "fresh"})
		},
	})
	if err := handler(reqCtx); err != nil {
		t.Fatal(err)
	}
	if !sawHandlerCall {
		t.Errorf("Handler not invoked after ErrLastIDUnknown")
	}
	if sawHeader != "1" {
		t.Errorf("Client.LastEventID() = %q, want %q", sawHeader, "1")
	}
	if !strings.Contains(ms.allData(), "data: fresh\n\n") {
		t.Errorf("fresh event not on wire: %q", ms.allData())
	}
}
