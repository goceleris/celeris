package stream

import (
	"runtime"
	"testing"

	"golang.org/x/net/http2"
)

// celeris#889 item 38: PRIORITY on an idle stream leaves a placeholder
// (RFC 9113 section 5.3.2 deprecates the frame, section 5.1.1 says an
// identifier at or below one the peer has used can never be opened). A
// placeholder the last client stream has passed is dead weight, and
// maxPriorityPlaceholders of them made every later PRIORITY on an idle stream
// be ignored for the life of the connection.

func priority889(t *testing.T, p *Processor, id, dep uint32) {
	t.Helper()
	if err := p.handlePriority(&http2.PriorityFrame{
		FrameHeader:   http2.FrameHeader{Type: http2.FramePriority, StreamID: id},
		PriorityParam: http2.PriorityParam{StreamDep: dep, Weight: 15},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPlaceholdersThePeerHasPassedAreReapedAtTheBound889(t *testing.T) {
	x := newProc944()
	m := x.manager
	// A client builds a priority tree ahead of its requests: 16 idle placeholders.
	var old []uint32
	for i := range maxPriorityPlaceholders {
		id := uint32(2*i + 3)
		old = append(old, id)
		priority889(t, x.Processor, id, 0)
	}
	if got := m.placeholders; got != maxPriorityPlaceholders {
		t.Fatalf("self-check: %d placeholders, want %d", got, maxPriorityPlaceholders)
	}
	// At the bound, a PRIORITY for another idle stream is ignored.
	priority889(t, x.Processor, 201, 0)
	if _, ok := m.GetStream(201); ok {
		t.Fatal("self-check: a placeholder was created past the bound")
	}

	// The client opens a stream far past them all: every placeholder is now a
	// stream that can never open (RFC 9113 section 5.1.1).
	if err := feed944(t, x, "raw", 101, true, encodeHeaders(t, reqHeaders944())); err != nil {
		t.Fatal(err)
	}

	// A PRIORITY on a new idle stream makes room by reaping them.
	priority889(t, x.Processor, 103, 0)
	if _, ok := m.GetStream(103); !ok {
		t.Fatalf("PRIORITY on idle stream 103 was ignored with %d placeholders the last client stream (101) has passed: they keep their slot until the connection closes", len(old))
	}
	if _, ok := m.priorityTree.GetPriority(103); !ok {
		t.Error("the priority tree has no entry for the new placeholder")
	}
	for _, id := range old {
		if _, ok := m.GetStream(id); ok {
			t.Errorf("placeholder %d, passed by stream 101, was not reaped", id)
		}
		if _, ok := m.priorityTree.GetPriority(id); ok {
			t.Errorf("the priority tree still has stream %d, whose placeholder was reaped", id)
		}
	}
	if got := m.placeholders; got != 1 {
		t.Errorf("placeholders = %d, want 1", got)
	}
}

// Placeholders above the last client stream are live: they are never reaped, and
// the bound holds. And a PRIORITY past the bound with nothing to reap costs no
// scan of the connection's streams (see CreatePlaceholder): it is ignored.
func TestLivePlaceholdersAreNotReaped889(t *testing.T) {
	x := newProc944()
	m := x.manager
	for i := range maxPriorityPlaceholders {
		priority889(t, x.Processor, uint32(2*i+301), 0)
	}
	for i := range 100 {
		priority889(t, x.Processor, uint32(2*i+501), 0)
	}
	if got := m.placeholders; got != maxPriorityPlaceholders {
		t.Fatalf("placeholders = %d, want the bound %d", got, maxPriorityPlaceholders)
	}
	for i := range maxPriorityPlaceholders {
		if _, ok := m.GetStream(uint32(2*i + 301)); !ok {
			t.Errorf("live placeholder %d was reaped", 2*i+301)
		}
	}
	if _, ok := m.GetStream(501); ok {
		t.Error("a placeholder was created past the bound with every other one live")
	}
}

// The books of the manager stay right through a reap and a replacement.
func TestPlaceholderBooksSurviveReaping889(t *testing.T) {
	m := NewManager()
	for i := range maxPriorityPlaceholders {
		if _, ok := m.CreatePlaceholder(uint32(2*i + 3)); !ok {
			t.Fatal("placeholder refused below the bound")
		}
	}
	m.lastClientStream.Store(99) // the peer has used identifiers up to 99
	s, ok := m.CreatePlaceholder(101)
	if !ok || s == nil {
		t.Fatal("no room after every placeholder was passed by the last client stream")
	}
	if got := m.placeholders; got != 1 {
		t.Fatalf("placeholders = %d, want 1", got)
	}
	// It is still a placeholder: replaced by a stream that opens on it, deleted,
	// and uncounted either way.
	if _, ok := m.TryOpenStream(101); !ok {
		t.Fatal("TryOpenStream on the placeholder's identifier failed")
	}
	if got := m.placeholders; got != 0 {
		t.Errorf("placeholders = %d after the placeholder was opened, want 0", got)
	}
	runtime.KeepAlive(s)
}

// A placeholder's identifier whose entry is another stream's (CreateStream
// overwrites without asking) is only forgotten by a reap: the live stream is
// neither deleted from the map nor released.
func TestReapNeverTakesALiveStream889(t *testing.T) {
	m := NewManager()
	for i := range maxPriorityPlaceholders {
		if _, ok := m.CreatePlaceholder(uint32(2*i + 3)); !ok {
			t.Fatal("placeholder refused below the bound")
		}
	}
	live := m.CreateStream(5) // over the placeholder on 5, which is not dropped from the books
	live.SetState(StateOpen)
	m.lastClientStream.Store(99)
	if _, ok := m.CreatePlaceholder(101); !ok {
		t.Fatal("no room after the placeholders were passed")
	}
	got, ok := m.GetStream(5)
	if !ok || got != live || got.GetState() != StateOpen || got.ID != 5 {
		t.Fatalf("the live stream 5 was taken by the reap: in the map %v, same object %v", ok, got == live)
	}
	if got := m.placeholders; got != 1 {
		t.Errorf("placeholders = %d, want 1 (the new one)", got)
	}
}

// A reap that only forgets stale identifiers (the entries are another stream's)
// has freed a slot all the same: the PRIORITY that asked for it is served.
func TestReapThatOnlyForgetsStaleIDsStillMakesRoom889(t *testing.T) {
	m := NewManager()
	for i := range maxPriorityPlaceholders {
		if _, ok := m.CreatePlaceholder(uint32(2*i + 3)); !ok {
			t.Fatal("placeholder refused below the bound")
		}
	}
	for i := range maxPriorityPlaceholders { // every placeholder overwritten by a live stream
		s := m.CreateStream(uint32(2*i + 3))
		s.SetState(StateOpen)
	}
	m.lastClientStream.Store(99)
	if _, ok := m.CreatePlaceholder(101); !ok {
		t.Fatal("PRIORITY refused although every identifier on the books was stale")
	}
	if got := m.placeholders; got != 1 {
		t.Errorf("placeholders = %d, want 1", got)
	}
}
