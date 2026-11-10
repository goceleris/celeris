package singleflight

import (
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

// celeris#889 item 16: a request's several Cookie lines reach the handler as
// one joined field (celeristest builds requests that way, as the engines
// deliver them), so the default key holds every cookie and tells apart two
// requests that differ in a later line only.
func TestDefaultKeyHoldsEveryCookieLine889(t *testing.T) {
	keyOf := func(proto string, cookies ...string) string {
		opts := []celeristest.Option{celeristest.WithProtocol(proto)}
		for _, v := range cookies {
			opts = append(opts, celeristest.WithHeader("cookie", v))
		}
		c, _ := celeristest.NewContext("GET", "/doc", opts...)
		defer celeristest.ReleaseContext(c)
		return defaultKeyFunc(c)
	}
	for _, proto := range []string{"1.1", "2"} {
		a := keyOf(proto, "lang=en", "sid=ALICE")
		if b := keyOf(proto, "lang=en", "sid=MALLORY"); a == b {
			t.Errorf("HTTP/%s: two requests with the same first Cookie line and different second lines share the key %q", proto, a)
		}
		if got, want := a, "GET\x00/doc\x00c=lang=en; sid=ALICE"; got != want {
			t.Errorf("HTTP/%s: key = %q, want %q", proto, got, want)
		}
	}
}

// BenchmarkDefaultKeyCookie889 is the default key of a request with one cookie
// field: the cookie component is one Header lookup, as it was.
func BenchmarkDefaultKeyCookie889(b *testing.B) {
	c, _ := celeristest.NewContext("GET", "/api/items", celeristest.WithHeader("cookie", "theme=dark; sid=alice"))
	defer celeristest.ReleaseContext(c)
	b.ReportAllocs()
	for b.Loop() {
		_ = defaultKeyFunc(c)
	}
}
