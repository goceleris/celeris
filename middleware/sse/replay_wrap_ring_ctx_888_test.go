package sse

import (
	"context"
	"strings"
	"testing"
	"unsafe"
)

// A decorator that derives a context of its own before it calls the ring (a
// timeout, a span) hides the drain's context from the ring, which then copies as
// it did before celeris#888 item 24: the replayed events are right, and the
// saving is not there. The limit is stated in ownedCtx.
type deriveStore888 struct {
	ReplayStore
	seen []*byte
}

func (w *deriveStore888) Append(ctx context.Context, e Event) (string, error) {
	w.seen = append(w.seen, unsafe.StringData(e.Data))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	return w.ReplayStore.Append(ctx, e)
}

func TestWrapperThatDerivesItsContextGetsACopy888(t *testing.T) {
	w := &deriveStore888{ReplayStore: NewRingBuffer(4)}
	runQueued888(t, w, 2)
	evs, err := w.Since(context.Background(), "")
	if err != nil || len(evs) != 2 {
		t.Fatalf("replayed %d events, err %v, want 2", len(evs), err)
	}
	for i, e := range evs {
		if want := strings.Repeat("x", 40) + string(rune('a'+i)); e.Data != want {
			t.Errorf("event %d: Data %q, want %q", i, e.Data, want)
		}
		if unsafe.StringData(e.Data) == w.seen[i] {
			t.Errorf("event %d: the ring kept the wrapper's string although the context it was given was not the drain's", i)
		}
	}
}
