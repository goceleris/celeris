package compress

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/internal/negotiate"
)

// mainDefaultEncodings is the default Config.Encodings order before
// celeris#1335. Before it, the middleware chose with
// negotiate.Accept(header, Encodings), which Context.AcceptsEncodings still
// calls, so that call with this order is the answer of the code before the
// change under the default configuration.
var mainDefaultEncodings = []string{"zstd", "br", "gzip"}

// encShape is one Accept-Encoding header and the Content-Encoding it got
// before celeris#1335 (before: negotiate.Accept with the offers of the old
// configuration) and gets now (after: the middleware). "" is no encoding.
type encShape struct {
	name   string
	ae     string
	before string
	after  string
}

// shape returns the row for the header ae. The subtest name is the header, with
// the "/" of "*/*" replaced because it would nest the subtest.
func shape(ae, before, after string) encShape {
	return encShape{strings.ReplaceAll(ae, "/", "|"), ae, before, after}
}

// unknownEntries is n entries no coding matches, as a prefix of a header.
func unknownEntries(n int) string { return strings.Repeat("foo, ", n) }

// contentEncodingFor runs mw and a handler that answers with a compressible
// body for a request with the Accept-Encoding header ae (none when empty) and
// returns the Content-Encoding of the response.
func contentEncodingFor(t *testing.T, mw celeris.HandlerFunc, ae string) string {
	t.Helper()
	body := testBody()
	h := func(c *celeris.Context) error { return c.Blob(200, "text/plain", body) }
	opts := []celeristest.Option{celeristest.WithHandlers(mw, h)}
	if ae != "" {
		opts = append(opts, celeristest.WithHeader("accept-encoding", ae))
	}
	c, rec := celeristest.NewContextT(t, "GET", "/", opts...)
	if err := c.Next(); err != nil {
		t.Fatalf("Accept-Encoding %q: %v", ae, err)
	}
	return rec.Header("content-encoding")
}

// changedDefaultShapes are the Accept-Encoding headers whose answer under the
// default configuration changes with celeris#1335: the default order moves
// from [zstd br gzip] to [gzip zstd br], and of the encodings the client
// accepts at the highest q-value the first in the server's order wins, where
// the one the client's header ranked first won before. The rows are the three
// kinds of move: br to gzip, zstd to gzip and br to zstd. No row moves to br,
// and none starts or stops compressing (TestDefaultOrderMovesAreContained).
var changedDefaultShapes = []encShape{
	// br to gzip: gzip is among the encodings at the top q-value, and br was
	// ranked above it.
	shape("br, gzip", "br", "gzip"),
	shape("BR, GZIP", "br", "gzip"),
	shape("deflate, br, gzip", "br", "gzip"),
	shape("br, *", "br", "gzip"),
	shape("br, x-gzip", "br", "gzip"),
	shape("br, gzip;q=0.5, gzip", "br", "gzip"),
	shape("*;q=0, br, gzip", "br", "gzip"),
	shape("br;q=0.5, zstd;q=0.5, gzip;q=0.5", "br", "gzip"),
	shape("br;q=0.7, gzip;q=0.7, gzip;q=0.3", "br", "gzip"),
	shape("zstd;q=0.5, *", "br", "gzip"),
	{"62 unknown entries, br, gzip", unknownEntries(62) + "br, gzip", "br", "gzip"},
	// zstd to gzip: gzip is among the encodings at the top q-value, and zstd
	// was ranked above it (the position of a "*" is one position for all the
	// codings it covers, and a tie there went to the first of the old order).
	shape("zstd, gzip", "zstd", "gzip"),
	shape("zstd, br, gzip", "zstd", "gzip"),
	shape("*", "zstd", "gzip"),
	shape("*/*", "zstd", "gzip"),
	shape("zstd, *", "zstd", "gzip"),
	shape("*, br", "zstd", "gzip"),
	shape("*, gzip", "zstd", "gzip"),
	shape("deflate, *", "zstd", "gzip"),
	shape("identity, *", "zstd", "gzip"),
	shape("br;q=0.5, *", "zstd", "gzip"),
	shape("*;q=0.5, gzip;q=0.5", "zstd", "gzip"),
	shape("*;q=0.5, br;q=0.5", "zstd", "gzip"),
	// br to zstd: gzip is not accepted at the top q-value, br and zstd are,
	// and br was ranked above zstd.
	shape("br, zstd", "br", "zstd"),
	shape("gzip;q=0, br, zstd", "br", "zstd"),
	shape("gzip;q=0.5, br, zstd", "br", "zstd"),
	shape("br;q=0.5, zstd;q=0.5", "br", "zstd"),
	shape("gzip;q=0, br;q=0.5, zstd;q=0.5, *", "br", "zstd"),
	shape("gzip, br, *, x-gzip;q=0", "br", "zstd"),
}

