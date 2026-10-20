package singleflight

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/internal/negotiate"
)

// celeris#888 items 23 and 24, review round 1. The canonical Accept-Encoding
// must fold two values only when the negotiator (c.AcceptsEncodings, which
// compress uses) reads them the same, or a waiter is served the encoding it
// refused; and the query component of the default key must not read as a
// labelled header component.

// runPairOpts is runPair with request options per request, so the two
// requests can differ in their query too. Both are GETs of the given paths;
// h is the chain's last handler and its first run is held.
func runPairOpts(t *testing.T, mw celeris.HandlerFunc, h celeris.HandlerFunc, pathA string, a []celeristest.Option, pathB string, b []celeristest.Option) pairResult {
	t.Helper()
	entered, gate := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	held := func(c *celeris.Context) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-gate
		}
		return h(c)
	}
	joined := make(chan struct{}, 1)
	prev := testHookWaiterJoined
	testHookWaiterJoined = func() {
		select {
		case joined <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { testHookWaiterJoined = prev })
	var ctxs [2]*celeris.Context
	do := func(i int, path string, o []celeristest.Option) *celeristest.ResponseRecorder {
		o = append(append([]celeristest.Option{}, o...), celeristest.WithHandlers(mw, held))
		c, rec := celeristest.NewContext("GET", path, o...)
		ctxs[i] = c
		_ = c.Next()
		return rec
	}
	var res pairResult
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); res.leader = do(0, pathA, a) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader's handler did not start")
	}
	done := make(chan struct{})
	wg.Add(1)
	go func() { defer wg.Done(); defer close(done); res.second = do(1, pathB, b) }()
	select {
	case <-joined:
		res.joined = true
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the second request neither joined the leader nor finished")
	}
	close(gate)
	wg.Wait()
	t.Cleanup(func() {
		for _, c := range ctxs {
			celeristest.ReleaseContext(c)
		}
	})
	res.calls = calls.Load()
	return res
}

// negotiatingReal negotiates exactly as compress does (c.AcceptsEncodings,
// internal/negotiate), unlike negotiating, which looks for the token.
func negotiatingReal(c *celeris.Context) error {
	if c.AcceptsEncodings("gzip") == "gzip" {
		c.SetHeader("content-encoding", "gzip")
		return c.Blob(200, "text/plain", []byte("GZIP-BODY"))
	}
	return c.Blob(200, "text/plain", []byte("identity-body"))
}

func aeOpt(v string) []celeristest.Option {
	return []celeristest.Option{celeristest.WithHeader("accept-encoding", v)}
}

// refusals: the waiter's value refuses gzip (or names no coding), the
// leader's reads as accepting it, the way the negotiator parses it. They are
// not one request, so the waiter must get the identity body from its own run.
var aeNotTheSame888 = []struct{ name, leader, waiter string }{
	{"blank-around-equals", "gzip;q = 0", "gzip;q=0"},
	{"blank-after-equals", "gzip;q= 0", "gzip;q=0"},
	{"blank-inside-token", "gzip", "g zip"},
	{"token-then-blank-token", "g zip", "gzip"},
}

// TestCanonicalAcceptEncodingKeepsApartWhatTheNegotiatorSeparates888: with
// the default key and with a custom key whose only guard is the Vary
// comparison, no waiter is served an encoding it refused.
func TestCanonicalAcceptEncodingKeepsApartWhatTheNegotiatorSeparates888(t *testing.T) {
	pathOnly := func(c *celeris.Context) string { return c.Method() + "\x00" + c.Path() }
	for _, tc := range aeNotTheSame888 {
		for _, custom := range []bool{false, true} {
			name, mw := tc.name+"/default-key", New()
			if custom {
				name, mw = tc.name+"/custom-key", New(Config{KeyFunc: pathOnly})
			}
			t.Run(name, func(t *testing.T) {
				r := runPairOpts(t, mw, negotiatingReal, "/doc", aeOpt(tc.leader), "/doc", aeOpt(tc.waiter))
				// Under the default key the keys differ, so the waiter never
				// joins; under the custom key it joins and the Vary
				// comparison sends it to its own run. Either way two runs.
				if (r.joined && !custom) || r.calls != 2 || r.second.Header("x-singleflight") == "HIT" {
					t.Errorf("leader %q, waiter %q: joined=%v handler runs %d x-singleflight %q, want its own run (2 runs, no HIT)",
						tc.leader, tc.waiter, r.joined, r.calls, r.second.Header("x-singleflight"))
				}
				// What the waiter's own run gives: the negotiator's answer to its own value.
				want := "identity-body"
				if negotiate.Accept(tc.waiter, []string{"gzip"}) == "gzip" {
					want = "GZIP-BODY"
				}
				if string(r.second.Body) != want {
					t.Errorf("waiter %q got %q (content-encoding %q), want its own answer %q", tc.waiter, r.second.Body, r.second.Header("content-encoding"), want)
				}
			})
		}
	}
}

// TestCanonicalAcceptEncodingFoldsTheCaseOfTheWeightName888: the negotiator
// reads "Q=0" as q=0 (RFC 9110 12.4.2), as "q=0", so both requests are refused
// gzip and are one request: they coalesce, and the waiter gets what its own
// run would have given.
func TestCanonicalAcceptEncodingFoldsTheCaseOfTheWeightName888(t *testing.T) {
	r := runPairOpts(t, New(), negotiatingReal, "/doc", aeOpt("gzip;Q=0"), "/doc", aeOpt("gzip;q=0"))
	if !r.joined || r.calls != 1 || string(r.second.Body) != "identity-body" {
		t.Errorf("gzip;Q=0 then gzip;q=0: joined=%v runs=%d waiter body %q, want one run shared (identity-body)", r.joined, r.calls, r.second.Body)
	}
}

