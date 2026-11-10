package negotiate

import (
	"math"
	"testing"
)

// celeris#888: a weight outside the grammar of RFC 9110 12.4.2 is not a weight
// (ParseFloat read "NaN", "Inf", "1e5", "-1" and ".5" as one), and x-gzip is
// gzip (RFC 9110 8.4.1.3) for the entries and the offers alike.
func TestParseQValueGrammar888(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
		ok   bool
	}{
		{"0", 0, true}, {"1", 1, true}, {"0.", 0, true}, {"1.", 1, true},
		{"0.5", 0.5, true}, {"0.500", 0.5, true}, {"0.001", 0.001, true},
		{"1.0", 1, true}, {"1.000", 1, true}, {"0.0", 0, true},
		// a recipient reads more digits than a sender may send
		{"0.0001", 0.0001, true}, {"1.00000", 1, true},
		// outside the grammar
		{"", 0, false}, {"NaN", 0, false}, {"nan", 0, false}, {"Inf", 0, false}, {"+Inf", 0, false},
		{"-Inf", 0, false}, {"1e5", 0, false}, {"1e-3", 0, false}, {"0x1", 0, false}, {"1_0", 0, false},
		{"-1", 0, false}, {"-0", 0, false}, {"+1", 0, false}, {".5", 0, false}, {".", 0, false},
		{"2", 0, false}, {"5", 0, false}, {"1.5", 0, false}, {"1.0001", 0, false}, {"01", 0, false},
		{"0,5", 0, false}, {"0.5x", 0, false}, {"0 .5", 0, false}, {"999", 0, false},
	} {
		got, ok := parseQValue(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseQValue(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// A malformed weight is ignored, as "q=abc" always was: the entry has q=1.
func TestMalformedWeightIsIgnored888(t *testing.T) {
	for _, h := range []string{
		"gzip;q=NaN", "gzip;q=Inf", "gzip;q=+Inf", "gzip;q=-Inf", "gzip;q=1e5", "gzip;q=1e-9",
		"gzip;q=-1", "gzip;q=.5", "gzip;q=2", "gzip;q=999", "gzip;q=abc", "gzip;q=0x1",
	} {
		got := Parse(h)
		if len(got) != 1 || got[0].MediaType != "gzip" || got[0].Quality != 1 {
			t.Errorf("Parse(%q) = %+v, want gzip at q=1", h, got)
		}
		if math.IsNaN(got[0].Quality) {
			t.Errorf("Parse(%q) leaves a NaN weight", h)
		}
	}
}

func TestMalformedWeightDoesNotChangeTheChoice888(t *testing.T) {
	for _, tc := range []struct {
		header string
		offers []string
		want   string
	}{
		// NaN compared false with everything: gzip could never be chosen over
		// br, and "br;q=Inf" outweighed everything.
		{"gzip;q=NaN, br;q=0.5", []string{"br", "gzip"}, "gzip"},
		{"gzip, br;q=Inf", []string{"gzip", "br"}, "gzip"},
		{"gzip, br;q=1e5", []string{"gzip", "br"}, "gzip"},
		{"gzip;q=0.5, br;q=1e5", []string{"gzip", "br"}, "br"}, // br at q=1, not at 100000
		{"gzip;q=0.5, br;q=-Inf", []string{"gzip", "br"}, "br"},
		{"text/html;q=NaN, text/plain;q=0.5", []string{"text/plain", "text/html"}, "text/html"},
		// Well-formed weights are what they were.
		{"gzip;q=0.5, br;q=0.4", []string{"br", "gzip"}, "gzip"},
		{"gzip;q=0.0001, br;q=0", []string{"br", "gzip"}, "gzip"},
		{"gzip;q=0, br;q=1.000", []string{"gzip", "br"}, "br"},
	} {
		if got := Accept(tc.header, tc.offers); got != tc.want {
			t.Errorf("Accept(%q, %q) = %q, want %q", tc.header, tc.offers, got, tc.want)
		}
	}
}

func TestXGzipIsGzip888(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		offers []string
		want   string
	}{
		{"x-gzip-accepts-gzip", "x-gzip", []string{"gzip"}, "gzip"},
		{"x-gzip-upper", "X-GZIP", []string{"gzip"}, "gzip"},
		{"x-gzip-q0-refuses-gzip", "x-gzip;q=0", []string{"gzip"}, ""},
		{"x-gzip-q0-refuses-gzip-with-star", "x-gzip;q=0, *", []string{"gzip", "br"}, "br"},
		// 96: gzip refused by name is refused, the wildcard is not an x-gzip.
		{"gzip-q0-star", "gzip;q=0, *", []string{"gzip", "x-gzip"}, ""},
		{"gzip-q0-star-gzip-only", "gzip;q=0, *", []string{"gzip"}, ""},
		{"gzip-q0-star-other-coding", "gzip;q=0, *", []string{"gzip", "br"}, "br"},
		{"gzip-q0-x-gzip-offer", "gzip;q=0, *", []string{"x-gzip"}, ""},
		{"x-gzip-offer-accepted-by-gzip", "gzip", []string{"x-gzip"}, "x-gzip"},
		{"x-gzip-offer-accepted-by-x-gzip", "x-gzip", []string{"x-gzip", "br"}, "x-gzip"},
		{"alias-entries-are-one", "x-gzip;q=0.2, gzip;q=0.8, br;q=0.5", []string{"br", "gzip"}, "gzip"},
		{"alias-q0-wins", "x-gzip;q=0, gzip", []string{"gzip"}, ""},
		{"x-compress", "x-compress", []string{"compress"}, "compress"},
		{"x-compress-q0", "x-compress;q=0, *", []string{"compress", "gzip"}, "gzip"},
		// no alias for anything else, and none for a media type
		{"x-other-unchanged", "x-br", []string{"br"}, ""},
		{"media-unchanged", "text/x-gzip", []string{"text/gzip"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Accept(tc.header, tc.offers); got != tc.want {
				t.Errorf("Accept(%q, %q) = %q, want %q", tc.header, tc.offers, got, tc.want)
			}
		})
	}
}
