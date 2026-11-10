package postgres

import (
	"testing"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/internal/looptest"
)

// celeris#1238: the Pool is the reference for the pairing of Resolve and
// Release. Close gives back exactly the reference Open took, once, whether the
// Pool has no engine or one with no event loop of its own, and a second Close
// gives back nothing (it would drop a reference that belongs to another user of
// the loop).
func TestPoolCloseReleasesTheStandaloneLoopOnce1238(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts func(t *testing.T) []Option
	}{
		{"no engine", func(*testing.T) []Option { return nil }},
		{"server not started", func(*testing.T) []Option { return []Option{WithEngine(looptest.Unstarted())} }},
		{"std without AsyncHandlers", func(t *testing.T) []Option { return []Option{WithEngine(looptest.StartStd(t))} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := eventloop.StandaloneRefs()
			// Another user of the loop, whose reference must survive the Pool.
			other, err := eventloop.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer eventloop.Release(other)
			pool, err := Open("postgres://u:p@127.0.0.1:1/db?sslmode=disable", tc.opts(t)...)
			if err != nil {
				t.Fatal(err)
			}
			if got := eventloop.StandaloneRefs(); got != before+2 {
				t.Errorf("open: references = %d, want %d", got, before+2)
			}
			_ = pool.Close()
			if got := eventloop.StandaloneRefs(); got != before+1 {
				t.Errorf("closed: references = %d, want %d", got, before+1)
			}
			_ = pool.Close()
			if got := eventloop.StandaloneRefs(); got != before+1 {
				t.Errorf("closed twice: references = %d, want %d", got, before+1)
			}
		})
	}
}
