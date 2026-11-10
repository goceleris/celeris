package celeristest_test

import (
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

// celeris#889 item 16: a request built with several cookie fields (WithHeader
// and WithCookie each add one) reaches the handler as one field joined with
// "; ", as the engines deliver it, so a test sees what a server sees.
func TestSeveralCookieFieldsAreJoined889(t *testing.T) {
	ctx, _ := celeristest.NewContext("GET", "/",
		celeristest.WithHeader("Cookie", "theme=dark"),
		celeristest.WithHeader("Cookie", "sid=alice"),
		celeristest.WithCookie("lang", "en"),
	)
	defer celeristest.ReleaseContext(ctx)
	if got, want := ctx.Header("cookie"), "theme=dark; sid=alice; lang=en"; got != want {
		t.Errorf("Header(cookie) = %q, want %q", got, want)
	}
	n := 0
	for _, h := range ctx.RequestHeaders() {
		if h[0] == "cookie" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d cookie fields, want 1", n)
	}
	for name, want := range map[string]string{"theme": "dark", "sid": "alice", "lang": "en"} {
		if got, err := ctx.Cookie(name); err != nil || got != want {
			t.Errorf("Cookie(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
}
