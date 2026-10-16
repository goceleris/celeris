package static

import "testing"

// Benchmarks for celeris#888 item 36 (RULE 74 A/B): static marks the response
// of a GET for a strong-tagged fs.FS file that names an encoding, for
// compress. Each row is a cache-hit request over fsBench846's a.css, with and
// without Accept-Encoding, so the cost of the mark (and of the check that
// decides to make it) shows against the same request that makes none.

func BenchmarkStaticFS200NoAE888(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846()}), "/a.css")
}

func BenchmarkStaticFS200AE888(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846()}), "/a.css", "accept-encoding", "gzip")
}

func BenchmarkStaticFS304NoAE888(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846()}), "/a.css", "if-none-match", "*")
}

func BenchmarkStaticFS304AE888(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846()}), "/a.css", "accept-encoding", "gzip", "if-none-match", "*")
}

// BenchmarkStaticFS304RangeAE888: a revalidation that carries a Range, whose
// outcome (a part or the whole) decides whether the 304 is marked.
func BenchmarkStaticFS304RangeAE888(b *testing.B) {
	benchFSConditional846(b, New(Config{FS: fsBench846()}), "/b.css", "accept-encoding", "gzip", "range", "bytes=0-9", "if-none-match", "*")
}
