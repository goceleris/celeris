package singleflight

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// celeris#1155: a Blob written with an empty Content-Type lost its body twice
// under singleflight: the leader's FlushResponse sent it bodyless, and the
// waiters' replay (entry.ct == "") sent NoContent.

// The leader's own response (FlushResponse), with no concurrency.
func TestBlobWithEmptyContentTypeKeepsItsBody1155(t *testing.T) {
	for _, tc := range []struct{ name, ct string }{{"with-type", "text/plain"}, {"empty-type", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			h := func(c *celeris.Context) error { return c.Blob(200, tc.ct, []byte("BLOB")) }
			rec, err := testutil.RunChain(t, []celeris.HandlerFunc{New(), h}, "GET", "/x")
			testutil.AssertNoError(t, err)
			if got := rec.BodyString(); got != "BLOB" {
				t.Fatalf("body = %q, want BLOB", got)
			}
		})
	}
}

// The waiters' replay: the leader blocks in its handler until every waiter has
// joined, then writes c.Blob(200, "", body); every request, the waiters' HIT
// responses included, gets the body.
func TestWaitersGetABlobWithEmptyContentType1155(t *testing.T) {
	for _, tc := range []struct {
		name   string
		write  func(c *celeris.Context) error
		status int
		body   string
		ct     string
	}{
		{"empty-type", func(c *celeris.Context) error { return c.Blob(200, "", []byte("BLOB")) }, 200, "BLOB", ""},
		{"empty-type-201", func(c *celeris.Context) error { return c.Blob(201, "", []byte("made")) }, 201, "made", ""},
		{"with-type", func(c *celeris.Context) error { return c.Blob(200, "text/plain", []byte("BLOB")) }, 200, "BLOB", "text/plain"},
		// A bodyless response is still replayed bodyless.
		{"nocontent", func(c *celeris.Context) error { return c.NoContent(204) }, 204, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct := &concurrentTest{
				n:       3,
				method:  "GET",
				path:    "/x",
				entered: make(chan struct{}),
				gate:    make(chan struct{}),
			}
			ct.handler = func(c *celeris.Context) error {
				ct.enterOnce.Do(func() { close(ct.entered) })
				<-ct.gate
				return tc.write(c)
			}
			ct.run(t)

			hits := 0
			for i, rec := range ct.recs {
				if ct.errs[i] != nil {
					t.Fatalf("request %d: %v", i, ct.errs[i])
				}
				if rec.StatusCode != tc.status || rec.BodyString() != tc.body {
					t.Fatalf("request %d: %d %q, want %d %q", i, rec.StatusCode, rec.Body, tc.status, tc.body)
				}
				if got := rec.Header("content-type"); got != tc.ct {
					t.Fatalf("request %d: content-type %q, want %q", i, got, tc.ct)
				}
				if rec.Header("x-singleflight") == "HIT" {
					hits++
				}
			}
			if hits != ct.n-1 {
				t.Fatalf("%d waiter responses (x-singleflight: HIT), want %d", hits, ct.n-1)
			}
		})
	}
}
