package singleflight

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/etag"
)

// celeris#888 items 23 and 24 (from #939 review r1). Items: the default key
// holds the four validators, so identical revalidations coalesce and each
// request still gets the answer to its own validator; and it holds a
// canonical Accept-Encoding, so equivalent values coalesce.

var validators888 = []struct{ name, a, b string }{
	{"if-none-match", `"v1"`, `"v2"`},
	{"if-modified-since", "Mon, 05 Oct 2026 10:00:00 GMT", "Tue, 06 Oct 2026 10:00:00 GMT"},
	{"if-match", `"v1"`, `"v2"`},
	{"if-unmodified-since", "Mon, 05 Oct 2026 10:00:00 GMT", "Tue, 06 Oct 2026 10:00:00 GMT"},
}

// TestDefaultKeyCoalescesIdenticalValidators888: with the default key a
// conditional request joins a leader that sent the same validator and takes
// its response; one that sends another validator, or none, does not.
func TestDefaultKeyCoalescesIdenticalValidators888(t *testing.T) {
	for _, v := range validators888 {
		hdr := func(val string) [][2]string { return [][2]string{{v.name, val}} }
		t.Run(v.name+"/identical", func(t *testing.T) {
			r := runPair(t, New(), []celeris.HandlerFunc{plain}, hdr(v.a), hdr(v.a))
			if !r.joined || r.second.Header("x-singleflight") != "HIT" || r.calls != 1 {
				t.Errorf("identical %s: joined=%v x-singleflight %q handler runs %d, want joined, HIT and 1 run",
					v.name, r.joined, r.second.Header("x-singleflight"), r.calls)
			}
			if r.second.StatusCode != 200 || string(r.second.Body) != reprBody {
				t.Errorf("second request: %d %q, want the leader's 200 %q", r.second.StatusCode, r.second.Body, reprBody)
			}
		})
		t.Run(v.name+"/different", func(t *testing.T) {
			r := runPair(t, New(), []celeris.HandlerFunc{plain}, hdr(v.a), hdr(v.b))
			if r.joined || r.calls != 2 {
				t.Errorf("different %s: joined=%v handler runs %d, want not joined and 2 runs", v.name, r.joined, r.calls)
			}
		})
		t.Run(v.name+"/validator-then-none", func(t *testing.T) {
			r := runPair(t, New(), []celeris.HandlerFunc{plain}, hdr(v.a), nil)
			if r.joined || r.calls != 2 {
				t.Errorf("%s then none: joined=%v handler runs %d, want not joined and 2 runs", v.name, r.joined, r.calls)
			}
		})
	}
}

// TestIdenticalRevalidationsShareTheLeadersAnswer888: the case the item is
// about. A CDN or browsers send the same If-None-Match for a hot resource:
// one handler run answers all of them with the 304, and a request with
// another tag gets the full representation from its own run.
func TestIdenticalRevalidationsShareTheLeadersAnswer888(t *testing.T) {
	tag := etagOf(t)
	handlers := []celeris.HandlerFunc{etag.New(), plain}
	inm := func(v string) [][2]string { return [][2]string{{"if-none-match", v}} }
	same := runPair(t, New(), handlers, inm(tag), inm(tag))
	if same.leader.StatusCode != 304 || same.second.StatusCode != 304 || !same.joined || same.calls != 1 {
		t.Errorf("identical If-None-Match: leader %d, second %d, joined=%v, handler runs %d, want 304, 304, joined, 1",
			same.leader.StatusCode, same.second.StatusCode, same.joined, same.calls)
	}
	other := runPair(t, New(), handlers, inm(tag), inm(`"other"`))
	if other.leader.StatusCode != 304 || other.second.StatusCode != 200 || string(other.second.Body) != reprBody || other.joined || other.calls != 2 {
		t.Errorf("another tag: leader %d, second %d %q, joined=%v, handler runs %d, want 304, 200 %q, not joined, 2",
			other.leader.StatusCode, other.second.StatusCode, other.second.Body, other.joined, other.calls, reprBody)
	}
}

// TestCustomKeyStillBypassesConditionalRequests888: a custom KeyFunc cannot
// be assumed to hold the validators, so a conditional request is still not
// coalesced, whatever the key says (the control for the default key's change).
func TestCustomKeyStillBypassesConditionalRequests888(t *testing.T) {
	pathOnly := func(c *celeris.Context) string { return c.Method() + "\x00" + c.Path() }
	for _, v := range validators888 {
		t.Run(v.name, func(t *testing.T) {
			hdr := [][2]string{{v.name, v.a}}
			r := runPair(t, New(Config{KeyFunc: pathOnly}), []celeris.HandlerFunc{plain}, hdr, hdr)
			if r.joined || r.calls != 2 {
				t.Errorf("custom key, identical %s: joined=%v handler runs %d, want not joined and 2 runs", v.name, r.joined, r.calls)
			}
		})
	}
}

// TestRangeIsNeverCoalesced888: the Range bypass stays, key or not.
func TestRangeIsNeverCoalesced888(t *testing.T) {
	rng := [][2]string{{"range", "bytes=0-3"}}
	pathOnly := func(c *celeris.Context) string { return c.Method() + "\x00" + c.Path() }
	for name, mw := range map[string]celeris.HandlerFunc{"default-key": New(), "custom-key": New(Config{KeyFunc: pathOnly})} {
		r := runPair(t, mw, []celeris.HandlerFunc{plain}, rng, rng)
		if r.joined || r.calls != 2 {
			t.Errorf("%s, identical Range: joined=%v handler runs %d, want not joined and 2 runs", name, r.joined, r.calls)
		}
	}
}