// unchangedDefaultShapes are headers whose answer under the default
// configuration is the same before and after celeris#1335 (before and after
// are equal for them). They include the headers the browsers and the benchmark
// client send.
var unchangedDefaultShapes = []encShape{
	{"no header", "", "", ""},
	shape("identity", "", ""),
	shape("identity;q=0", "", ""),
	shape("gzip;q=0", "", ""),
	shape("*;q=0", "", ""),
	shape("deflate", "", ""),
	shape("gzip", "gzip", "gzip"),
	shape("br", "br", "br"),
	shape("zstd", "zstd", "zstd"),
	shape("gzip, deflate, br, zstd", "gzip", "gzip"),
	shape("gzip, br", "gzip", "gzip"),
	shape("gzip, br;q=0.5", "gzip", "gzip"),
	shape("br;q=0.8, gzip", "gzip", "gzip"),
	shape("br;q=1.0, gzip;q=0.8", "br", "br"),
	shape("zstd, br", "zstd", "zstd"),
	shape("gzip;q=0, *", "zstd", "zstd"),
	shape("gzip;q=0.5, *", "zstd", "zstd"),
	shape("gzip, *;q=0", "gzip", "gzip"),
	shape("br, *;q=0", "br", "br"),
	shape("br, *;q=0.5", "br", "br"),
	// gzip is the 65th entry and is not read: only br is accepted, as before.
	{"63 unknown entries, br, gzip", unknownEntries(63) + "br, gzip", "br", "br"},
}

func TestChangedShapesDefault(t *testing.T) {
	for _, tc := range changedDefaultShapes {
		t.Run(tc.name, func(t *testing.T) {
			if tc.before == tc.after {
				t.Fatalf("Accept-Encoding %q: the row does not change the answer (%q)", tc.ae, tc.before)
			}
			if got := negotiate.Accept(tc.ae, mainDefaultEncodings); got != tc.before {
				t.Errorf("Accept-Encoding %q: negotiate.Accept with %v gives %q, the row says %q before",
					tc.ae, mainDefaultEncodings, got, tc.before)
			}
			if got := contentEncodingFor(t, New(), tc.ae); got != tc.after {
				t.Errorf("Accept-Encoding %q: Content-Encoding %q, want %q (before celeris#1335: %q)",
					tc.ae, got, tc.after, tc.before)
			}
		})
	}
}

func TestUnchangedShapesDefault(t *testing.T) {
	for _, tc := range unchangedDefaultShapes {
		t.Run(tc.name, func(t *testing.T) {
			if tc.before != tc.after {
				t.Fatalf("Accept-Encoding %q: the row changes the answer (%q to %q)", tc.ae, tc.before, tc.after)
			}
			if got := negotiate.Accept(tc.ae, mainDefaultEncodings); got != tc.before {
				t.Errorf("Accept-Encoding %q: negotiate.Accept with %v gives %q, the row says %q before",
					tc.ae, mainDefaultEncodings, got, tc.before)
			}
			if got := contentEncodingFor(t, New(), tc.ae); got != tc.after {
				t.Errorf("Accept-Encoding %q: Content-Encoding %q, want %q", tc.ae, got, tc.after)
			}
		})
	}
}

// TestChangedShapesExplicitEncodings pins the headers whose answer changes for
// a server that sets Config.Encodings itself: of the encodings the client
// accepts at the same q-value the first listed there wins, where the first in
// the client's header won before. The first five rows are what a server gets
// that wrote the old default, or a brotli-first order, into its config.
func TestChangedShapesExplicitEncodings(t *testing.T) {
	for _, tc := range []struct {
		ae        string
		encodings []string
		before    string
		after     string
	}{
		{"gzip, deflate, br, zstd", []string{"zstd", "br", "gzip"}, "gzip", "zstd"},
		{"gzip, deflate, br, zstd", []string{"zstd", "gzip", "br"}, "gzip", "zstd"},
		{"gzip, deflate, br, zstd", []string{"br", "zstd", "gzip"}, "gzip", "br"},
		{"gzip, deflate, br, zstd", []string{"br", "gzip", "zstd"}, "gzip", "br"},
		{"gzip, deflate, br, zstd", []string{"deflate", "gzip", "br", "zstd"}, "gzip", "deflate"},
		{"gzip, br", []string{"zstd", "br", "gzip"}, "gzip", "br"},
		{"gzip, br", []string{"br", "zstd", "gzip"}, "gzip", "br"},
		{"gzip, br", []string{"br", "gzip"}, "gzip", "br"},
		{"br, gzip", []string{"gzip", "br", "zstd"}, "br", "gzip"},
		{"zstd, br, gzip", []string{"gzip", "br", "zstd"}, "zstd", "gzip"},
		{"zstd, br, gzip", []string{"br", "zstd", "gzip"}, "zstd", "br"},
		{"gzip, *", []string{"zstd", "br", "gzip"}, "gzip", "zstd"},
		{"gzip, *", []string{"br", "gzip", "zstd"}, "gzip", "br"},
	} {
		t.Run(tc.ae+" with "+strings.Join(tc.encodings, ">"), func(t *testing.T) {
			if tc.before == tc.after {
				t.Fatalf("Accept-Encoding %q: the row does not change the answer (%q)", tc.ae, tc.before)
			}
			if got := negotiate.Accept(tc.ae, tc.encodings); got != tc.before {
				t.Errorf("Accept-Encoding %q: negotiate.Accept with %v gives %q, the row says %q before",
					tc.ae, tc.encodings, got, tc.before)
			}
			if got := contentEncodingFor(t, New(Config{Encodings: tc.encodings}), tc.ae); got != tc.after {
				t.Errorf("Accept-Encoding %q, Encodings %v: Content-Encoding %q, want %q (before celeris#1335: %q)",
					tc.ae, tc.encodings, got, tc.after, tc.before)
			}
		})
	}
}
