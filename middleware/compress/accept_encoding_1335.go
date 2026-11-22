package compress

import (
	"strconv"
	"strings"
)

// This file is the negotiator of the middleware, for content codings only. It
// is a copy of the weighing of internal/negotiate (Accept, offerQuality,
// ParseInto) with one change, the tie: of the codings with the highest
// q-value the first in the server's order wins, where Context.AcceptsEncodings
// ranks equal q-values by the client's header (celeris#1335). It is a copy and
// does not import the core's internal package because this module cannot
// import an internal package of the core that older releases lack (see
// compressibleKey). The differential test and fuzz target of
// encodings_differential_1335_test.go pin it to internal/negotiate's
// AcceptServerOrder, which is the same rule written against the core.

// maxAEEntries bounds what parseAcceptEncoding reads of a header (the first 64
// entries) and maxAESegments the list elements it walks, empty ones included,
// as internal/negotiate does.
const (
	maxAEEntries  = 64
	maxAESegments = 4 * maxAEEntries
)

// aeEntry is one entry of an Accept-Encoding header.
type aeEntry struct {
	coding string  // lowercase, "x-gzip" read as "gzip"
	q      float64 // the weight, 1 when the entry has none
	params bool    // a parameter other than the weight: ranks one notch below
}

// negotiateEncoding returns the coding of offers the header accepts with the
// highest q-value, the first in offers among those that tie, and "" if none
// is accepted.
//
// The q-value of an offer is the one of the most specific entry that matches
// it: the entry naming it, else "*" (RFC 9110 12.5.3, so "gzip, *;q=0" accepts
// gzip and nothing else, and "gzip;q=0.5, *" weighs gzip 0.5 and the rest 1);
// an entry with a parameter other than the weight ranks below the same entry
// without one but above "*". q=0 refuses, whatever less specific entry would
// accept; of two equally specific entries (a repeated coding) q=0 wins, and
// otherwise the higher q-value does.
func negotiateEncoding(header string, offers []string) string {
	var buf [8]aeEntry
	entries := parseAcceptEncoding(buf[:0], header)
	best := ""
	bestQ := 0.0
	for _, offer := range offers {
		// Strictly greater: an earlier offer keeps a tie.
		if q := encodingQuality(entries, aeCoding(strings.ToLower(offer))); q > bestQ {
			bestQ, best = q, offer
		}
	}
	return best
}

// encodingQuality is the q-value of the lowercase offer: that of the most
// specific entries matching it, 0 if any of them says 0, else the highest; 0
// when no entry matches.
func encodingQuality(entries []aeEntry, offer string) (q float64) {
	bestSpec := 0
	for _, e := range entries {
		spec := 0
		switch {
		case e.coding == "*" || e.coding == "*/*":
			spec = 2
		case e.coding == offer:
			spec = 6
		}
		if spec == 0 {
			continue
		}
		if e.params {
			spec--
		}
		if spec < bestSpec {
			continue
		}
		switch {
		case spec > bestSpec:
			bestSpec, q = spec, e.q
		case q <= 0:
			// An earlier equally specific entry refused it: that stands.
		case e.q <= 0, e.q > q:
			q = e.q
		}
	}
	return q
}

// parseAcceptEncoding appends to dst the entries of header (at most 64, from
// at most 256 list elements). A weight outside the grammar of RFC 9110 12.4.2
// ("NaN", "Inf", "1e5", "-1", ".5", "1.5") is no weight and the entry keeps q=1,
// except one that reads as zero or below ("-1", "0e0", "-Inf"), which stays a
// refusal.
func parseAcceptEncoding(dst []aeEntry, header string) []aeEntry {
	kept := 0
	for segments := 0; len(header) > 0 && kept < maxAEEntries && segments < maxAESegments; segments++ {
		var part string
		if i := strings.IndexByte(header, ','); i >= 0 {
			part, header = header[:i], header[i+1:]
		} else {
			part, header = header, ""
		}
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		e := aeEntry{q: 1.0}
		name := part
		if i := strings.IndexByte(part, ';'); i >= 0 {
			name = strings.TrimSpace(part[:i])
			params := part[i+1:]
			for len(params) > 0 {
				var p string
				if j := strings.IndexByte(params, ';'); j >= 0 {
					p, params = params[:j], params[j+1:]
				} else {
					p, params = params, ""
				}
				p = strings.TrimSpace(p)
				if p != "" && !isWeightParam(p) {
					e.params = true
				}
				if len(p) > 2 && (p[0] == 'q' || p[0] == 'Q') && p[1] == '=' {
					if q, ok := parseQValue(p[2:]); ok {
						e.q = q
					} else if f, err := strconv.ParseFloat(p[2:], 64); err == nil && f <= 0 {
						e.q = 0
					}
				}
			}
		}
		e.coding = aeCoding(strings.ToLower(strings.TrimSpace(name)))
		if e.coding != "" {
			dst = append(dst, e)
			kept++
		}
	}
	return dst
}

// isWeightParam reports whether the parameter p is named "q", whatever its
// value: it is the weight, well formed or not, and not a parameter of the coding.
func isWeightParam(p string) bool {
	name := p
	if i := strings.IndexByte(p, '='); i >= 0 {
		name = p[:i]
	}
	return strings.EqualFold(strings.TrimSpace(name), "q")
}

// parseQValue reads a weight: RFC 9110 12.4.2, qvalue = ( "0" [ "." 0*3DIGIT ] )
// / ( "1" [ "." 0*3("0") ] ), lenient on the count of digits after the point.
// ok is false for anything else.
func parseQValue(s string) (q float64, ok bool) {
	if s == "" || (s[0] != '0' && s[0] != '1') {
		return 0, false
	}
	if len(s) > 1 {
		if s[1] != '.' {
			return 0, false
		}
		for i := 2; i < len(s); i++ {
			if c := s[i]; c < '0' || c > '9' || (s[0] == '1' && c != '0') {
				return 0, false
			}
		}
	}
	q, err := strconv.ParseFloat(s, 64)
	return q, err == nil
}

// aeCoding folds the deprecated "x-gzip" into "gzip" (RFC 9110 8.4.1.3), in
// the header and in the offers alike ("x-compress" too, for parity with the
// core).
func aeCoding(name string) string {
	switch name {
	case "x-gzip":
		return "gzip"
	case "x-compress":
		return "compress"
	}
	return name
}