// TestAcceptEncodingValuesCoalesceWhenEquivalent888: Accept-Encoding values
// that differ only in case or in blanks name the same encodings, so the
// default key coalesces them (and a waiter whose value is equivalent to the
// Vary-ed value of a custom-key leader takes its response); a different
// order, or different encodings, stay apart.
func TestAcceptEncodingValuesCoalesceWhenEquivalent888(t *testing.T) {
	pathOnly := func(c *celeris.Context) string { return c.Method() + "\x00" + c.Path() }
	ae := func(v string) [][2]string { return [][2]string{{"accept-encoding", v}} }
	gzipped := "200 gzip:" + reprBody + " encoding=\"gzip\""
	identity := "200 " + reprBody + " encoding=\"\""
	cases := []struct {
		name         string
		mw           celeris.HandlerFunc
		leader, next string
		shared       bool   // the second request takes the leader's response
		secondWant   string // what the second request answers
	}{
		{"default-key/blanks", New(), "gzip, deflate, br", "gzip,deflate,br", true, gzipped},
		{"default-key/case", New(), "gzip", "GZIP", true, gzipped},
		{"default-key/q-blanks", New(), "gzip;q=0.8, br", "gzip; q=0.8,br", true, gzipped},
		{"default-key/tab", New(), "gzip,\tbr", "gzip, br", true, gzipped},
		{"default-key/identity-spellings", New(), "br", " br ", true, identity},
		{"default-key/order-kept", New(), "gzip, br", "br, gzip", false, gzipped},
		{"default-key/other-encodings", New(), "gzip, br", "gzip", false, gzipped},
		{"custom-key-vary/blanks", New(Config{KeyFunc: pathOnly}), "gzip, deflate", "gzip,deflate", true, gzipped},
		{"custom-key-vary/case", New(Config{KeyFunc: pathOnly}), "gzip", "GZIP", true, gzipped},
		{"custom-key-vary/other-encodings", New(Config{KeyFunc: pathOnly}), "gzip", "br", false, identity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runPair(t, tc.mw, []celeris.HandlerFunc{negotiating}, ae(tc.leader), ae(tc.next))
			got := strings.Join([]string{itoa(r.second.StatusCode), string(r.second.Body), "encoding=\"" + r.second.Header("content-encoding") + "\""}, " ")
			if got != tc.secondWant {
				t.Errorf("second request: %s, want %s", got, tc.secondWant)
			}
			shared := r.second.Header("x-singleflight") == "HIT"
			wantCalls := int32(2)
			if tc.shared {
				wantCalls = 1
			}
			if shared != tc.shared || r.calls != wantCalls {
				t.Errorf("second request shared the leader's response: %v with %d handler runs (joined=%v), want %v with %d",
					shared, r.calls, r.joined, tc.shared, wantCalls)
			}
		})
	}
}

// TestDefaultKeyValidatorComponentsDistinct888: the validators are labelled
// key components, so none reads as another or as Authorization, Cookie or
// Accept-Encoding, and a request without them keeps the plain key.
func TestDefaultKeyValidatorComponentsDistinct888(t *testing.T) {
	keys := map[string]string{}
	add := func(name, key string) {
		t.Helper()
		if prev, ok := keys[key]; ok {
			t.Errorf("%s and %s give the same key %q", prev, name, key)
		}
		keys[key] = name
	}
	for _, h := range []string{"", "authorization", "cookie", "accept-encoding", "if-none-match", "if-modified-since", "if-match", "if-unmodified-since"} {
		var opts []celeristest.Option
		if h != "" {
			opts = append(opts, celeristest.WithHeader(h, "x"))
		}
		c, _ := celeristest.NewContext("GET", "/doc", opts...)
		add("header "+h, defaultKeyFunc(c))
		celeristest.ReleaseContext(c)
	}
	c, _ := celeristest.NewContext("GET", "/doc")
	if k := defaultKeyFunc(c); k != "GET\x00/doc" {
		t.Errorf("a request with none of them: key %q, want the plain \"GET\\x00/doc\"", k)
	}
	celeristest.ReleaseContext(c)
}

// TestDefaultKeyAllocatesOnce888: the canonical Accept-Encoding and the four
// validators are written into the key's one buffer: a browser's request (a
// blank-separated Accept-Encoding, a validator) still costs one allocation.
func TestDefaultKeyAllocatesOnce888(t *testing.T) {
	c, _ := celeristest.NewContext("GET", "/doc",
		celeristest.WithHeader("accept-encoding", "gzip, deflate, br, zstd"),
		celeristest.WithHeader("if-none-match", `"v1"`),
		celeristest.WithHeader("if-modified-since", "Mon, 05 Oct 2026 10:00:00 GMT"))
	defer celeristest.ReleaseContext(c)
	if k := defaultKeyFunc(c); !strings.Contains(k, "\x00e=gzip,deflate,br,zstd") || !strings.Contains(k, "\x00nm=\"v1\"") {
		t.Fatalf("key %q does not hold the canonical encodings and the validator", k)
	}
	if allocs := testing.AllocsPerRun(1000, func() { _ = defaultKeyFunc(c) }); allocs > 1 {
		t.Errorf("defaultKeyFunc made %.1f allocations per call, want 1", allocs)
	}
}
