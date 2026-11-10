package negotiate

import (
	"strings"
	"testing"
)

// BenchmarkAcceptTypical888 is the request-path cost of Accept for the headers
// compress and static negotiate on (RFC 9110 12.5.3 rewrite, celeris#888
// items 44, 45 and 40): a browser's list, a wildcard list, a q=0 list.
func BenchmarkAcceptTypical888(b *testing.B) {
	offers := []string{"zstd", "br", "gzip"}
	for _, tc := range []struct{ name, header string }{
		{"browser", "gzip, deflate, br, zstd"},
		{"weighted", "br;q=0.9, gzip;q=0.8, *;q=0.1"},
		{"star-q0", "gzip, *;q=0"},
		{"refused", "br;q=0, gzip;q=0, zstd;q=0"},
		{"one", "gzip"},
		{"star-first", "*, gzip"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = Accept(tc.header, offers)
			}
		})
	}
}

func BenchmarkAcceptMedia888(b *testing.B) {
	offers := []string{"application/json", "application/xml", "text/plain"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Accept("text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", offers)
	}
}

// BenchmarkAcceptLongHeader888 is Accept over a header the client chose the
// length of (celeris#888 item 42): 64 KiB, the native engines' head limit, of
// "a," and of "a;q=0.5,". Before the bound of 64 entries the first cost about
// 3.8 MB and 32768 entries per call.
func BenchmarkAcceptLongHeader888(b *testing.B) {
	offers := []string{"zstd", "br", "gzip"}
	for _, tc := range []struct{ name, header string }{
		{"entries", strings.Repeat("a,", 32<<10)},
		{"weighted", strings.Repeat("a;q=0.5,", 8<<10)},
		{"commas", strings.Repeat(",", 64<<10)},
		// One list element with many parameters: not bounded (no heap, CPU linear in the header).
		{"params", "a" + strings.Repeat(";q=1", 16<<10)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = Accept(tc.header, offers)
			}
		})
	}
}
