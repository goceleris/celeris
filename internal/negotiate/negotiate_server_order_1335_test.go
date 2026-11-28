package negotiate

import "testing"

// TestAcceptServerOrder: the q-values weigh the offers as Accept weighs them,
// and of the offers with the highest q the first in offers wins, whatever the
// order of the header.
func TestAcceptServerOrder(t *testing.T) {
	zbg := []string{"zstd", "br", "gzip"}
	gbz := []string{"gzip", "br", "zstd"}
	for _, tc := range []struct {
		name, header string
		offers       []string
		want         string
	}{
		{"browser, zstd first", "gzip, deflate, br, zstd", zbg, "zstd"},
		{"browser, gzip first", "gzip, deflate, br, zstd", gbz, "gzip"},
		{"benchmark, zstd first", "gzip, br", zbg, "br"},
		{"benchmark, gzip first", "gzip, br", gbz, "gzip"},
		{"client order is no tie-break", "br, gzip", gbz, "gzip"},
		{"higher q wins over the offer order", "br;q=1.0, gzip;q=0.8", gbz, "br"},
		{"lower q loses", "gzip, br;q=0.5", []string{"br", "gzip"}, "gzip"},
		{"equal q below 1", "gzip;q=0.5, br;q=0.5", []string{"br", "gzip"}, "br"},
		{"star ties by offers", "*", gbz, "gzip"},
		{"star and a name tie by offers", "gzip, *", []string{"br", "gzip"}, "br"},
		{"star weighs only the unlisted", "gzip;q=0.5, *", gbz, "br"},
		{"refused is not offered", "gzip;q=0, br", gbz, "br"},
		{"all refused", "gzip;q=0, *;q=0", gbz, ""},
		{"nothing listed", "deflate", gbz, ""},
		{"empty header", "", gbz, ""},
		{"no offers", "gzip", nil, ""},
		{"x-gzip is gzip", "x-gzip, br", gbz, "gzip"},
	} {
		if got := AcceptServerOrder(tc.header, tc.offers); got != tc.want {
			t.Errorf("%s: AcceptServerOrder(%q, %v) = %q, want %q", tc.name, tc.header, tc.offers, got, tc.want)
		}
	}
}

// TestAcceptServerOrderSameQuality: Accept and AcceptServerOrder differ only
// in the tie. For each header both answer with an offer of the same q-value,
// the highest there is, and the server order's answer is the first offer that
// has it.
func TestAcceptServerOrderSameQuality(t *testing.T) {
	offers := []string{"zstd", "br", "gzip", "deflate"}
	quality := func(entries []AcceptItem, offer string) float64 {
		var specs [16]int8
		q, _ := offerQuality(entries, offer, specs[:])
		return q
	}
	for _, h := range []string{
		"gzip, deflate, br, zstd", "gzip, br", "br;q=1.0, gzip;q=0.8", "gzip, br;q=0.5",
		"*", "gzip;q=0, *", "identity;q=0", "gzip;q=0.9, br;q=0.9, zstd;q=0.1",
		"deflate;q=0.3, gzip;q=0.3, *;q=0.1", "gzip;q=0, br;q=0, zstd;q=0",
	} {
		entries := Parse(h)
		best := 0.0
		first := ""
		for _, o := range offers {
			if q := quality(entries, o); q > best {
				best, first = q, o
			}
		}
		a, s := Accept(h, offers), AcceptServerOrder(h, offers)
		if s != first {
			t.Errorf("%q: AcceptServerOrder %q, want the first offer of the best q: %q", h, s, first)
		}
		if quality(entries, a) != best && (a != "" || best != 0) {
			t.Errorf("%q: Accept %q has q %v, best is %v", h, a, quality(entries, a), best)
		}
	}
}
