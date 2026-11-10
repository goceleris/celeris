package sse

import (
	"context"
	"strings"
	"sync"
	"testing"
	"unsafe"
)

// A ReplayStore that wraps NewRingBuffer (a metrics or logging decorator) in
// queued mode (celeris#888 item 24). sendQueued copies each event before it
// queues it, and the drain hands that copy to the store; the ring keeps it
// without a second copy when it can tell the event is owned. A wrapper hides
// the ring from appendQueued's type test, so the ring has to tell by the event.

// wrapStore888 is the decorator: it forwards to the ring and notes the Data
// string it was handed.
type wrapStore888 struct {
	ReplayStore
	mu   sync.Mutex
	seen []*byte // unsafe.StringData of each appended event's Data
	mut  func(*Event)
}

func (w *wrapStore888) Append(ctx context.Context, e Event) (string, error) {
	if w.mut != nil {
		w.mut(&e)
	}
	w.mu.Lock()
	w.seen = append(w.seen, unsafe.StringData(e.Data))
	w.mu.Unlock()
	return w.ReplayStore.Append(ctx, e)
}

// runQueued888 sends n events of distinct data through a queued client whose
// replay store is w, and returns once the handler has returned (the queue is
// drained by then).
func runQueued888(t *testing.T, store ReplayStore, n int) {
	t.Helper()
	handler := New(Config{
		HeartbeatInterval: -1,
		MaxQueueDepth:     2 * n,
		ReplayStore:       store,
		Handler: func(client *Client) {
			for i := range n {
				if err := client.Send(Event{Event: "update", Data: strings.Repeat("x", 40) + string(rune('a'+i))}); err != nil {
					t.Errorf("Send %d: %v", i, err)
				}
			}
		},
	})
	ctx := newDiscardContext()
	if err := handler(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestWrappedRingKeepsTheQueuedCopy888: the ring behind a wrapper keeps the
// string the wrapper was handed. Before the fix it copied it a second time, so
// the replayed Data was another allocation (435 allocs against 335 per 100
// events in the finding's benchmark).
func TestWrappedRingKeepsTheQueuedCopy888(t *testing.T) {
	const n = 8
	w := &wrapStore888{ReplayStore: NewRingBuffer(2 * n)}
	runQueued888(t, w, n)
	evs, err := w.Since(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != n || len(w.seen) != n {
		t.Fatalf("replayed %d events, the wrapper saw %d, want %d of each", len(evs), len(w.seen), n)
	}
	for i, e := range evs {
		if unsafe.StringData(e.Data) != w.seen[i] {
			t.Errorf("event %d: the ring holds a second copy of Data (%q) instead of the string the wrapper was handed", i, e.Data)
		}
		if want := strings.Repeat("x", 40) + string(rune('a'+i)); e.Data != want {
			t.Errorf("event %d: Data %q, want %q", i, e.Data, want)
		}
	}
}

// TestWrapperThatReplacesDataStillGetsACopy888 is the guard for the fix: a
// decorator that puts a string of its own into the event (here a view of a
// buffer that changes later, as a request string is) is not forwarding what the
// queue owns, and the ring copies it.
func TestWrapperThatReplacesDataStillGetsACopy888(t *testing.T) {
	buf := []byte("view-of-a-reused-buffer")
	w := &wrapStore888{ReplayStore: NewRingBuffer(4), mut: func(e *Event) { e.Data = view(buf) }}
	runQueued888(t, w, 2)
	for k := range buf {
		buf[k] = 'z'
	}
	evs, err := w.Since(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("replayed %d events, want 2", len(evs))
	}
	for i, e := range evs {
		if e.Data != "view-of-a-reused-buffer" {
			t.Errorf("event %d: Data %q after the buffer changed: the ring kept the wrapper's view", i, e.Data)
		}
	}
}

// TestWrapperThatReplacesEventTypeStillGetsACopy888: the same for the Event
// field, which sits next to Data in the queue's copy.
func TestWrapperThatReplacesEventTypeStillGetsACopy888(t *testing.T) {
	buf := []byte("kind-view")
	w := &wrapStore888{ReplayStore: NewRingBuffer(4), mut: func(e *Event) { e.Event = view(buf) }}
	runQueued888(t, w, 1)
	for k := range buf {
		buf[k] = 'z'
	}
	evs, _ := w.Since(context.Background(), "")
	if len(evs) != 1 || evs[0].Event != "kind-view" {
		t.Fatalf("replayed %+v, want one event of type kind-view", evs)
	}
}
