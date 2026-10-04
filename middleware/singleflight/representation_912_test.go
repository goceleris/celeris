package singleflight

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/etag"
)

// pairResult is what runPair saw: both responses, whether the second request
// joined the leader, and how many times the handler ran.
type pairResult struct {
	leader, second *celeristest.ResponseRecorder
	joined         bool
	calls          int32
}

// runPair sends a leader request and, while the leader's handler is held, a
// second request with the same method and path. The leader's handler blocks
// until the second request has either joined it (testHookWaiterJoined) or
// finished on its own, so the interleaving is the same on every run.
// handlers are the chain after the singleflight middleware mw; the last one
// is wrapped so that its first run holds.
func runPair(t *testing.T, mw celeris.HandlerFunc, handlers []celeris.HandlerFunc, leaderHdrs, secondHdrs [][2]string) pairResult {
	t.Helper()
	entered, gate := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	last := handlers[len(handlers)-1]
	held := func(c *celeris.Context) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-gate
		}
		return last(c)
	}
	chain := append([]celeris.HandlerFunc{mw}, handlers[:len(handlers)-1]...)
	chain = append(chain, held)

	joined := make(chan struct{}, 1)
	prev := testHookWaiterJoined
	testHookWaiterJoined = func() {
		select {
		case joined <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { testHookWaiterJoined = prev })

	do := func(hdrs [][2]string) (*celeristest.ResponseRecorder, *celeris.Context) {
		opts := []celeristest.Option{celeristest.WithHandlers(chain...)}
		for _, h := range hdrs {
			opts = append(opts, celeristest.WithHeader(h[0], h[1]))
		}
		c, rec := celeristest.NewContext("GET", "/doc", opts...)
		_ = c.Next()
		return rec, c
	}
	var res pairResult
	var ctxs [2]*celeris.Context
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); res.leader, ctxs[0] = do(leaderHdrs) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader's handler did not start")
	}
	secondDone := make(chan struct{})
	wg.Add(1)
	go func() { defer wg.Done(); defer close(secondDone); res.second, ctxs[1] = do(secondHdrs) }()
	select {
	case <-joined:
		res.joined = true
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the second request neither joined the leader nor finished")
	}
	close(gate)
	wg.Wait()
	// The recorders belong to the contexts: release them after the test
	// has read them.
	t.Cleanup(func() {
		for _, c := range ctxs {
			celeristest.ReleaseContext(c)
		}
	})
	res.calls = calls.Load()
	return res
}

const reprBody = "the full representation of /doc"

// negotiating answers with a representation chosen by Accept-Encoding, as
// compress does: "gzip:"-prefixed with Content-Encoding: gzip when the
// request accepts gzip, the identity body otherwise, and Vary:
// Accept-Encoding on both.
func negotiating(c *celeris.Context) error {
	c.AddHeader("vary", "Accept-Encoding")
	if strings.Contains(c.Header("accept-encoding"), "gzip") {
		c.SetHeader("content-encoding", "gzip")
		return c.Blob(200, "text/plain", []byte("gzip:"+reprBody))
	}
	return c.Blob(200, "text/plain", []byte(reprBody))
}

func plain(c *celeris.Context) error {
	return c.Blob(200, "text/plain", []byte(reprBody))
}

// etagOf returns the tag etag.New() gives reprBody.
func etagOf(t *testing.T) string {
	t.Helper()
	c, rec := celeristest.NewContext("GET", "/doc", celeristest.WithHandlers(etag.New(), plain))
	if err := c.Next(); err != nil {
		t.Fatal(err)
	}
	tag := rec.Header("etag")
	celeristest.ReleaseContext(c)
	if tag == "" {
		t.Fatal("etag.New() set no ETag")
	}
	return tag
}

