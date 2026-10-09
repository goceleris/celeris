package std

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestH2StreamsCountClosesOnceAndRefusesAfter pins the count the drain closes
// (celeris#878): the close succeeds only at zero, one stream that is in a
// handler holds it open, and once it has succeeded every arrival is refused
// and leaves the count as it found it.
func TestH2StreamsCountClosesOnceAndRefusesAfter(t *testing.T) {
	var e Engine
	if !e.enterH2Stream() {
		t.Fatal("a stream was refused before the drain ended")
	}
	if e.closeH2Streams() {
		t.Fatal("the count closed with a stream in its handler")
	}
	e.h2Streams.Add(-1)
	if !e.closeH2Streams() {
		t.Fatal("the count did not close at zero")
	}
	for i := 0; i < 3; i++ {
		if e.enterH2Stream() {
			t.Fatal("a stream was admitted after the count closed")
		}
	}
	if got := e.h2Streams.Load(); got != h2StreamsClosed {
		t.Fatalf("refused arrivals moved the closed count to %d, want %d", got, h2StreamsClosed)
	}
	if e.closeH2Streams() {
		t.Fatal("the count closed twice")
	}

	// drained alone refuses too: the budget ran out with streams still in
	// their handlers, so the count never reached zero.
	var f Engine
	f.drained.Store(true)
	if f.enterH2Stream() {
		t.Fatal("a stream was admitted after the drain ended")
	}
	if got := f.h2Streams.Load(); got != 0 {
		t.Fatalf("a refused arrival left the count at %d, want 0", got)
	}
}

// TestH2StreamsCloseRacesArrivals: an arrival is either counted before the
// close, and the close then fails until it leaves, or refused after it; none
// is admitted past a successful close, and the count balances.
func TestH2StreamsCloseRacesArrivals(t *testing.T) {
	for round := 0; round < 200; round++ {
		var e Engine
		var closed atomic.Bool
		var admittedAfterClose atomic.Int64
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					wasClosed := closed.Load()
					if e.enterH2Stream() {
						if wasClosed {
							admittedAfterClose.Add(1)
						}
						e.h2Streams.Add(-1)
					}
				}
			}()
		}
		for !e.closeH2Streams() {
		}
		closed.Store(true)
		// Let the arrivals run against the closed count for a moment.
		for i := 0; i < 1000; i++ {
			e.enterH2Stream()
		}
		close(stop)
		wg.Wait()
		if n := admittedAfterClose.Load(); n != 0 {
			t.Fatalf("round %d: %d streams admitted after the count closed", round, n)
		}
		// Every refused arrival undid its own increment (the main goroutine's
		// too: it never calls enterH2Stream with a stream to end).
		if got := e.h2Streams.Load(); got != h2StreamsClosed {
			t.Fatalf("round %d: closed count is %d, want %d: an arrival's increment was not undone", round, got, h2StreamsClosed)
		}
	}
}
