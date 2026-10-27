package singleflight

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/negotiate"
)

// The negotiator reads at most 64 entries of an Accept-Encoding (celeris#888
// item 42), and the canonical form is the negotiator's reading, so two values
// that differ only after the 64th entry are one key, and they are served alike
// by the negotiator: a waiter never gets an encoding its own run would not
// give. Values that differ within the first 64 entries stay apart.
func TestCanonicalAcceptEncodingFollowsTheNegotiatorsEntryCap888(t *testing.T) {
	pad := strings.Repeat("x,", 70) // 140 bytes, under maxCanonicalAE
	t.Run("differ-past-the-cap", func(t *testing.T) {
		a, b := pad+"gzip", pad+"br"
		if negotiate.Accept(a, []string{"gzip"}) != negotiate.Accept(b, []string{"gzip"}) {
			t.Fatal("the negotiator tells the two values apart: the test premise is wrong")
		}
		r := runPairOpts(t, New(), negotiatingReal, "/doc", aeOpt(a), "/doc", aeOpt(b))
		if !r.joined || r.calls != 1 {
			t.Errorf("values equal for the negotiator: joined=%v runs=%d, want one shared run", r.joined, r.calls)
		}
	})
	t.Run("differ-within-the-cap", func(t *testing.T) {
		a, b := "gzip,"+pad, "br,"+pad
		r := runPairOpts(t, New(), negotiatingReal, "/doc", aeOpt(a), "/doc", aeOpt(b))
		if r.joined || r.calls != 2 {
			t.Errorf("values the negotiator tells apart: joined=%v runs=%d, want two runs", r.joined, r.calls)
		}
		if got, want := string(r.second.Body), "identity-body"; got != want {
			t.Errorf("waiter body %q, want %q", got, want)
		}
	})
}