// TestSingleflightConditionalRequestsAnswerThemselves912 is celeris#912's
// conditional half. A conditional request's response depends on its
// validator, which the default key does not include: a leader whose
// If-None-Match matched got a 304 with no body, and an unconditional request
// that joined it got the same empty 304. A conditional request is not
// coalesced, so no leader is conditional and no conditional request waits on
// an unconditional one.
func TestSingleflightConditionalRequestsAnswerThemselves912(t *testing.T) {
	tag := etagOf(t)
	lastModified := "Mon, 05 Oct 2026 10:00:00 GMT"
	// ifModifiedSince is a handler that evaluates If-Modified-Since itself.
	ifModifiedSince := func(c *celeris.Context) error {
		c.SetHeader("last-modified", lastModified)
		if c.Header("if-modified-since") == lastModified {
			return c.NoContent(304)
		}
		return plain(c)
	}
	// ifMatch is a handler that evaluates If-Match itself.
	ifMatch := func(c *celeris.Context) error {
		if v := c.Header("if-match"); v != "" && v != `"v1"` {
			return c.NoContent(412)
		}
		return plain(c)
	}
	// ifUnmodifiedSince is a handler that evaluates If-Unmodified-Since itself.
	ifUnmodifiedSince := func(c *celeris.Context) error {
		if v := c.Header("if-unmodified-since"); v != "" && v != lastModified {
			return c.NoContent(412)
		}
		return plain(c)
	}
	type want struct {
		status int
		body   string
	}
	full := want{200, reprBody}
	cases := []struct {
		name                   string
		handlers               []celeris.HandlerFunc
		leaderHdr, secondHdr   [][2]string
		leaderWant, secondWant want
	}{
		{"if-none-match-leader-then-unconditional", []celeris.HandlerFunc{etag.New(), plain},
			[][2]string{{"if-none-match", tag}}, nil, want{304, ""}, full},
		{"unconditional-leader-then-if-none-match", []celeris.HandlerFunc{etag.New(), plain},
			nil, [][2]string{{"if-none-match", tag}}, full, want{304, ""}},
		{"if-none-match-leader-then-other-tag", []celeris.HandlerFunc{etag.New(), plain},
			[][2]string{{"if-none-match", tag}}, [][2]string{{"if-none-match", `"other"`}}, want{304, ""}, full},
		{"if-modified-since-leader-then-unconditional", []celeris.HandlerFunc{ifModifiedSince},
			[][2]string{{"if-modified-since", lastModified}}, nil, want{304, ""}, full},
		{"if-match-failing-leader-then-unconditional", []celeris.HandlerFunc{ifMatch},
			[][2]string{{"if-match", `"v0"`}}, nil, want{412, ""}, full},
		{"if-unmodified-since-failing-leader-then-unconditional", []celeris.HandlerFunc{ifUnmodifiedSince},
			[][2]string{{"if-unmodified-since", "Sun, 04 Oct 2026 10:00:00 GMT"}}, nil, want{412, ""}, full},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runPair(t, New(), tc.handlers, tc.leaderHdr, tc.secondHdr)
			check := func(who string, w want, rec *celeristest.ResponseRecorder) {
				if rec.StatusCode != w.status || string(rec.Body) != w.body {
					t.Errorf("%s: %d %q x-singleflight %q (joined the leader: %v), want %d %q: the answer to its own request",
						who, rec.StatusCode, rec.Body, rec.Header("x-singleflight"), r.joined, w.status, w.body)
				}
			}
			check("leader", tc.leaderWant, r.leader)
			check("second request", tc.secondWant, r.second)
			if r.joined {
				t.Errorf("the second request joined the leader; a conditional request must not be coalesced")
			}
			t.Logf("%s: joined=%v handler runs=%d leader %d, second %d", tc.name, r.joined, r.calls, r.leader.StatusCode, r.second.StatusCode)
		})
	}
}

