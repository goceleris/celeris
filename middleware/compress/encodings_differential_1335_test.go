package compress

import (
	"testing"

	"github.com/goceleris/celeris/internal/negotiate"
)

// diffOffers are the Config.Encodings orders the differential runs under: the
// default, the others of the three default codings, and deflate listed.
var diffOffers = [][]string{
	{"zstd", "br", "gzip"},
	{"zstd", "gzip", "br"},
	{"br", "zstd", "gzip"},
	{"br", "gzip", "zstd"},
	{"gzip", "br", "zstd"},
	{"gzip", "zstd", "br"},
	{"deflate", "gzip", "br", "zstd"},
	{"gzip"},
	{"br"},
}

// diffHeaders are Accept-Encoding values that cover the grammar: no weights,
// weights, "*", refusals, repeated and parametrised entries, case, blanks,
// "x-gzip", values that are no weight, and the empty header.
var diffHeaders = []string{
	"", "gzip", "br", "zstd", "deflate", "identity",
	"gzip, deflate, br, zstd", "gzip, br", "br, gzip", "zstd, br, gzip", "deflate, gzip",
	"br;q=1.0, gzip;q=0.8", "gzip, br;q=0.5", "gzip, br, zstd;q=0.1",
	"br;q=0.5, zstd;q=0.5, gzip;q=0.5", "gzip;q=0.9, br;q=0.9, zstd;q=0.1",
	"*", "*;q=0", "*;q=0.5", "*/*", "gzip, *", "*, gzip", "gzip;q=0, *", "gzip;q=0.5, *",
	"gzip, *;q=0", "br, *;q=0", "*;q=0, gzip", "gzip;q=1.0, identity; q=0.5, *;q=0",
	"identity;q=0", "gzip;q=0", "gzip;q=0, gzip", "gzip, gzip;q=0", "gzip;q=0.3, gzip;q=0.7",
	"zstd;q=0, br;Q=0, gzip", "zstd;Q=0, br;Q=0, gzip;Q=0",
	"GZIP, BR", "x-gzip", "x-gzip, br", "gzip;q=0, x-gzip", "X-GZIP;q=0, *",
	"gzip;foo=1", "gzip;foo=1, br", "gzip;foo=1;q=0, gzip", "gzip;foo=1;q=0, *", "br;level=2;q=0.5, gzip;q=0.5",
	"gzip;q=NaN, br", "gzip;q=1e5, br", "gzip;q=-1, br", "gzip;q=.5, br", "gzip;q=1.5, br",
	"gzip;q=0.0001, br;q=0.0002", "gzip;q = 0, br", "gzip ; q=0.5 , br ; q=0.4",
	",,,gzip,,br,,", " gzip , br ", "gzip;", "gzip;q", "gzip;q=", ";q=0.5", "text/html, gzip",
}

// TestNegotiateEncodingMatchesCore pins the middleware's own negotiator to
// internal/negotiate.AcceptServerOrder, the rule written against the core: the
// middleware keeps a copy because it cannot import an internal package that
// older cores lack (celeris#1335).
func TestNegotiateEncodingMatchesCore(t *testing.T) {
	for _, h := range diffHeaders {
		for _, offers := range diffOffers {
			if got, want := negotiateEncoding(h, offers), negotiate.AcceptServerOrder(h, offers); got != want {
				t.Errorf("Accept-Encoding %q, offers %v: negotiateEncoding %q, core %q", h, offers, got, want)
			}
		}
	}
}

// FuzzNegotiateEncodingMatchesCore: the same, for any header (and a word of
// the offers' order picked by n). Run it for a while with
//
//	go test -run xxx -fuzz FuzzNegotiateEncodingMatchesCore -fuzztime 30s
func FuzzNegotiateEncodingMatchesCore(f *testing.F) {
	for i, h := range diffHeaders {
		f.Add(h, uint8(i))
	}
	f.Fuzz(func(t *testing.T, h string, n uint8) {
		offers := diffOffers[int(n)%len(diffOffers)]
		if got, want := negotiateEncoding(h, offers), negotiate.AcceptServerOrder(h, offers); got != want {
			t.Fatalf("Accept-Encoding %q, offers %v: negotiateEncoding %q, core %q", h, offers, got, want)
		}
	})
}
