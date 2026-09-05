package celeristest_test

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// TestRecorderHeadersAreAWireSnapshot pins that ResponseRecorder.Headers
// records exactly the headers handed to WriteResponse. Before the fix the
// recorder retained the slice passed by Context.Blob, which aliases the
// Context's inline header buffer: a SetCookie issued after the body was
// written (the shape of a post-chain middleware) overwrote the recorded
// content-type in place, so tests saw a Set-Cookie that never reached the
// wire and lost one that did.
func TestRecorderHeadersAreAWireSnapshot(t *testing.T) {
	ctx, rec := celeristest.NewContextT(t, "GET", "/")
	if err := ctx.String(200, "ok"); err != nil {
		t.Fatal(err)
	}
	if got := rec.Header("content-type"); got != "text/plain" {
		t.Fatalf("content-type on the wire: got %q, want text/plain", got)
	}

	// Too late: the response is already written. The Context still
	// accumulates the header (nothing else can), but the wire never sees it.
	ctx.SetCookie(&celeris.Cookie{Name: "late", Value: "x"})
	ctx.SetHeader("x-late", "1")

	if got := rec.Header("content-type"); got != "text/plain" {
		t.Fatalf("post-write header mutation corrupted the recorded wire headers: content-type=%q, headers=%v", got, rec.Headers)
	}
	for _, h := range rec.Headers {
		if h[0] == "set-cookie" || h[0] == "x-late" {
			t.Fatalf("recorder reports header %v that was added after the write and never reached the wire", h)
		}
	}
	if !ctx.IsWritten() {
		t.Fatal("expected IsWritten after String")
	}
}
