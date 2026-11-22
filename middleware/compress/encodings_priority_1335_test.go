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
// The rule it pins: an encoding the client refuses (q=0) or does not name
// is never used; of the rest the one with the highest q wins; among those
// with the same q, Config.Encodings, the server's priority order, decides,
// and the order of the client's header does not. So a browser's "gzip,
// deflate, br, zstd" gets what the server ranks first of the four, and
// Config.Encodings does what its documentation says for the commonest request
// (celeris#1335).
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
			[8]string{"zstd", "zstd", "zstd", "br", "br", "gzip", "gzip", "deflate"}},
		{"benchmark", "gzip, br",
			[8]string{"br", "br", "gzip", "br", "br", "gzip", "gzip", "gzip"}},
		{"br first", "br, gzip",
			[8]string{"br", "br", "gzip", "br", "br", "gzip", "gzip", "gzip"}},
		{"zstd first", "zstd, br, gzip",
			[8]string{"zstd", "zstd", "zstd", "br", "br", "gzip", "gzip", "gzip"}},
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
			[8]string{"br", "br", "gzip", "br", "br", "gzip", "gzip", "gzip"}},
		// Equal q that is not 1: the client's order still decides.
		{"equal q 0.5", "br;q=0.5, zstd;q=0.5, gzip;q=0.5",
			[8]string{"zstd", "zstd", "zstd", "br", "br", "gzip", "gzip", "gzip"}},
		// The wildcard.
		{"star", "*",
			[8]string{"zstd", "zstd", "zstd", "br", "br", "gzip", "gzip", "deflate"}},
		{"gzip then star", "gzip, *",
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
				if v := rec.Header("vary"); !strings.Contains(strings.ToLower(v), "accept-encoding") {
					t.Errorf("Accept-Encoding %q: Vary %q does not name Accept-Encoding", tc.ae, v)
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
