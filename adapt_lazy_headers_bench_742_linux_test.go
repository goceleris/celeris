//go:build linux

package celeris_test

import (
	"net/http"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

// The cost of Adapt on epoll and io_uring when nothing read a header before
// it (celeris#742 item 4). The native H1 parser leaves the header slice
// unbuilt until something reads a header, so on such a route Adapt's
// MaterializeHeaders call (#720) builds it: the four pseudo-headers and one
// lowercased entry per raw header. celeristest Contexts carry built headers,
// so a benchmark on them measures Adapt with that work already done.
//
//   - BenchmarkAdaptHeadersBuilt742: the headers were built before Adapt
//     ran (what a route that reads a header first pays in Adapt).
//   - BenchmarkAdaptHeadersLazy742: nothing read a header; Adapt builds the
//     slice. Each iteration first restores the request's unbuilt state.
//   - BenchmarkLazyHeadersRestore742: that restore alone, to subtract.
//
// Adapt's cost on a lazy route is Lazy minus Restore; the header build it
// adds is Lazy minus Restore minus Built.

type benchNopWriter struct{}

func (benchNopWriter) WriteResponse(*stream.Stream, int, [][2]string, []byte) error { return nil }

// benchRequestHeaders is a typical API request's headers, in wire case.
var benchRequestHeaders = [...][2]string{
	{"Host", "api.example.com"},
	{"User-Agent", "Mozilla/5.0 (X11; Linux x86_64) bench/1.0"},
	{"Accept", "application/json"},
	{"Accept-Encoding", "gzip, br"},
	{"Authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.e30.ZRrHA1JJJW8opsbCGfG_HACGpVUMN_a9IV7pAx_Zmeo"},
	{"X-Request-Id", "0f8c2a3e-6b1d-4c9e-9a57-3d2e1f0b7c64"},
	{"Cookie", "session=abc123; theme=dark"},
	{"Connection", "keep-alive"},
}

// lazyBenchStream returns an H1 stream and a restore func that puts it back
// in the state the parser leaves it in: headers unbuilt, raw names in wire
// case (the build lowercases them in place).
func lazyBenchStream() (*stream.Stream, func()) {
	s := stream.NewH1Stream(1)
	s.ResponseWriter = benchNopWriter{}
	raw := make([][2][]byte, len(benchRequestHeaders))
	for i, h := range benchRequestHeaders {
		raw[i] = [2][]byte{[]byte(h[0]), []byte(h[1])}
	}
	restore := func() {
		stream.ResetH1Stream(s)
		s.Headers = s.Headers[:0]
		for i, h := range benchRequestHeaders {
			copy(raw[i][0], h[0])
		}
		s.Method, s.Path, s.Scheme, s.Authority = "GET", "/api/v1/items", "http", "api.example.com"
		s.LazyRawHeaders = raw
	}
	restore()
	return s, restore
}

func benchAdaptHandler() celeris.HandlerFunc {
	return celeris.Adapt(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
}

func BenchmarkAdaptHeadersBuilt742(b *testing.B) {
	h := benchAdaptHandler()
	s, _ := lazyBenchStream()
	s.MaterializeHeaders()
	b.ReportAllocs()
	for b.Loop() {
		c := celeris.AcquireTestContext(s)
		_ = h(c)
		celeris.ReleaseTestContext(c)
	}
}

func BenchmarkAdaptHeadersLazy742(b *testing.B) {
	h := benchAdaptHandler()
	s, restore := lazyBenchStream()
	b.ReportAllocs()
	for b.Loop() {
		restore()
		c := celeris.AcquireTestContext(s)
		_ = h(c)
		celeris.ReleaseTestContext(c)
	}
}

func BenchmarkLazyHeadersRestore742(b *testing.B) {
	_, restore := lazyBenchStream()
	b.ReportAllocs()
	for b.Loop() {
		restore()
	}
}
