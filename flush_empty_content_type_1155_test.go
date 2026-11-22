package celeris

import (
	"testing"
)

// celeris#1155: FlushResponse read an empty captured Content-Type as "this was
// a NoContent response" and sent a buffered Blob(code, "", body) with no body.
// c.Blob(200, "", data) is what Adapt and AdaptFunc write for a net/http
// handler that sets no Content-Type, so it was lost behind any middleware that
// buffers.

func TestFlushResponseKeepsBodyWithEmptyContentType1155(t *testing.T) {
	for _, tc := range []struct {
		name       string
		write      func(c *Context) error
		wantStatus int
		wantBody   string
		wantCT     string // the captured Content-Type the flush replays
	}{
		{"blob-with-type", func(c *Context) error { return c.Blob(200, "text/plain", []byte("BLOB")) }, 200, "BLOB", "text/plain"},
		{"blob-empty-type", func(c *Context) error { return c.Blob(200, "", []byte("BLOB")) }, 200, "BLOB", ""},
		{"blob-empty-type-201", func(c *Context) error { return c.Blob(201, "", []byte("made")) }, 201, "made", ""},
		{"blob-empty-type-one-byte", func(c *Context) error { return c.Blob(200, "", []byte("x")) }, 200, "x", ""},
		// A response with no body at all stays bodyless.
		{"nocontent", func(c *Context) error { return c.NoContent(204) }, 204, "", ""},
		{"nocontent-200", func(c *Context) error { return c.NoContent(200) }, 200, "", ""},
		{"blob-empty-type-empty-body", func(c *Context) error { return c.Blob(200, "", nil) }, 200, "", ""},
		{"blob-with-type-empty-body", func(c *Context) error { return c.Blob(200, "text/plain", nil) }, 200, "", "text/plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rw := newTestStream("GET", "/x")
			defer s.Release()
			c := acquireContext(s)
			defer releaseContext(c)

			c.BufferResponse()
			if err := tc.write(c); err != nil {
				t.Fatal(err)
			}
			if c.IsWritten() {
				t.Fatal("the response reached the wire before FlushResponse")
			}
			if err := c.FlushResponse(); err != nil {
				t.Fatal(err)
			}
			if rw.status != tc.wantStatus || string(rw.body) != tc.wantBody {
				t.Fatalf("wire = %d %q, want %d %q", rw.status, rw.body, tc.wantStatus, tc.wantBody)
			}
			var ct string
			for _, h := range rw.headers {
				if h[0] == "content-type" {
					ct = h[1]
				}
			}
			if ct != tc.wantCT {
				t.Fatalf("content-type on the wire = %q, want %q", ct, tc.wantCT)
			}
		})
	}
}

// Nested BufferResponse layers: the body survives the inner flush (a depth
// decrement) and the outer one (the write).
func TestFlushResponseNestedKeepsBodyWithEmptyContentType1155(t *testing.T) {
	s, rw := newTestStream("GET", "/x")
	defer s.Release()
	c := acquireContext(s)
	defer releaseContext(c)

	c.BufferResponse()
	c.BufferResponse()
	if err := c.Blob(200, "", []byte("nested")); err != nil {
		t.Fatal(err)
	}
	if err := c.FlushResponse(); err != nil {
		t.Fatal(err)
	}
	if c.IsWritten() {
		t.Fatal("the inner FlushResponse wrote")
	}
	if err := c.FlushResponse(); err != nil {
		t.Fatal(err)
	}
	if rw.status != 200 || string(rw.body) != "nested" {
		t.Fatalf("wire = %d %q, want 200 \"nested\"", rw.status, rw.body)
	}
}
