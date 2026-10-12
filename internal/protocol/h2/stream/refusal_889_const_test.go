package stream

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/net/http2"
)

// celeris#889: the exact bounds. The tests in refusal_889_test.go pin the
// behaviour; these pin the numbers and the books that keep them.

// clock889 is the clock of noteRefusal in these tests.
type clock889 struct{ sec atomic.Int64 }

func (c *clock889) now() int64 { return c.sec.Load() }

func newClocked889() (*proc944, *clock889) {
	p := newProc944()
	c := &clock889{}
	c.sec.Store(1_000_000)
	p.nowUnix = c.now
	return p, c
}

// A window admits its burst and ends the connection on the next refusal; the
// next second starts a new window; the two kinds of refusal are counted apart.
func TestRefusalWindow889(t *testing.T) {
	t.Run("stream-limit-burst", func(t *testing.T) {
		p, _ := newClocked889()
		for i := range streamRefusalBurstMax {
			if err := p.noteRefusal(true); err != nil {
				t.Fatalf("refusal %d of %d: %v", i+1, streamRefusalBurstMax, err)
			}
		}
		if err := p.noteRefusal(true); err == nil {
			t.Fatalf("refusal %d in one second did not end the connection", streamRefusalBurstMax+1)
		}
		goaways, _ := sinks889(p.Processor)
		if len(goaways) != 1 || goaways[0].code != http2.ErrCodeEnhanceYourCalm {
			t.Fatalf("GOAWAYs = %+v, want one ENHANCE_YOUR_CALM", goaways)
		}
	})
	t.Run("header-list-burst", func(t *testing.T) {
		p, _ := newClocked889()
		for i := range headerListRefusalBurstMax {
			if err := p.noteRefusal(false); err != nil {
				t.Fatalf("refusal %d of %d: %v", i+1, headerListRefusalBurstMax, err)
			}
		}
		if err := p.noteRefusal(false); err == nil {
			t.Fatalf("refusal %d in one second did not end the connection", headerListRefusalBurstMax+1)
		}
	})
	t.Run("the-next-second-is-a-new-window", func(t *testing.T) {
		p, c := newClocked889()
		for round := range 3 {
			for i := range streamRefusalBurstMax {
				if err := p.noteRefusal(true); err != nil {
					t.Fatalf("round %d refusal %d: %v", round, i+1, err)
				}
			}
			for i := range headerListRefusalBurstMax {
				if err := p.noteRefusal(false); err != nil {
					t.Fatalf("round %d header-list refusal %d: %v", round, i+1, err)
				}
			}
			c.sec.Add(1)
		}
		if goaways, _ := sinks889(p.Processor); len(goaways) != 0 {
			t.Fatalf("GOAWAYs = %+v, want none: every second stayed within its burst", goaways)
		}
	})
	t.Run("the-two-kinds-are-counted-apart", func(t *testing.T) {
		p, _ := newClocked889()
		for range streamRefusalBurstMax {
			_ = p.noteRefusal(true)
		}
		for i := range headerListRefusalBurstMax {
			if err := p.noteRefusal(false); err != nil {
				t.Fatalf("header-list refusal %d after %d stream refusals: %v", i+1, streamRefusalBurstMax, err)
			}
		}
	})
	t.Run("burst-is-above-200-a-second", func(t *testing.T) {
		// A client that has not read SETTINGS may open streams past the limit
		// at once (RFC 9113 section 6.5.2: initially there is none), and
		// RST_STREAM's own burst is 200.
		if streamRefusalBurstMax <= 1000 || streamRefusalBurstMax <= rstBurstMax {
			t.Errorf("streamRefusalBurstMax = %d, want above 1000 (a client with that many requests ready, before SETTINGS) and rstBurstMax (%d)", streamRefusalBurstMax, rstBurstMax)
		}
	})
}

