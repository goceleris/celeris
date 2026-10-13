package singleflight

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

// TestAcceptEncodingCapBoundary888: up to maxCanonicalAE the value is the
// negotiator's reading (spellings of one list share a key), past it the value
// is the key's as sent (they do not, but equal values still do).
func TestAcceptEncodingCapBoundary888(t *testing.T) {
	key := func(v string) string {
		c, _ := celeristest.NewContext("GET", "/doc", celeristest.WithHeader("accept-encoding", v))
		defer celeristest.ReleaseContext(c)
		return defaultKeyFunc(c)
	}
	// list builds a list of n entries "x0", "x1", ... with sep between them.
	list := func(n int, sep string) string {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteString(sep)
			}
			b.WriteString("x")
			b.WriteString(strings.Repeat("y", i%7))
		}
		return b.String()
	}
	short, spaced := list(10, ","), list(10, ", ")
	if len(spaced) > maxCanonicalAE || key(short) != key(spaced) {
		t.Errorf("a list within the cap: keys %q and %q, want one", key(short), key(spaced))
	}
	long, longSpaced := list(400, ","), list(400, ", ")
	if len(long) <= maxCanonicalAE {
		t.Fatalf("test list is only %d bytes, below the cap %d", len(long), maxCanonicalAE)
	}
	if key(long) != key(strings.Clone(long)) || key(long) == key(longSpaced) {
		t.Errorf("a list past the cap: want equal values to share a key and spellings not to")
	}
	if !strings.Contains(key(long), long) {
		t.Errorf("a list past the cap is not held as sent")
	}
	// The cap is on the value, exactly: maxCanonicalAE bytes are canonical, one more is not.
	at := strings.Repeat("A", maxCanonicalAE)
	if k := key(at); !strings.Contains(k, "\x00e="+strings.ToLower(at)) {
		t.Errorf("a value of exactly maxCanonicalAE bytes is not canonical (key %q)", k)
	}
	if k := key(at + "A"); !strings.Contains(k, "\x00e="+at+"A") {
		t.Errorf("a value of maxCanonicalAE+1 bytes is not held as sent")
	}
}
