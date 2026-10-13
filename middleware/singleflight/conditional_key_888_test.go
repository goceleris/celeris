package singleflight

import (
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

// celeris#888 item 23, review round 4. A conditional write is never coalesced,
// so the key built for it is dropped at once: defaultKey must tell it from the
// preconditions (which it reads anyway) before it sorts the query and builds
// the key, and cost neither for a request that bypasses.

// TestDefaultKeyOfAConditionalWriteBuildsNothing888: no allocation for a
// conditional PUT, POST, PATCH or DELETE (a long query, cookies and
// Accept-Encoding would otherwise be sorted, canonicalised and copied), and
// the same request as a GET or HEAD, or the write without the precondition,
// still gets its whole key.
func TestDefaultKeyOfAConditionalWriteBuildsNothing888(t *testing.T) {
	const target = "/doc?z=26&y=25&x=24&b=2&a=1"
	for _, v := range validators888 {
		for _, method := range []string{"PUT", "POST", "PATCH", "DELETE"} {
			t.Run(method+"/"+v.name, func(t *testing.T) {
				c, _ := celeristest.NewContext(method, target,
					celeristest.WithHeader(v.name, v.a), celeristest.WithHeader("accept-encoding", "gzip, deflate, br"), celeristest.WithHeader("cookie", "s=1"))
				defer celeristest.ReleaseContext(c)
				if _, conditional := defaultKey(c); !conditional {
					t.Fatalf("%s with %s: not told to be conditional", method, v.name)
				}
				if allocs := testing.AllocsPerRun(50, func() { _, _ = defaultKey(c) }); allocs != 0 {
					t.Errorf("%s with %s: defaultKey allocates %.0f times, want 0: a request that bypasses builds no key", method, v.name, allocs)
				}
			})
		}
		for _, method := range []string{"GET", "HEAD"} {
			t.Run(method+"/"+v.name+"/keeps its key", func(t *testing.T) {
				c, _ := celeristest.NewContext(method, target, celeristest.WithHeader(v.name, v.a))
				defer celeristest.ReleaseContext(c)
				key, conditional := defaultKey(c)
				if !conditional || key == "" || key == method+"\x00/doc" {
					t.Errorf("%s with %s: key %q, conditional %v, want the full key of a conditional read", method, v.name, key, conditional)
				}
			})
		}
	}
	c, _ := celeristest.NewContext("PUT", target)
	defer celeristest.ReleaseContext(c)
	if key, conditional := defaultKey(c); conditional || key == "" {
		t.Errorf("an unconditional PUT: key %q, conditional %v, want a key and not conditional", key, conditional)
	}
}
