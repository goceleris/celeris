package negotiate

import (
	"math/rand"
	"strings"
	"testing"
)

// acceptRef is Accept written the plain way, per the doc of Accept: the q-value
// of an offer is that of the most specific entries matching it (0 if any of
// them says 0, else the highest), offers are ranked by that q-value, then by
// the earliest entry that matches the offer and carries it, then by their
// order in offers. Accept's scratch buffer of specificities must give the
// same answers, past its length too.
func acceptRef(header string, offers []string) string {
	entries := Parse(header)
	best, bestQ, bestIdx := "", 0.0, 0
	for _, offer := range offers {
		lo := strings.ToLower(offer)
		spec, q := 0, 0.0
		for _, e := range entries {
			s := matchSpecificity(e.MediaType, lo)
			if s == 0 {
				continue
			}
			switch {
			case s > spec:
				spec, q = s, e.Quality
			case s == spec && (e.Quality <= 0 || (q > 0 && e.Quality > q)):
				q = e.Quality
			}
		}
		if q <= 0 {
			continue
		}
		idx := -1
		for i, e := range entries {
			if e.Quality == q && MatchMedia(e.MediaType, lo) {
				idx = i
				break
			}
		}
		if best == "" || q > bestQ || (q == bestQ && idx < bestIdx) {
			best, bestQ, bestIdx = offer, q, idx
		}
	}
	return best
}

func TestAcceptMatchesReference888(t *testing.T) {
	rng := rand.New(rand.NewSource(888))
	names := []string{"gzip", "br", "zstd", "deflate", "identity", "*", "x-gzip", "GZIP", "Br"}
	weights := []string{"", "", ";q=0", ";q=1", ";q=0.5", ";q=0.1", ";Q=0.5", ";q=0.9"}
	offers := []string{"gzip", "br", "zstd", "deflate", "identity"}
	for range 20000 {
		n := 1 + rng.Intn(40) // past the 8-entry parse buffer and the 16 of the specificity scratch
		var sb strings.Builder
		for i := 0; i < n; i++ {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(names[rng.Intn(len(names))])
			sb.WriteString(weights[rng.Intn(len(weights))])
		}
		h := sb.String()
		rng.Shuffle(len(offers), func(i, j int) { offers[i], offers[j] = offers[j], offers[i] })
		o := offers[:1+rng.Intn(len(offers))]
		if got, want := Accept(h, o), acceptRef(h, o); got != want {
			t.Fatalf("Accept(%q, %q) = %q, the reference says %q", h, o, got, want)
		}
	}
}

// Media ranges, which mix the three specificities.
func TestAcceptMatchesReferenceMedia888(t *testing.T) {
	rng := rand.New(rand.NewSource(889))
	names := []string{"text/html", "text/*", "*/*", "application/json", "application/*", "text/plain", "Text/HTML", "*"}
	weights := []string{"", ";q=0", ";q=0.8", ";q=0.3", ";Q=0.8"}
	offers := []string{"text/html", "text/plain", "application/json", "application/xml", "image/png"}
	for range 20000 {
		n := 1 + rng.Intn(25)
		var sb strings.Builder
		for i := 0; i < n; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(names[rng.Intn(len(names))])
			sb.WriteString(weights[rng.Intn(len(weights))])
		}
		h := sb.String()
		rng.Shuffle(len(offers), func(i, j int) { offers[i], offers[j] = offers[j], offers[i] })
		o := offers[:1+rng.Intn(len(offers))]
		if got, want := Accept(h, o), acceptRef(h, o); got != want {
			t.Fatalf("Accept(%q, %q) = %q, the reference says %q", h, o, got, want)
		}
	}
}
