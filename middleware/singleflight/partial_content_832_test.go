package singleflight

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// TestSingleflightDoesNotShareAPartialResponse832 is celeris#832's family in
// singleflight. Requests that arrive while a leader with the same key is in
// flight get the leader's response, and the default key (method, path, query,
// credentials) does not include Range: a full GET that joined a ranged leader
// got the leader's 206. A request with a Range header is not coalesced, so no
// leader is ranged and no ranged request waits on a full one.
//
// The leader's handler blocks until the second request has either joined it
// (testHookWaiterJoined) or finished on its own, so the interleaving is the
// same on every run.
func TestSingleflightDoesNotShareAPartialResponse832(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name                     string
		leaderRange, secondRange string
	}{
		{"ranged-leader-then-full-get", "bytes=0-3", ""},
		{"full-leader-then-ranged-get", "", "bytes=0-3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entered, gate := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			h := func(c *celeris.Context) error {
				if calls.Add(1) == 1 {
					close(entered)
					<-gate
				}
				return c.File(p)
			}
			mw := New()
			joined := make(chan struct{}, 1)
			prev := testHookWaiterJoined
			testHookWaiterJoined = func() {
				select {
				case joined <- struct{}{}:
				default:
				}
			}
			t.Cleanup(func() { testHookWaiterJoined = prev })

			do := func(rangeHdr string) (*celeristest.ResponseRecorder, *celeris.Context) {
				opts := []celeristest.Option{celeristest.WithHandlers(mw, h)}
				if rangeHdr != "" {
					opts = append(opts, celeristest.WithHeader("range", rangeHdr))
				}
				c, rec := celeristest.NewContext("GET", "/f", opts...)
				_ = c.Next()
				return rec, c
			}
			var wg sync.WaitGroup
			var leaderRec, secondRec *celeristest.ResponseRecorder
			var ctxs [2]*celeris.Context
			wg.Add(1)
			go func() { defer wg.Done(); leaderRec, ctxs[0] = do(tc.leaderRange) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the leader's handler did not start")
			}
			secondDone := make(chan struct{})
			wg.Add(1)
			go func() { defer wg.Done(); defer close(secondDone); secondRec, ctxs[1] = do(tc.secondRange) }()
			outcome := ""
			select {
			case <-joined:
				outcome = "joined the leader"
			case <-secondDone:
				outcome = "ran on its own"
			case <-time.After(5 * time.Second):
				t.Fatal("the second request neither joined the leader nor finished")
			}
			close(gate)
			wg.Wait()
			defer func() {
				for _, c := range ctxs {
					celeristest.ReleaseContext(c)
				}
			}()

			check := func(who, rangeHdr string, r *celeristest.ResponseRecorder) {
				switch {
				case rangeHdr == "" && r.StatusCode == 200 && string(r.Body) == "0123456789":
				case rangeHdr != "" && r.StatusCode == 206 && string(r.Body) == "0123" && r.Header("content-range") == "bytes 0-3/10":
				case rangeHdr != "" && r.StatusCode == 200 && string(r.Body) == "0123456789":
					// A server may answer a Range with the full representation.
				default:
					t.Errorf("%s (Range %q, %s): %d %q content-range %q x-singleflight %q, want the answer to its own request",
						who, rangeHdr, outcome, r.StatusCode, r.Body, r.Header("content-range"), r.Header("x-singleflight"))
				}
			}
			check("leader", tc.leaderRange, leaderRec)
			check("second request", tc.secondRange, secondRec)
			t.Logf("%s: second request %s; leader %d, second %d", tc.name, outcome, leaderRec.StatusCode, secondRec.StatusCode)
		})
	}
}
