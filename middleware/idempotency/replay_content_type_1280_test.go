package idempotency

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/store"
)

// celeris#1280: the content type a handler writes with c.String, c.JSON or
// c.Blob is kept by the response buffer, not among c.ResponseHeaders(), which
// is all the middleware stored: a replay answered application/octet-stream, and
// a replayed 204 went out as an empty Blob. Same bug class as celeris#1156 (the
// cache), and the same design: the type is stored as a content-type header.

func idemKey(k string) celeristest.Option { return celeristest.WithHeader("idempotency-key", k) }

func TestReplayKeepsTheContentTypeOfTheFirstResponse1280(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler celeris.HandlerFunc
		status  int
		body    string
		ct      string // the Content-Type of the first response, and so of the replay
	}{
		{"string", func(c *celeris.Context) error { return c.String(201, "hello") }, 201, "hello", "text/plain"},
		{"json", func(c *celeris.Context) error { return c.JSON(201, map[string]string{"a": "b"}) }, 201, `{"a":"b"}`, "application/json"},
		{"html", func(c *celeris.Context) error { return c.HTML(200, "<p>hi</p>") }, 200, "<p>hi</p>", "text/html; charset=utf-8"},
		{"blob-custom-type", func(c *celeris.Context) error { return c.Blob(200, "image/png", []byte("PNG")) }, 200, "PNG", "image/png"},
		{"blob-type-with-parameter", func(c *celeris.Context) error {
			return c.Blob(200, "text/csv; charset=utf-8; header=present", []byte("a,b"))
		}, 200, "a,b", "text/csv; charset=utf-8; header=present"},
		{"blob-octet-stream-on-purpose", func(c *celeris.Context) error {
			return c.Blob(200, "application/octet-stream", []byte("raw"))
		}, 200, "raw", "application/octet-stream"},
		// A response with no body has no type, and a replay must not give it one.
		{"nocontent", func(c *celeris.Context) error { return c.NoContent(204) }, 204, "", ""},
	} {
		for _, bodyHash := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bodyhash=%v", tc.name, bodyHash), func(t *testing.T) {
				kv := store.NewMemoryKV()
				defer kv.Close()
				var runs atomic.Int32
				h := func(c *celeris.Context) error { runs.Add(1); return tc.handler(c) }
				mw := New(Config{Store: kv, BodyHash: bodyHash})

				first := runOnce(t, mw, h, "POST", "/x", idemKey("k1"), celeristest.WithBody([]byte("req")))
				for i := range 3 {
					replayed := runOnce(t, mw, h, "POST", "/x", idemKey("k1"), celeristest.WithBody([]byte("req")))
					if replayed.StatusCode != tc.status || replayed.BodyString() != tc.body {
						t.Errorf("replay %d: %d %q, want %d %q", i, replayed.StatusCode, replayed.BodyString(), tc.status, tc.body)
					}
					if got := replayed.Header("content-type"); got != tc.ct {
						t.Errorf("replay %d: content-type %q, want %q", i, got, tc.ct)
					}
				}
				if runs.Load() != 1 {
					t.Errorf("handler ran %d times, want once", runs.Load())
				}
				if first.StatusCode != tc.status || first.BodyString() != tc.body || first.Header("content-type") != tc.ct {
					t.Errorf("first response: %d %q content-type %q, want %d %q %q",
						first.StatusCode, first.BodyString(), first.Header("content-type"), tc.status, tc.body, tc.ct)
				}
			})
		}
	}
}

// Headers the handler sets besides the type are replayed with it, and a
// content-type header the handler set itself still wins over the type c.JSON
// recorded, as it did before the type was stored.
func TestReplayKeepsOtherHeadersAndTheHandlersOwnContentType1280(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	h := func(c *celeris.Context) error {
		c.SetHeader("location", "/orders/7")
		c.SetHeader("content-type", "application/vnd.api+json")
		return c.JSON(201, map[string]int{"id": 7})
	}
	mw := New(Config{Store: kv})

	_ = runOnce(t, mw, h, "POST", "/x", idemKey("k"))
	r := runOnce(t, mw, h, "POST", "/x", idemKey("k"))
	if r.StatusCode != 201 || r.BodyString() != `{"id":7}` || r.Header("location") != "/orders/7" {
		t.Fatalf("replay: %d %q location %q", r.StatusCode, r.BodyString(), r.Header("location"))
	}
	if got := r.Header("content-type"); got != "application/vnd.api+json" {
		t.Errorf("replay: content-type %q, want the handler's own header", got)
	}
}

// A response written with an empty type is replayed with an empty type, not as
// octet-stream, and its body is kept. Only the replay is checked here: the first
// response, a buffered Blob with an empty type, lost its body in FlushResponse
// (celeris#1155), which is not this middleware's to fix.
func TestReplayOfABlobWithAnEmptyContentType1280(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	h := func(c *celeris.Context) error { return c.Blob(200, "", []byte("raw")) }
	mw := New(Config{Store: kv})

	_ = runOnce(t, mw, h, "POST", "/x", idemKey("k"))
	r := runOnce(t, mw, h, "POST", "/x", idemKey("k"))
	if r.StatusCode != 200 || r.BodyString() != "raw" {
		t.Fatalf("replay: %d %q, want 200 \"raw\"", r.StatusCode, r.BodyString())
	}
	if got := r.Header("content-type"); got != "" {
		t.Errorf("replay: content-type %q, want none, as the handler wrote it", got)
	}
}

