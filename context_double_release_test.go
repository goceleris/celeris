package celeris

import (
	"testing"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

// TestReleaseContextIsIdempotent is the celeris#512 regression guard.
//
// Returning one Context to contextPool twice lets two concurrent Get calls
// hand the SAME object to two goroutines, which then both write c.stream and
// c.index in acquireContext. The resulting data race does not surface at the
// double-release site — it surfaces in whatever unrelated code next draws
// from the pool, which is why the original report pointed at a session
// write-behind test that was entirely innocent.
//
// Two independent test helpers had this bug (a NewContextT that registers a
// cleanup release, paired with an explicit release), so the guard belongs in
// the release path itself rather than only in the callers.
func TestReleaseContextIsIdempotent(t *testing.T) {
	s := stream.NewStream(1)
	s.Headers = append(s.Headers,
		[2]string{":method", "GET"},
		[2]string{":path", "/"},
		[2]string{":scheme", "http"},
		[2]string{":authority", "localhost"},
	)
	c := acquireContext(s)

	releaseContext(c)
	releaseContext(c) // must be a no-op, not a second pool.Put

	// The pool must not hand this pointer out twice while it is "held".
	const n = 64
	seen := make(map[*Context]int, n)
	held := make([]*Context, 0, n)
	for i := 0; i < n; i++ {
		st := stream.NewStream(uint32(i + 2))
		st.Headers = append(st.Headers,
			[2]string{":method", "GET"},
			[2]string{":path", "/"},
			[2]string{":scheme", "http"},
			[2]string{":authority", "localhost"},
		)
		got := acquireContext(st)
		seen[got]++
		held = append(held, got)
	}
	for ptr, count := range seen {
		if count > 1 {
			t.Fatalf("contextPool handed out %p %d times while it was still held: "+
				"the pool contains a duplicate pointer, so two goroutines can own "+
				"one Context (celeris#512)", ptr, count)
		}
	}
	for _, h := range held {
		releaseContext(h)
	}
}
