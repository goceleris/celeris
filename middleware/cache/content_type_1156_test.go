package cache

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/store"
)

// celeris#1156: the content type a handler writes with c.String, c.JSON or
// c.Blob is kept by the response buffer, not among c.ResponseHeaders(), which
// is all the cache stored: a HIT replayed application/octet-stream.

func TestCacheHitReplaysTheContentTypeOfTheMiss1156(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler celeris.HandlerFunc
		status  int
		body    string
		ct      string // the Content-Type of the MISS, and so of the HIT
	}{
		{"string", func(c *celeris.Context) error { return c.String(200, "hello") }, 200, "hello", "text/plain"},
		{"json", func(c *celeris.Context) error { return c.JSON(200, map[string]string{"a": "b"}) }, 200, `{"a":"b"}`, "application/json"},
		{"html", func(c *celeris.Context) error { return c.HTML(200, "<p>hi</p>") }, 200, "<p>hi</p>", "text/html; charset=utf-8"},
		{"blob-custom-type", func(c *celeris.Context) error { return c.Blob(200, "image/png", []byte("PNG")) }, 200, "PNG", "image/png"},
		{"blob-type-with-parameter", func(c *celeris.Context) error {
			return c.Blob(200, "text/csv; charset=utf-8; header=present", []byte("a,b"))
		}, 200, "a,b", "text/csv; charset=utf-8; header=present"},
		{"blob-octet-stream-on-purpose", func(c *celeris.Context) error {
			return c.Blob(200, "application/octet-stream", []byte("raw"))
		}, 200, "raw", "application/octet-stream"},
		// A response with no body has no type, and a HIT must not give it one.
		{"nocontent", func(c *celeris.Context) error { return c.NoContent(204) }, 204, "", ""},
	} {
		for _, sf := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/singleflight=%v", tc.name, sf), func(t *testing.T) {
				kv := store.NewMemoryKV()
				defer kv.Close()
				var runs atomic.Int32
				h := func(c *celeris.Context) error { runs.Add(1); return tc.handler(c) }
				mw := New(Config{Store: kv, TTL: time.Minute, DisableSingleflight: !sf})

				miss := runOnce(t, mw, h, "GET", "/x")
				hit := runOnce(t, mw, h, "GET", "/x")
				if miss.Header("x-cache") != "MISS" || hit.Header("x-cache") != "HIT" || runs.Load() != 1 {
					t.Fatalf("x-cache %q then %q, handler ran %d times, want MISS then HIT, once",
						miss.Header("x-cache"), hit.Header("x-cache"), runs.Load())
				}
				for _, r := range []struct {
					name string
					rec  interface {
						Header(string) string
						BodyString() string
					}
					status int
				}{{"MISS", miss, miss.StatusCode}, {"HIT", hit, hit.StatusCode}} {
					if r.status != tc.status || r.rec.BodyString() != tc.body {
						t.Errorf("%s: %d %q, want %d %q", r.name, r.status, r.rec.BodyString(), tc.status, tc.body)
					}
					if got := r.rec.Header("content-type"); got != tc.ct {
						t.Errorf("%s: content-type %q, want %q", r.name, got, tc.ct)
					}
				}
			})
		}
	}
}

// The content type is part of the stored representation: a whitelist of
// response headers does not drop it, and a response with a body of its own
// type stays that type on every later HIT.
func TestCacheKeepsTheContentTypeWithIncludeHeaders1156(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	h := func(c *celeris.Context) error {
		c.SetHeader("etag", `"v1"`)
		c.SetHeader("x-other", "dropped")
		return c.JSON(200, map[string]int{"n": 1})
	}
	mw := New(Config{Store: kv, TTL: time.Minute, IncludeHeaders: []string{"etag"}})

	_ = runOnce(t, mw, h, "GET", "/x")
	for i := range 3 {
		hit := runOnce(t, mw, h, "GET", "/x")
		if hit.Header("x-cache") != "HIT" {
			t.Fatalf("request %d: x-cache %q, want HIT", i, hit.Header("x-cache"))
		}
		if got := hit.Header("content-type"); got != "application/json" {
			t.Errorf("HIT %d: content-type %q, want application/json", i, got)
		}
		if hit.Header("etag") != `"v1"` || hit.Header("x-other") != "" {
			t.Errorf("HIT %d: etag %q x-other %q, want the whitelisted header only", i, hit.Header("etag"), hit.Header("x-other"))
		}
	}
}