// An entry written before the content type was stored (by an earlier version, in
// a store that outlives a deploy) replays as it always did: a body with no
// stored type is application/octet-stream. Such an entry with no body is a
// response that had none, and is now a NoContent.
func TestReplayOfAnEntryWithoutAContentType1280(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		headers  [][2]string
		body     string
		wantCT   string
		wantETag string
	}{
		{"body", 200, [][2]string{{"etag", `"v1"`}}, "old", "application/octet-stream", `"v1"`},
		{"no-body", 204, [][2]string{{"etag", `"v2"`}}, "", "", `"v2"`},
	} {
		for _, bodyHash := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bodyhash=%v", tc.name, bodyHash), func(t *testing.T) {
				kv := store.NewMemoryKV()
				defer kv.Close()
				enc := store.EncodedResponse{Status: tc.status, Headers: tc.headers, Body: []byte(tc.body)}.Encode()
				var payload []byte
				if bodyHash {
					sum := sha256.Sum256([]byte("req"))
					payload = append([]byte{entryCompleted}, sum[:]...)
				} else {
					payload = []byte{entryCompleted}
				}
				payload = append(payload, enc...)
				if err := kv.Set(t.Context(), "k", payload, time.Minute); err != nil {
					t.Fatal(err)
				}
				var runs atomic.Int32
				h := func(c *celeris.Context) error { runs.Add(1); return c.String(200, "from the handler") }
				mw := New(Config{Store: kv, BodyHash: bodyHash})

				r := runOnce(t, mw, h, "POST", "/x", idemKey("k"), celeristest.WithBody([]byte("req")))
				if runs.Load() != 0 || r.StatusCode != tc.status || r.BodyString() != tc.body || r.Header("etag") != tc.wantETag {
					t.Fatalf("replay: handler ran %d times, %d %q etag %q, want 0, %d %q %q",
						runs.Load(), r.StatusCode, r.BodyString(), r.Header("etag"), tc.status, tc.body, tc.wantETag)
				}
				if got := r.Header("content-type"); got != tc.wantCT {
					t.Errorf("replay: content-type %q, want %q", got, tc.wantCT)
				}
			})
		}
	}
}

// Duplicates that arrive while the first request is in flight get 409 and no
// response of the first; the next one, after it completed, is a replay that
// carries its Content-Type.
func TestInFlightDuplicateThenReplayKeepsTheContentType1280(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	ready, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	h := func(c *celeris.Context) error {
		if runs.Add(1) == 1 {
			close(ready)
			<-release
		}
		return c.JSON(201, map[string]string{"id": "1"})
	}
	mw := New(Config{Store: kv})

	var first *celeristest.ResponseRecorder
	go func() {
		defer close(done)
		first = runOnce(t, mw, h, "POST", "/x", idemKey("k"))
	}()
	<-ready
	dup := runOnce(t, mw, h, "POST", "/x", idemKey("k"))
	if dup.StatusCode != 409 {
		t.Fatalf("in-flight duplicate: %d, want 409", dup.StatusCode)
	}
	close(release)
	<-done
	if first.StatusCode != 201 || first.Header("content-type") != "application/json" {
		t.Fatalf("first: %d content-type %q", first.StatusCode, first.Header("content-type"))
	}
	r := runOnce(t, mw, h, "POST", "/x", idemKey("k"))
	if r.StatusCode != 201 || r.BodyString() != `{"id":"1"}` || r.Header("content-type") != "application/json" || runs.Load() != 1 {
		t.Fatalf("replay: %d %q content-type %q, handler ran %d times", r.StatusCode, r.BodyString(), r.Header("content-type"), runs.Load())
	}
}

// racyStore hides the completed entry from the first Get of a key, as when the
// first request completes between a follower's Get and its SetNX: the
// follower's SetNX fails and its second look finds the completed entry, which
// is the other place a stored response is replayed from.
type racyStore struct {
	KVStore
	hid atomic.Bool
}

func (s *racyStore) Get(ctx context.Context, key string) ([]byte, error) {
	if s.hid.CompareAndSwap(false, true) {
		return nil, store.ErrNotFound
	}
	return s.KVStore.Get(ctx, key)
}

func TestReplayAfterLosingTheLockRaceKeepsTheContentType1280(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler celeris.HandlerFunc
		status  int
		ct      string
	}{
		{"json", func(c *celeris.Context) error { return c.JSON(201, map[string]int{"n": 1}) }, 201, "application/json"},
		{"nocontent", func(c *celeris.Context) error { return c.NoContent(204) }, 204, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kv := store.NewMemoryKV()
			defer kv.Close()
			// Run the first request on the plain store, then replay through a store that
			// hides the entry from the first Get.
			_ = runOnce(t, New(Config{Store: kv}), tc.handler, "POST", "/x", idemKey("k"))
			var runs atomic.Int32
			h := func(c *celeris.Context) error { runs.Add(1); return tc.handler(c) }
			r := runOnce(t, New(Config{Store: &racyStore{KVStore: kv}}), h, "POST", "/x", idemKey("k"))
			if runs.Load() != 0 || r.StatusCode != tc.status || r.Header("content-type") != tc.ct {
				t.Fatalf("replay: handler ran %d times, %d content-type %q, want 0, %d %q", runs.Load(), r.StatusCode, r.Header("content-type"), tc.status, tc.ct)
			}
		})
	}
}
