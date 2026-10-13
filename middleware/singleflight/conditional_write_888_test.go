package singleflight

import (
	"sync"
	"testing"

	"github.com/goceleris/celeris"
)

// celeris#888 item 23, review round 2. The default key holds the four
// preconditions so that identical revalidations (GET and HEAD, whose answer is
// the same for the same validator) coalesce. A write with a precondition is
// another matter: two concurrent PUTs from one user with the same If-Match
// share a key, and the second would receive the first's response without its
// handler ever running: its update is silently dropped, and it never gets the
// 412 If-Match exists to give the loser. main did not coalesce any request
// carrying a precondition (celeris#912); the default key must not coalesce a
// conditional write either.

// recordingWrite applies a write: it records each body its handler is given.
type recordingWrite struct {
	mu     sync.Mutex
	bodies []string
}

func (w *recordingWrite) handler(c *celeris.Context) error {
	w.mu.Lock()
	w.bodies = append(w.bodies, string(c.Body()))
	w.mu.Unlock()
	return c.Blob(200, "text/plain", []byte("applied "+string(c.Body())))
}

func (w *recordingWrite) seen() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.bodies...)
}

// TestConditionalWritesAreNeverCoalesced888: whatever the precondition, a
// PUT, POST, PATCH or DELETE that carries one runs its own handler, with the
// default key.
func TestConditionalWritesAreNeverCoalesced888(t *testing.T) {
	for _, v := range validators888 {
		for _, method := range []string{"PUT", "POST", "PATCH", "DELETE"} {
			t.Run(method+"/"+v.name, func(t *testing.T) {
				var w recordingWrite
				hdr := [][2]string{{v.name, v.a}}
				r := runPairReq(t, New(), []celeris.HandlerFunc{w.handler},
					reqSpec{method: method, hdrs: hdr, body: "A"}, reqSpec{method: method, hdrs: hdr, body: "B"})
				if r.joined || r.calls != 2 {
					t.Errorf("two %s requests with the same %s: joined=%v, handler runs %d, want not joined and 2 runs (second got %q)",
						method, v.name, r.joined, r.calls, r.second.Body)
				}
				if got := w.seen(); len(got) != 2 {
					t.Errorf("the writes applied: %q, want both A and B", got)
				}
				if string(r.second.Body) != "applied B" {
					t.Errorf("the second request got %q, want the answer to its own write, %q", r.second.Body, "applied B")
				}
			})
		}
	}
}

// TestConditionalReadsStillCoalesce888 is the control: a GET or HEAD with the
// same validator is a revalidation, and joins its leader (the point of item
// 23), so the bypass for writes does not switch coalescing off for them.
func TestConditionalReadsStillCoalesce888(t *testing.T) {
	for _, v := range validators888 {
		for _, method := range []string{"GET", "HEAD"} {
			t.Run(method+"/"+v.name, func(t *testing.T) {
				hdr := [][2]string{{v.name, v.a}}
				r := runPairReq(t, New(), []celeris.HandlerFunc{plain},
					reqSpec{method: method, hdrs: hdr}, reqSpec{method: method, hdrs: hdr})
				if !r.joined || r.calls != 1 {
					t.Errorf("two %s requests with the same %s: joined=%v, handler runs %d, want joined and 1 run", method, v.name, r.joined, r.calls)
				}
			})
		}
	}
}