// ExcludeHeaders naming content-type is honoured: nothing is stored for it, so
// the HIT is replayed as a response written with no type (the recorder cannot
// tell an absent content-type header from an empty one).
func TestCacheExcludeHeadersCanDropTheContentType1156(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	h := func(c *celeris.Context) error { return c.String(200, "hello") }
	mw := New(Config{Store: kv, TTL: time.Minute, ExcludeHeaders: []string{"Content-Type"}})

	_ = runOnce(t, mw, h, "GET", "/x")
	hit := runOnce(t, mw, h, "GET", "/x")
	if hit.Header("x-cache") != "HIT" || hit.BodyString() != "hello" {
		t.Fatalf("HIT: x-cache %q body %q", hit.Header("x-cache"), hit.BodyString())
	}
	if got := hit.Header("content-type"); got != "" {
		t.Errorf("HIT: content-type %q, want none (excluded)", got)
	}
}

// An entry written before the content type was stored (by an earlier version,
// in a store that outlives a deploy) has none: it replays as it is stored, with no invented type.
func TestCacheReplaysAnEntryWithoutAContentType1156(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	ctx := t.Context()
	enc := store.EncodedResponse{Status: 200, Headers: [][2]string{{"etag", `"v1"`}}, Body: []byte("old")}.Encode()
	mw := New(Config{Store: kv, TTL: time.Minute, KeyGenerator: func(*celeris.Context) string { return "k" }})
	if err := kv.Set(ctx, "k", enc, time.Minute); err != nil {
		t.Fatal(err)
	}
	h := func(c *celeris.Context) error { return c.String(200, "from the handler") }
	hit := runOnce(t, mw, h, "GET", "/x")
	if hit.Header("x-cache") != "HIT" || hit.BodyString() != "old" || hit.Header("etag") != `"v1"` {
		t.Fatalf("HIT: x-cache %q body %q etag %q", hit.Header("x-cache"), hit.BodyString(), hit.Header("etag"))
	}
	if got := hit.Header("content-type"); got != "" {
		t.Errorf("HIT: content-type %q, want none (the entry stores none)", got)
	}
}

// A response written with an empty type is replayed with an empty type, not as
// octet-stream, and its body is kept. Only the HIT is checked here: the MISS of
// a buffered Blob with an empty type lost its body in FlushResponse
// (celeris#1155), which is not the cache's to fix.
func TestCacheHitOfABlobWithAnEmptyContentType1156(t *testing.T) {
	for _, sf := range []bool{false, true} {
		t.Run(fmt.Sprintf("singleflight=%v", sf), func(t *testing.T) {
			kv := store.NewMemoryKV()
			defer kv.Close()
			h := func(c *celeris.Context) error { return c.Blob(200, "", []byte("raw")) }
			mw := New(Config{Store: kv, TTL: time.Minute, DisableSingleflight: !sf})

			_ = runOnce(t, mw, h, "GET", "/x")
			hit := runOnce(t, mw, h, "GET", "/x")
			if hit.Header("x-cache") != "HIT" || hit.StatusCode != 200 || hit.BodyString() != "raw" {
				t.Fatalf("HIT: x-cache %q, %d %q, want HIT 200 \"raw\"", hit.Header("x-cache"), hit.StatusCode, hit.BodyString())
			}
			if got := hit.Header("content-type"); got != "" {
				t.Errorf("HIT: content-type %q, want none, as the handler wrote it", got)
			}
		})
	}
}
