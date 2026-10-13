package cache

import (
	"strings"
	"testing"
)

// The decision on one Cache-Control value, measured in the same run: main's
// substring check, and parseCacheControl (celeris#888 item 27: directives as
// tokens, a value that is not a list of directives is not stored).
var ccBenchValues = []struct{ name, cc string }{
	{"typical", "public, max-age=3600, must-revalidate"},
	{"short", "max-age=60"},
	{"long", "public, max-age=31536000, immutable, s-maxage=600, stale-while-revalidate=30, stale-if-error=86400, must-revalidate"},
	{"quoted", `public, ext="a, b", max-age=3600`},
}

var ccSink bool

func BenchmarkCacheControlDecision(b *testing.B) {
	for _, v := range ccBenchValues {
		b.Run("main-substring/"+v.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ccSink = strings.Contains(v.cc, "no-store") || strings.Contains(v.cc, "private")
			}
		})
		b.Run("parse/"+v.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ccSink, _, _ = parseCacheControl(v.cc)
			}
		})
	}
}