// TestCanonicalAcceptEncodingIsTheNegotiatorsReading888: two values are one
// key component exactly when the negotiator parses them to the same list.
func TestCanonicalAcceptEncodingIsTheNegotiatorsReading888(t *testing.T) {
	values := []string{
		"", ",", " , ", "gzip", "GZIP", " gzip ", "gzip,", ",gzip", "gzip;q=1", "gzip;q=1.0", "gzip;q=0.8", "gzip; q=0.8", "gzip;q=0.80",
		"gzip;q=0", "gzip;q=0.0", "gzip;Q=0", "gzip;q = 0", "gzip;q= 0", "gzip;q =0", "gzip;foo=1;q=0.5", "gzip;foo=1",
		"gzip, br", "gzip,br", "br, gzip", "gzip, br;q=0", "g zip", "gz ip", "gzip br", "gzipbr", "*", "*;q=0", "identity", "identity;q=0",
		"gzip;q=nan", "gzip;q=x", "gzip;q=", "br", "gzip, deflate, br, zstd", "gzip,deflate,br,zstd", "GZIP, Deflate, BR, ZSTD",
		"a,b,c,d,e,f,g,h,i,j,k,l,m,n", "A, B, C, D, E, F, G, H, I, J, K, L, M, N",
		// Weights that differ past the first decimal, or q=0 against a tiny
		// positive q, are different negotiations: a canonical form that
		// rounds the weight would give them one key.
		"gzip;q=0.001", "gzip;q=0.85", "gzip;q=0.8", "gzip;q=0.5, br;q=0.55", "gzip;q=0.5, br;q=0.5", "gzip;q=0.5, br;q=0.50",
		"gzip;q=0.999", "gzip;q=0.9999999", "gzip;q=1e-3", "gzip;q=.5", "gzip;q=0.5",
	}
	same := func(a, b []negotiate.AcceptItem) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i].MediaType != b[i].MediaType || (a[i].Quality != b[i].Quality && !(a[i].Quality != a[i].Quality && b[i].Quality != b[i].Quality)) {
				return false
			}
		}
		return true
	}
	for _, a := range values {
		for _, b := range values {
			canon := sameCanonical("accept-encoding", canonicalValue("accept-encoding", a), b)
			read := same(negotiate.ParseInto(nil, a), negotiate.ParseInto(nil, b))
			if canon != read {
				t.Errorf("Accept-Encoding %q and %q: canonically equal %v, but the negotiator reads them as equal: %v", a, b, canon, read)
			}
		}
	}
}

// TestDefaultKeyQueryIsLabelled888: the query component carries a label of
// its own, so a query string that looks like a labelled header component
// (?nm=abc and If-None-Match: abc, ?a=tok and Authorization: tok) is not the
// same key.
func TestDefaultKeyQueryIsLabelled888(t *testing.T) {
	for _, tc := range []struct{ header, param string }{
		{"if-none-match", "nm"}, {"if-modified-since", "ms"}, {"if-match", "ma"}, {"if-unmodified-since", "us"},
		{"authorization", "a"}, {"cookie", "c"}, {"accept-encoding", "e"},
	} {
		t.Run(tc.header, func(t *testing.T) {
			hc, _ := celeristest.NewContext("GET", "/doc", celeristest.WithHeader(tc.header, "abc"))
			qc, _ := celeristest.NewContext("GET", "/doc", celeristest.WithQuery(tc.param, "abc"))
			defer celeristest.ReleaseContext(hc)
			defer celeristest.ReleaseContext(qc)
			if hk, qk := defaultKeyFunc(hc), defaultKeyFunc(qc); hk == qk {
				t.Errorf("%s: abc and ?%s=abc give the same key %q", tc.header, tc.param, hk)
			}
		})
	}
}

// TestDefaultKeyQueryNeverReadsAsAHeader888: the query's label is its own for
// every header component, not only for a parameter named like a label (the
// previous test): a header whose value is the whole query string ("x=abc"
// against ?x=abc) reads as the query under any label the query shares with a
// header.
func TestDefaultKeyQueryNeverReadsAsAHeader888(t *testing.T) {
	for _, h := range []string{"authorization", "cookie", "accept-encoding", "if-none-match", "if-modified-since", "if-match", "if-unmodified-since"} {
		t.Run(h, func(t *testing.T) {
			hc, _ := celeristest.NewContext("GET", "/doc", celeristest.WithHeader(h, "x=abc"))
			qc, _ := celeristest.NewContext("GET", "/doc", celeristest.WithQuery("x", "abc"))
			defer celeristest.ReleaseContext(hc)
			defer celeristest.ReleaseContext(qc)
			if hk, qk := defaultKeyFunc(hc), defaultKeyFunc(qc); hk == qk {
				t.Errorf("%s: x=abc and ?x=abc give the same key %q", h, hk)
			}
		})
	}
}

// TestQueryNeverJoinsTheValidatorsLeader888: the end to end case. GET
// /doc?nm=abc must not take the response of GET /doc with If-None-Match: abc.
func TestQueryNeverJoinsTheValidatorsLeader888(t *testing.T) {
	h := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("q="+c.Query("nm")+" inm="+c.Header("if-none-match")))
	}
	r := runPairOpts(t, New(), h,
		"/doc", []celeristest.Option{celeristest.WithHeader("if-none-match", "abc")},
		"/doc", []celeristest.Option{celeristest.WithQuery("nm", "abc")})
	if r.joined || r.calls != 2 || !strings.HasPrefix(string(r.second.Body), "q=abc ") {
		t.Errorf("GET /doc?nm=abc: %q joined=%v runs %d, want its own response and 2 runs", r.second.Body, r.joined, r.calls)
	}
}
