// Package negotiate provides HTTP content negotiation utilities.
package negotiate

import (
	"strconv"
	"strings"
)

// AcceptItem represents a parsed Accept header entry with media type and quality.
type AcceptItem struct {
	MediaType string
	Quality   float64
}

// Accept returns the best matching offer based on the Accept header value.
// Returns "" if no offer matches. Matching is case-insensitive per RFC 9110,
// but the original offer string is returned.
//
// The q-value of an offer is the one of the most specific entry that matches
// it: an entry naming the offer, else a "type/*" range, else "*" or "*/*"
// (RFC 9110 12.5.1; for a content coding or a language, 12.5.3 says the same:
// "*" matches whatever is not listed elsewhere in the field). So "gzip, *;q=0"
// accepts gzip and refuses every other coding, "text/html;q=0, text/*" refuses
// text/html and accepts the rest of text, and "gzip;q=0.5, *" weighs gzip
// 0.5 and the rest 1. An entry with q=0 means "not acceptable"
// (RFC 9110 12.5.1): the offer it decides is not returned, even when a less
// specific entry would accept it. Where two entries are equally specific (a
// repeated entry), q=0 wins, and otherwise the higher q-value does.
//
// Of the acceptable offers the highest q-value is chosen, then the one whose
// entry comes first in the header, then the first offer.
func Accept(header string, offers []string) string {
	// Stack-backed entry buffer. Real Accept-Encoding / Accept-Language
	// headers almost always hold ≤8 items; anything larger falls back to
	// heap-allocated growth. Saves one alloc on the hot path of every
	// request that passes through a compression or language-negotiation
	// middleware.
	var entryBuf [8]AcceptItem
	entries := ParseInto(entryBuf[:0], header)

	bestOffer := ""
	bestQ := -1.0
	bestIdx := len(entries) // higher = worse; prefer earlier Accept entries on tie
	for _, offer := range offers {
		lo := strings.ToLower(offer)
		q, idx := offerQuality(entries, lo)
		if q <= 0 {
			continue
		}
		if q > bestQ || (q == bestQ && idx < bestIdx) {
			bestQ = q
			bestOffer = offer
			bestIdx = idx
		}
	}
	if bestQ <= 0 {
		return ""
	}
	return bestOffer
}

// offerQuality returns the q-value of the lowercased offer and the index of
// the entry that gave it: that of the most specific entries matching the offer
// (see [Accept]), q=0 if any of them says 0, else the highest, the earliest on
// a tie. It returns q=0 when no entry matches.
func offerQuality(entries []AcceptItem, offer string) (q float64, idx int) {
	bestSpec := 0
	idx = len(entries)
	for i, e := range entries {
		spec := matchSpecificity(e.MediaType, offer)
		if spec == 0 || spec < bestSpec {
			continue
		}
		switch {
		case spec > bestSpec:
			bestSpec, q, idx = spec, e.Quality, i
		case q <= 0:
			// An earlier equally specific entry refused it: that stands.
		case e.Quality <= 0:
			q, idx = e.Quality, i
		case e.Quality > q:
			q, idx = e.Quality, i
		}
	}
	return q, idx
}

// matchSpecificity is 0 when the pattern does not match the offer, and
// otherwise how specific the pattern is: 1 for "*" and "*/*", 2 for "type/*",
// 3 for a name or a full type. Both are lowercase.
func matchSpecificity(pattern, offer string) int {
	if !MatchMedia(pattern, offer) {
		return 0
	}
	switch {
	case pattern == "*" || pattern == "*/*":
		return 1
	case strings.HasSuffix(pattern, "/*"):
		return 2
	default:
		return 3
	}
}

// Parse parses an Accept header value into a slice of AcceptItems.
// MediaType values are lowercased for case-insensitive matching.
func Parse(header string) []AcceptItem {
	return ParseInto(nil, header)
}

// ParseInto is the alloc-friendly variant of [Parse]. Callers can pass
// a pre-sized slice (e.g. a stack-backed `make([]AcceptItem, 0, 8)[:0]`
// or the zero value of an array via `buf[:0]`) to avoid the fresh
// allocation when the expected number of entries is small. If the
// header yields more entries than dst's capacity, it grows naturally.
func ParseInto(dst []AcceptItem, header string) []AcceptItem {
	entries := dst
	for len(header) > 0 {
		var part string
		if i := strings.IndexByte(header, ','); i >= 0 {
			part = header[:i]
			header = header[i+1:]
		} else {
			part = header
			header = ""
		}
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		e := AcceptItem{Quality: 1.0}
		if i := strings.IndexByte(part, ';'); i >= 0 {
			params := part[i+1:]
			e.MediaType = strings.TrimSpace(part[:i])
			for len(params) > 0 {
				var p string
				if j := strings.IndexByte(params, ';'); j >= 0 {
					p = params[:j]
					params = params[j+1:]
				} else {
					p = params
					params = ""
				}
				p = strings.TrimSpace(p)
				// The parameter name is case-insensitive (RFC 9110 5.6.6, 12.4.2).
				if len(p) > 2 && (p[0] == 'q' || p[0] == 'Q') && p[1] == '=' {
					if q, err := strconv.ParseFloat(p[2:], 64); err == nil {
						e.Quality = q
					}
				}
			}
		} else {
			e.MediaType = part
		}
		e.MediaType = strings.ToLower(strings.TrimSpace(e.MediaType))
		if e.MediaType != "" {
			entries = append(entries, e)
		}
	}
	return entries
}

// MatchMedia returns true if the Accept pattern matches the offered media type.
// Supports wildcards: "*" or "*/*" matches everything, "text/*" matches any text subtype.
// Comparison is case-insensitive per RFC 9110.
func MatchMedia(pattern, offer string) bool {
	if pattern == "*" || pattern == "*/*" {
		return true
	}
	// Defensive: internal callers (Accept, isExcluded) pass pre-lowered
	// values, but MatchMedia is exported and may receive mixed-case input.
	pattern = strings.ToLower(pattern)
	offer = strings.ToLower(offer)
	pSlash := strings.IndexByte(pattern, '/')
	oSlash := strings.IndexByte(offer, '/')
	if pSlash < 0 || oSlash < 0 {
		return pattern == offer
	}
	pType := pattern[:pSlash]
	pSub := pattern[pSlash+1:]
	oType := offer[:oSlash]
	oSub := offer[oSlash+1:]
	if pType != oType {
		return false
	}
	if pSub == "*" {
		return true
	}
	return pSub == oSub
}