// The books of the placeholders: created up to the bound, uncounted when the
// stream is opened over one, deleted or reset, or the peer's GOAWAY drops it.
func TestPlaceholderBooks889(t *testing.T) {
	count := func(m *Manager) int {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.placeholders
	}
	fill := func(m *Manager) {
		t.Helper()
		for i := range maxPriorityPlaceholders {
			if _, ok := m.CreatePlaceholder(uint32(2*i + 3)); !ok {
				t.Fatalf("placeholder %d of %d refused", i+1, maxPriorityPlaceholders)
			}
		}
		if _, ok := m.CreatePlaceholder(1001); ok {
			t.Fatalf("placeholder %d was created, the bound is %d", maxPriorityPlaceholders+1, maxPriorityPlaceholders)
		}
		if got := count(m); got != maxPriorityPlaceholders {
			t.Fatalf("placeholders = %d, want %d", got, maxPriorityPlaceholders)
		}
	}
	room := func(t *testing.T, m *Manager, why string) {
		t.Helper()
		if _, ok := m.CreatePlaceholder(1001); !ok {
			t.Errorf("no room for a placeholder after %s", why)
		}
	}

	t.Run("creating-moves-nothing-else", func(t *testing.T) {
		m := NewManager()
		if _, ok := m.CreatePlaceholder(2001); !ok {
			t.Fatal("refused")
		}
		if got := m.GetLastStreamID(); got != 0 {
			t.Errorf("GetLastStreamID = %d after a placeholder, want 0", got)
		}
		if got := m.GetLastClientStreamID(); got != 0 {
			t.Errorf("last client stream = %d after a placeholder, want 0", got)
		}
		if got := m.CountActiveStreams(); got != 0 {
			t.Errorf("active streams = %d after a placeholder, want 0", got)
		}
	})
	t.Run("a-stream-held-is-returned-as-it-is", func(t *testing.T) {
		m := NewManager()
		a, _ := m.CreatePlaceholder(3)
		b, ok := m.CreatePlaceholder(3)
		if !ok || a != b || count(m) != 1 {
			t.Errorf("second CreatePlaceholder(3) = %p, %v (first %p), placeholders %d", b, ok, a, count(m))
		}
	})
	t.Run("opened-over", func(t *testing.T) {
		m := NewManager()
		fill(m)
		ph, _ := m.GetStream(3)
		s, ok := m.TryOpenStream(3)
		if !ok || s == ph {
			t.Fatalf("TryOpenStream(3) = %p, %v; the placeholder is %p", s, ok, ph)
		}
		// Before anything else takes a stream from the pool, which may hand ph back.
		if ph.flags.Load()&flagPlaceholder != 0 {
			t.Error("the placeholder the stream replaced was not released")
		}
		room(t, m, "a stream was opened over a placeholder")
		if got := m.GetLastStreamID(); got != 3 {
			t.Errorf("GetLastStreamID = %d, want 3", got)
		}
	})
	t.Run("opened-over-and-refused-keeps-it", func(t *testing.T) {
		m := NewManager()
		m.SetMaxConcurrentStreams(1)
		if _, ok := m.TryOpenStream(1); !ok {
			t.Fatal("stream 1")
		}
		if _, ok := m.CreatePlaceholder(3); !ok {
			t.Fatal("placeholder")
		}
		if _, ok := m.TryOpenStream(3); ok {
			t.Fatal("stream 3 opened past the limit")
		}
		if got := count(m); got != 1 {
			t.Errorf("placeholders = %d after a refused open, want 1", got)
		}
		if _, ok := m.GetStream(3); !ok {
			t.Error("the placeholder was dropped by a refused open")
		}
	})
	t.Run("deleted", func(t *testing.T) {
		m := NewManager()
		fill(m)
		m.DeleteStream(5)
		room(t, m, "DeleteStream")
	})
	t.Run("reset-by-the-peer", func(t *testing.T) {
		m := NewManager()
		fill(m)
		if !m.resetStream(5) {
			t.Fatal("resetStream(5) found nothing")
		}
		room(t, m, "RST_STREAM")
	})
	t.Run("dropped-by-the-peers-goaway", func(t *testing.T) {
		p := newProc944()
		fill(p.manager)
		// The peer's GOAWAY(last 7) drops every stream above 7: the placeholders 9 and up.
		if err := processFrames981(t, p.Processor, func(w *http2.Framer) {
			if err := w.WriteGoAway(7, http2.ErrCodeNo, nil); err != nil {
				t.Fatal(err)
			}
		}); err != nil {
			t.Fatal(err)
		}
		if got := count(p.manager); got != 3 { // 3, 5, 7
			t.Errorf("placeholders = %d after a GOAWAY naming stream 7, want 3", got)
		}
	})
	t.Run("closed", func(t *testing.T) {
		m := NewManager()
		fill(m)
		m.Close()
		if got := count(m); got != 0 {
			t.Errorf("placeholders = %d after Close, want 0", got)
		}
	})
}

