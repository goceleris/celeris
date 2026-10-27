package stream

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/net/http2"
)

// handleGoAway ends the streams above the peer's last stream ID and lowers the
// manager's counts for them (celeris#889). A pool handler's goroutine ends its
// own stream with SetState(StateClosed), a Swap, without the manager's lock.
// handleGoAway used to read the state and then Store Closed: if the goroutine's
// Swap came between the two, both lowered the counts for the one stream, and
// with a bystander stream whose request is still owed (requestOpen 1) the
// count read 0, ServerOwes() true, and a silent client was exempt from
// ReadTimeout. The window is a few nanoseconds, so this test hands the stream
// to a spinning goroutine and calls handleGoAway at once, until the
// bystander's counts are wrong or a bound is reached. It is a race, not a
// deterministic interleaving: on the old code it failed in 5 of 5 runs at -cpu 2
// (round 43 in one), and it never fails on the fixed one.
func TestGoAwayAndTheHandlerEndingTheStreamLowerTheCountsOnce889(t *testing.T) {
	const rounds = 300000
	p := &Processor{manager: NewManager()}
	m := p.manager
	if _, ok := m.TryOpenStream(1); !ok { // the bystander: its request is not whole
		t.Fatal("no stream slot")
	}
	var (
		cur  atomic.Pointer[Stream] // the stream the goroutine is to end next
		done atomic.Bool
		quit atomic.Bool
		wg   sync.WaitGroup
	)
	wg.Add(1)
	go func() { // the pool goroutine, spinning so that it ends the stream the instant it is handed one
		defer wg.Done()
		for !quit.Load() {
			if s := cur.Swap(nil); s != nil {
				s.SetState(StateClosed)
				done.Store(true)
			}
		}
	}()
	defer func() { quit.Store(true); wg.Wait() }()
	for i := 0; i < rounds; i++ {
		id := uint32(3 + 2*i)
		s, ok := m.TryOpenStream(id)
		if !ok {
			t.Fatal("no stream slot")
		}
		s.flags.Store(flagAsyncRunning) // a pool handler's stream: handleGoAway leaves its release to the goroutine
		s.SetState(StateHalfClosedLocal)
		done.Store(false)
		cur.Store(s)
		_ = p.handleGoAway(&http2.GoAwayFrame{LastStreamID: 1})
		for !done.Load() {
			if cur.Load() == nil && !done.Load() {
				runtime.Gosched()
			}
		}
		if m.ServerOwes() || m.requestOpen.Load() != 1 || m.activeStreams.Load() != 1 {
			t.Fatalf("round %d: after one stream ended, active %d, request open %d, ServerOwes %v; want 1, 1, false (the bystander's counts were lowered for it too)",
				i, m.activeStreams.Load(), m.requestOpen.Load(), m.ServerOwes())
		}
	}
}
