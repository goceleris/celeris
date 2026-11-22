package compress

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// priorityOrders are the Config.Encodings values the table below is written
// for, in column order. The first is the default configuration (New() with no
// Config), which is ["zstd", "br", "gzip"]; the last adds the opt-in deflate.
var priorityOrders = [][]string{
	nil, // New(): the default Encodings
	{"zstd", "br", "gzip"},
	{"zstd", "gzip", "br"},
	{"br", "zstd", "gzip"},
	{"br", "gzip", "zstd"},
	{"gzip", "br", "zstd"},
	{"gzip", "zstd", "br"},
	{"deflate", "gzip", "br", "zstd"},
}

// TestEncodingsPriority pins the Content-Encoding the compress middleware
// answers for a set of Accept-Encoding headers, under the default
// configuration and under each Encodings order. The want field holds one
// answer per priorityOrders entry, in that order; "-" is no Content-Encoding.
//
// It pins the behaviour of the middleware before celeris#1335: among encodings
// the client offers with the same q, the one named first in the CLIENT's
// header won, and Config.Encodings only broke the ties the header left (an
// "*", or a repeated name). So a browser's "gzip, deflate, br, zstd" always
// got gzip, whatever Encodings said.
func TestEncodingsPriority(t *testing.T) {
	body := testBody()
	h := func(c *celeris.Context) error { return c.Blob(200, "text/plain", body) }

	for _, tc := range []struct {
		name string
		ae   string
		// want, one entry per priorityOrders column.
		want [8]string
	}{
		// What the browsers and the benchmark client send, no q-values.
		{"browser", "gzip, deflate, br, zstd",
			[8]string{"gzip", "gzip", "gzip", "gzip", "gzip", "gzip", "gzip", "gzip"}},
		{"benchmark", "gzip, br",
			[8]string{"gzip", "gzip", "gzip", "gzip", "gzip", "gzip", "gzip", "gzip"}},
		{"br first", "br, gzip",
			[8]string{"br", "br", "br", "br", "br", "br", "br", "br"}},
		{"zstd first", "zstd, br, gzip",
			[8]string{"zstd", "zstd", "zstd", "zstd", "zstd", "zstd", "zstd", "zstd"}},
		{"deflate then gzip", "deflate, gzip",
			[8]string{"gzip", "gzip", "gzip", "gzip", "gzip", "gzip", "gzip", "deflate"}},
		// A single encoding.
		{"only gzip", "gzip",
			[8]string{"gzip", "gzip", "gzip", "gzip", "gzip", "gzip", "gzip", "gzip"}},
		{"only br", "br",
			[8]string{"br", "br", "br", "br", "br", "br", "br", "br"}},
		{"only zstd", "zstd",
			[8]string{"zstd", "zstd", "zstd", "zstd", "zstd", "zstd", "zstd", "zstd"}},
		{"only deflate", "deflate",
			[8]string{"-", "-", "-", "-", "-", "-", "-", "deflate"}},
		{"no header", "",
			[8]string{"-", "-", "-", "-", "-", "-", "-", "-"}},
		{"identity", "identity",
			[8]string{"-", "-", "-", "-", "-", "-", "-", "-"}},
		// An explicit q wins over any order.
		{"q br over gzip", "br;q=1.0, gzip;q=0.8",
			[8]string{"br", "br", "br", "br", "br", "br", "br", "br"}},
		{"q gzip over br", "gzip, br;q=0.5",
			[8]string{"gzip", "gzip", "gzip", "gzip", "gzip", "gzip", "gzip", "gzip"}},
		{"q zstd lowest", "gzip, br, zstd;q=0.1",
			[8]string{"gzip", "gzip", "gzip", "gzip", "gzip", "gzip", "gzip", "gzip"}},
		// Equal q that is not 1: the client's order still decides.
		{"equal q 0.5", "br;q=0.5, zstd;q=0.5, gzip;q=0.5",
			[8]string{"br", "br", "br", "br", "br", "br", "br", "br"}},
		// The wildcard.
		{"star", "*",
			[8]string{"zstd", "zstd", "zstd", "br", "br", "gzip", "gzip", "deflate"}},
		{"gzip refused, star", "gzip;q=0, *",
			[8]string{"zstd", "zstd", "zstd", "br", "br", "br", "zstd", "deflate"}},
		{"gzip low, star", "gzip;q=0.5, *",
			[8]string{"zstd", "zstd", "zstd", "br", "br", "br", "zstd", "deflate"}},
		// Refusals, and what is not listed is not accepted.
		{"identity refused", "identity;q=0",
			[8]string{"-", "-", "-", "-", "-", "-", "-", "-"}},
		{"gzip refused", "gzip;q=0",
			[8]string{"-", "-", "-", "-", "-", "-", "-", "-"}},
		{"star refused", "*;q=0",
			[8]string{"-", "-", "-", "-", "-", "-", "-", "-"}},
	} {
		for i, order := range priorityOrders {
			var mw celeris.HandlerFunc
			name := tc.name + "/default"
			if order == nil {
				mw = New()
			} else {
				name = tc.name + "/" + strings.Join(order, ">")
				mw = New(Config{Encodings: order})
			}
			t.Run(name, func(t *testing.T) {
				opts := []celeristest.Option{celeristest.WithHandlers(mw, h)}
				if tc.ae != "" {
					opts = append(opts, celeristest.WithHeader("accept-encoding", tc.ae))
				}
				c, rec := celeristest.NewContextT(t, "GET", "/", opts...)
				if err := c.Next(); err != nil {
					t.Fatalf("Accept-Encoding %q: %v", tc.ae, err)
				}
				got := rec.Header("content-encoding")
				if got == "" {
					got = "-"
				}
				if got != tc.want[i] {
					t.Errorf("Accept-Encoding %q, Encodings %v: Content-Encoding %q, want %q",
						tc.ae, order, got, tc.want[i])
				}
			})
		}
	}
}