// A PRIORITY frame on an idle stream followed by HEADERS on it keeps the
// priority the peer gave: the stream opened over the placeholder is in the
// tree with it.
func TestPriorityOfAPlaceholderSurvivesTheOpen889(t *testing.T) {
	p := newProc944()
	if err := processFrames981(t, p.Processor, func(w *http2.Framer) {
		if err := w.WritePriority(3, http2.PriorityParam{StreamDep: 0, Weight: 200, Exclusive: false}); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.ProcessRawHeaders(3, false, newEnc981().req()); err != nil {
		t.Fatal(err)
	}
	pr, ok := p.manager.priorityTree.GetPriority(3)
	if !ok || pr.Weight != 200 {
		t.Errorf("priority of stream 3 = %+v, %v, want weight 200", pr, ok)
	}
}

// The placeholders and the streams are shared by the event loop (frames), the
// pool handlers' goroutines (retire) and the shutdown path (OutboundPending,
// StreamCount): the changes took no new lock and nest none, so this finds no
// deadlock (the test times out) and, with -race, no race.
func TestPlaceholderLocksDoNotDeadlock889(t *testing.T) {
	m := NewManager()
	m.SetMaxConcurrentStreams(1 << 20)
	p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), newTestFrameWriter(), newTestResponseWriter())
	p.manager = m
	const rounds = 3000
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 3 { // readers: the shutdown path and metrics
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = p.OutboundPending()
				_ = m.StreamCount()
				_ = m.GetLastStreamID()
				_ = m.CountActiveStreams()
			}
		}()
	}
	var loop sync.WaitGroup
	loop.Add(1)
	go func() { // the event loop
		defer loop.Done()
		for i := range rounds {
			id := uint32(4*i + 1)
			m.CreatePlaceholder(id + 2)
			m.CreatePlaceholder(id)
			if s, ok := m.TryOpenStream(id); ok {
				m.retire(s) // what a pool handler's goroutine does when it returns
				m.drainRetired()
			}
			m.resetStream(id + 2)
			m.DeleteStream(id)
		}
	}()
	loop.Wait()
	close(stop)
	wg.Wait()
	m.mu.RLock()
	got := m.placeholders
	m.mu.RUnlock()
	if got != 0 {
		t.Errorf("placeholders = %d after every one was opened over or reset, want 0", got)
	}
}

// serverResets remembers the odd identifiers of a sliding window and nothing
// else: not even ones, not ones that were not marked, not ones the window has
// slid past, and not the position an older identifier left behind.
func TestServerResets889(t *testing.T) {
	var r serverResets
	for _, id := range []uint32{1, 2, 3, 0} {
		if r.has(id) {
			t.Fatalf("empty set has %d", id)
		}
	}
	r.mark(3)
	r.mark(2) // even: never recorded
	r.mark(0)
	if !r.has(3) || r.has(1) || r.has(5) || r.has(2) || r.has(0) {
		t.Errorf("after mark(3): has(3)=%v has(1)=%v has(5)=%v has(2)=%v has(0)=%v", r.has(3), r.has(1), r.has(5), r.has(2), r.has(0))
	}
	// The window holds serverResetWindow odd identifiers.
	last := uint32(2*serverResetWindow + 3) // 3 is serverResetWindow+1 odd identifiers below it: just out of the window, and in the same position as last
	r.mark(last)
	if !r.has(last) {
		t.Error("the identifier just marked is not held")
	}
	if r.has(3) {
		t.Errorf("identifier 3 is held with the window at %d, it has slid out", last)
	}
	// 3 and last share a position in the window: 3 must not read as marked through last's bit,
	// and an identifier that was not marked must not read as marked through another's.
	if r.has(last - 2*serverResetWindow) {
		t.Errorf("identifier %d reads as marked through the position of %d", last-2*serverResetWindow, last)
	}
	r.mark(last - 2) // inside the window, below the top
	if !r.has(last-2) || !r.has(last) || r.has(last-4) {
		t.Errorf("after mark(%d): has(%d)=%v has(%d)=%v has(%d)=%v", last-2, last-2, r.has(last-2), last, r.has(last), last-4, r.has(last-4))
	}
	r.mark(1) // older than the window: dropped
	if r.has(1) {
		t.Error("an identifier older than the window was recorded")
	}
	// A jump of more than the window clears it.
	far := last + 4*serverResetWindow
	r.mark(far)
	if !r.has(far) || r.has(last) || r.has(last-2) {
		t.Errorf("after a jump to %d: has(far)=%v has(%d)=%v has(%d)=%v", far, r.has(far), last, r.has(last), last-2, r.has(last-2))
	}
	// A slide by a few steps keeps the identifiers still inside the window.
	r.mark(far + 6)
	if !r.has(far) || !r.has(far+6) || r.has(far+2) || r.has(far+4) {
		t.Errorf("after a slide: has(far)=%v has(far+6)=%v has(far+2)=%v has(far+4)=%v", r.has(far), r.has(far+6), r.has(far+2), r.has(far+4))
	}
}

// The refused streams of a burst of serverResetWindow requests are all still
// remembered when the last one has been refused.
func TestServerResetsHoldAFullBurst889(t *testing.T) {
	var r serverResets
	for i := range serverResetWindow {
		r.mark(uint32(2*i + 1))
	}
	for i := range serverResetWindow {
		if id := uint32(2*i + 1); !r.has(id) {
			t.Fatalf("identifier %d of a burst of %d is not remembered", id, serverResetWindow)
		}
	}
}
