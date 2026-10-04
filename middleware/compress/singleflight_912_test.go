package compress

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/singleflight"
)

// TestSingleflightOutsideCompressGivesEachRequestItsEncoding912 is
// celeris#912 with the real middleware, in the order singleflight's doc
// recommends (singleflight, then compress). The leader accepts gzip and a
// request that arrives while it runs sends no Accept-Encoding: the second
// request must get the identity body, not the leader's gzip body. It runs
// with the default key (Accept-Encoding is part of it) and with a key of
// method and path only (compress's Vary: Accept-Encoding sends the waiter to
// its own handler). The control sends gzip twice and is still coalesced.
//
// The second request is sent once the leader's handler is running and is
// given 200 ms to join it before the leader is released. A request that has
// not joined by then runs on its own and gets its own answer, so the timing
// can only make a failing run pass, never a passing run fail; the
// deterministic cases are in middleware/singleflight
// (representation_912_test.go).
func TestSingleflightOutsideCompressGivesEachRequestItsEncoding912(t *testing.T) {
	body := strings.Repeat("the full representation ", 200)
	pathOnly := func(c *celeris.Context) string { return c.Method() + "\x00" + c.Path() }
	cases := []struct {
		name     string
		cfg      singleflight.Config
		secondAE string
	}{
		{"default-key/gzip-then-identity", singleflight.Config{}, ""},
		{"path-key/gzip-then-identity", singleflight.Config{KeyFunc: pathOnly}, ""},
		{"control/default-key/gzip-then-gzip", singleflight.Config{}, "gzip"},
		{"control/path-key/gzip-then-gzip", singleflight.Config{KeyFunc: pathOnly}, "gzip"},
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
				return c.Blob(200, "text/plain", []byte(body))
			}
			sf, cz := singleflight.New(tc.cfg), New()
			var ctxs []*celeris.Context
			var mu sync.Mutex
			t.Cleanup(func() {
				for _, c := range ctxs {
					celeristest.ReleaseContext(c)
				}
			})
			do := func(ae string) *celeristest.ResponseRecorder {
				opts := []celeristest.Option{celeristest.WithHandlers(sf, cz, h)}
				if ae != "" {
					opts = append(opts, celeristest.WithHeader("accept-encoding", ae))
				}
				c, rec := celeristest.NewContext("GET", "/x", opts...)
				mu.Lock()
				ctxs = append(ctxs, c)
				mu.Unlock()
				_ = c.Next()
				return rec
			}
			var wg sync.WaitGroup
			var leader, second *celeristest.ResponseRecorder
			wg.Add(1)
			go func() { defer wg.Done(); leader = do("gzip") }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the leader's handler did not start")
			}
			secondDone := make(chan struct{})
			wg.Add(1)
			go func() { defer wg.Done(); defer close(secondDone); second = do(tc.secondAE) }()
			select {
			case <-secondDone:
			case <-time.After(200 * time.Millisecond):
			}
			close(gate)
			wg.Wait()

			check := func(who, ae string, r *celeristest.ResponseRecorder) {
				enc := r.Header("content-encoding")
				got := string(r.Body)
				if enc == "gzip" {
					got = string(decompressGzip(t, r.Body))
				}
				wantEnc := ""
				if ae == "gzip" {
					wantEnc = "gzip"
				}
				if r.StatusCode != 200 || enc != wantEnc || got != body {
					t.Errorf("%s (Accept-Encoding %q): %d content-encoding %q, %d body bytes (x-singleflight %q), want 200 content-encoding %q and the body",
						who, ae, r.StatusCode, enc, len(r.Body), r.Header("x-singleflight"), wantEnc)
				}
			}
			check("leader", "gzip", leader)
			check("second request", tc.secondAE, second)
			if strings.HasPrefix(tc.name, "control/") && second.Header("x-singleflight") != "HIT" {
				t.Errorf("control: the second request was not coalesced (x-singleflight %q, handler runs %d)", second.Header("x-singleflight"), calls.Load())
			}
			t.Logf("%s: second x-singleflight=%q handler runs=%d", tc.name, second.Header("x-singleflight"), calls.Load())
		})
	}
}