// TestSingleflightAcceptEncodingSelectsTheRepresentation912 is celeris#912's
// Accept-Encoding half. Behind a middleware that negotiates the encoding
// (compress), a waiter got its leader's encoded body whatever it accepted.
//
//   - With the default key, Accept-Encoding is part of the key, so requests
//     that accept different encodings are not coalesced.
//   - With a custom KeyFunc that leaves it out, a waiter whose request differs
//     from its leader's in a header the leader's response names in Vary runs
//     its own handler instead of taking the leader's response.
//
// The controls show both still coalesce requests that send the same value.
func TestSingleflightAcceptEncodingSelectsTheRepresentation912(t *testing.T) {
	pathOnly := func(c *celeris.Context) string { return c.Method() + "\x00" + c.Path() }
	varyStar := func(c *celeris.Context) error {
		c.SetHeader("vary", "*")
		return negotiating(c)
	}
	gzip := [][2]string{{"accept-encoding", "gzip"}}
	identity := "200 " + reprBody + " encoding=\"\""
	gzipped := "200 gzip:" + reprBody + " encoding=\"gzip\""
	cases := []struct {
		name                   string
		mw                     celeris.HandlerFunc
		h                      celeris.HandlerFunc
		leaderHdr, secondHdr   [][2]string
		leaderWant, secondWant string
		wantJoined, wantHIT    bool
		wantCalls              int32
	}{
		{"default-key/gzip-leader-then-identity", New(), negotiating, gzip, nil, gzipped, identity, false, false, 2},
		{"default-key/identity-leader-then-gzip", New(), negotiating, nil, gzip, identity, gzipped, false, false, 2},
		{"default-key/gzip-leader-then-br", New(), negotiating, gzip, [][2]string{{"accept-encoding", "br"}}, gzipped, identity, false, false, 2},
		{"custom-key-vary/gzip-leader-then-identity", New(Config{KeyFunc: pathOnly}), negotiating, gzip, nil, gzipped, identity, true, false, 2},
		{"custom-key-vary/identity-leader-then-gzip", New(Config{KeyFunc: pathOnly}), negotiating, nil, gzip, identity, gzipped, true, false, 2},
		{"custom-key-vary-star/same-headers", New(Config{KeyFunc: pathOnly}), varyStar, gzip, gzip, gzipped, gzipped, true, false, 2},
		// Controls: the same Accept-Encoding still coalesces.
		{"control/default-key/same-gzip", New(), negotiating, gzip, gzip, gzipped, gzipped, true, true, 1},
		{"control/default-key/both-identity", New(), negotiating, nil, nil, identity, identity, true, true, 1},
		{"control/custom-key-vary/same-gzip", New(Config{KeyFunc: pathOnly}), negotiating, gzip, gzip, gzipped, gzipped, true, true, 1},
		// A response with no Vary is shared whatever the waiter sends.
		{"control/custom-key-no-vary/gzip-leader-then-identity", New(Config{KeyFunc: pathOnly}), plain, gzip, nil,
			"200 " + reprBody + " encoding=\"\"", "200 " + reprBody + " encoding=\"\"", true, true, 1},
	}
	got := func(rec *celeristest.ResponseRecorder) string {
		return strings.Join([]string{itoa(rec.StatusCode), string(rec.Body), "encoding=\"" + rec.Header("content-encoding") + "\""}, " ")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runPair(t, tc.mw, []celeris.HandlerFunc{tc.h}, tc.leaderHdr, tc.secondHdr)
			if g := got(r.leader); g != tc.leaderWant {
				t.Errorf("leader: %s, want %s", g, tc.leaderWant)
			}
			if g := got(r.second); g != tc.secondWant {
				t.Errorf("second request: %s (x-singleflight %q, joined the leader: %v), want %s",
					g, r.second.Header("x-singleflight"), r.joined, tc.secondWant)
			}
			if r.joined != tc.wantJoined {
				t.Errorf("second request joined the leader: %v, want %v", r.joined, tc.wantJoined)
			}
			if hit := r.second.Header("x-singleflight") == "HIT"; hit != tc.wantHIT {
				t.Errorf("second request x-singleflight HIT: %v, want %v", hit, tc.wantHIT)
			}
			if r.calls != tc.wantCalls {
				t.Errorf("handler ran %d times, want %d", r.calls, tc.wantCalls)
			}
			t.Logf("%s: joined=%v HIT=%q handler runs=%d", tc.name, r.joined, r.second.Header("x-singleflight"), r.calls)
		})
	}
}

// TestDefaultKeyHeaderComponentsDistinct912: the default key's header
// components are labelled, so one header's value never reads as another's,
// and Accept-Encoding is one of them.
func TestDefaultKeyHeaderComponentsDistinct912(t *testing.T) {
	keys := map[string]string{}
	for _, hdr := range [][2]string{{"", ""}, {"authorization", "x"}, {"cookie", "x"}, {"accept-encoding", "x"}} {
		var opts []celeristest.Option
		if hdr[0] != "" {
			opts = append(opts, celeristest.WithHeader(hdr[0], hdr[1]))
		}
		c, _ := celeristest.NewContext("GET", "/doc", opts...)
		k := defaultKeyFunc(c)
		celeristest.ReleaseContext(c)
		if prev, ok := keys[k]; ok {
			t.Errorf("%q and %q give the same key %q", prev, hdr[0], k)
		}
		keys[k] = hdr[0]
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
