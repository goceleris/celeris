package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/store"
)

func file832(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCacheDoesNotStorePartialContent832 is celeris#832 for the cache. A 206
// answers the request's Range, but the cache key (method, path, query) does
// not include it: a 206 the default 2xx filter let through was stored, and the
// next full GET got the part back as a HIT, for the whole TTL. A partial
// response is never stored; the full response still is, and a ranged request
// may then be answered with it (a server may ignore Range, RFC 9110 §14.2).
// Both the plain path and the singleflight leader's path are checked: they
// share the store decision.
func TestCacheDoesNotStorePartialContent832(t *testing.T) {
	for _, sf := range []bool{false, true} {
		t.Run(fmt.Sprintf("singleflight=%v", sf), func(t *testing.T) {
			kv := store.NewMemoryKV()
			defer kv.Close()
			p := file832(t)
			var runs atomic.Int32
			mw := New(Config{Store: kv, TTL: time.Minute, Singleflight: sf})
			h := func(c *celeris.Context) error {
				runs.Add(1)
				return c.File(p)
			}

			part := runOnce(t, mw, h, "GET", "/f", celeristest.WithHeader("range", "bytes=0-3"))
			if part.StatusCode != 206 || string(part.Body) != "0123" || part.Header("content-range") != "bytes 0-3/10" {
				t.Fatalf("ranged GET: %d %q content-range %q, want 206 \"0123\" bytes 0-3/10 (the handler must produce the 206 under test)",
					part.StatusCode, part.Body, part.Header("content-range"))
			}

			full := runOnce(t, mw, h, "GET", "/f")
			if full.StatusCode != 200 || string(full.Body) != "0123456789" || full.Header("content-range") != "" {
				t.Errorf("full GET after a ranged one: %d %q content-range %q x-cache %q, want 200 \"0123456789\" from the handler",
					full.StatusCode, full.Body, full.Header("content-range"), full.Header("x-cache"))
			}
			if got := full.Header("x-cache"); got != "MISS" {
				t.Errorf("full GET after a ranged one: x-cache %q, want MISS (the 206 must not have been stored)", got)
			}

			// The control: the full response is stored.
			again := runOnce(t, mw, h, "GET", "/f")
			if again.StatusCode != 200 || string(again.Body) != "0123456789" || again.Header("x-cache") != "HIT" {
				t.Errorf("second full GET: %d %q x-cache %q, want 200 \"0123456789\" HIT", again.StatusCode, again.Body, again.Header("x-cache"))
			}
			if n := runs.Load(); n != 2 {
				t.Errorf("handler ran %d times, want 2 (the ranged GET and the first full GET)", n)
			}
		})
	}
}

// TestCacheDoesNotStoreRangeNotSatisfiable832: a 416 answers the request's
// Range too. A StatusFilter that admits 4xx must not make the cache replay it
// to requests without that Range.
func TestCacheDoesNotStoreRangeNotSatisfiable832(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	p := file832(t)
	mw := New(Config{Store: kv, TTL: time.Minute, StatusFilter: func(status int) bool { return status < 500 }})
	h := func(c *celeris.Context) error { return c.File(p) }

	r416 := runOnce(t, mw, h, "GET", "/f", celeristest.WithHeader("range", "bytes=100-200"))
	if r416.StatusCode != 416 {
		t.Fatalf("unsatisfiable range: %d, want 416 (the handler must produce the 416 under test)", r416.StatusCode)
	}
	full := runOnce(t, mw, h, "GET", "/f")
	if full.StatusCode != 200 || string(full.Body) != "0123456789" || full.Header("x-cache") != "MISS" {
		t.Errorf("full GET after a 416: %d %q x-cache %q, want 200 \"0123456789\" MISS", full.StatusCode, full.Body, full.Header("x-cache"))
	}
	// The control: the filter does admit a 404 (so it is the 416 rule, not the
	// filter, that kept the 416 out).
	h404 := func(c *celeris.Context) error { return c.Blob(404, "text/plain", []byte("nf")) }
	_ = runOnce(t, mw, h404, "GET", "/missing")
	nf := runOnce(t, mw, h404, "GET", "/missing")
	if nf.StatusCode != 404 || nf.Header("x-cache") != "HIT" {
		t.Errorf("404 with a filter that admits it: %d x-cache %q, want 404 HIT", nf.StatusCode, nf.Header("x-cache"))
	}
}

// TestCacheDoesNotStorePartialContentKeyedOnRange832: a 206 is not stored
// even when the key does include Range (here in VaryHeaders). A stored part
// would be replayed without the handler's If-Range check: a request with the
// same Range whose If-Range no longer matches must get the whole
// representation (RFC 9110 §13.1.5), not the part.
func TestCacheDoesNotStorePartialContentKeyedOnRange832(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	p := file832(t)
	mw := New(Config{Store: kv, TTL: time.Minute, VaryHeaders: []string{"Range"}})
	h := func(c *celeris.Context) error { return c.File(p) }
	rng := celeristest.WithHeader("range", "bytes=0-3")

	part := runOnce(t, mw, h, "GET", "/f", rng)
	if part.StatusCode != 206 || string(part.Body) != "0123" {
		t.Fatalf("ranged GET: %d %q, want 206 \"0123\" (the handler must produce the 206 under test)", part.StatusCode, part.Body)
	}
	stale := runOnce(t, mw, h, "GET", "/f", rng, celeristest.WithHeader("if-range", `"stale"`))
	if stale.StatusCode != 200 || string(stale.Body) != "0123456789" || stale.Header("x-cache") != "MISS" {
		t.Errorf("same Range with an If-Range that does not match: %d %q x-cache %q, want 200 \"0123456789\" MISS from the handler",
			stale.StatusCode, stale.Body, stale.Header("x-cache"))
	}
}
