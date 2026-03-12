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
// Returns "" if no offer matches.
func Accept(header string, offers []string) string {
	entries := Parse(header)
	bestOffer := ""
	bestQ := -1.0
	bestIdx := len(entries) // higher = worse; prefer earlier Accept entries on tie
	for _, offer := range offers {
		for idx, e := range entries {
			if !MatchMedia(e.MediaType, offer) {
				continue
			}
			if e.Quality > bestQ || (e.Quality == bestQ && idx < bestIdx) {
				bestQ = e.Quality
				bestOffer = offer
				bestIdx = idx
			}
		}
	}
	if bestQ < 0 {
		return ""
	}
	return bestOffer
}

// Parse parses an Accept header value into a slice of AcceptItems.
func Parse(header string) []AcceptItem {
	var entries []AcceptItem
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		e := AcceptItem{Quality: 1.0}
		if i := strings.IndexByte(part, ';'); i >= 0 {
			params := part[i+1:]
			e.MediaType = strings.TrimSpace(part[:i])
			for _, p := range strings.Split(params, ";") {
				p = strings.TrimSpace(p)
				if strings.HasPrefix(p, "q=") {
					if q, err := strconv.ParseFloat(p[2:], 64); err == nil {
						e.Quality = q
					}
				}
			}
		} else {
			e.MediaType = part
		}
		e.MediaType = strings.TrimSpace(e.MediaType)
		if e.MediaType != "" {
			entries = append(entries, e)
		}
	}
	return entries
}

// MatchMedia returns true if the Accept pattern matches the offered media type.
// Supports wildcards: "*/*" matches everything, "text/*" matches any text subtype.
func MatchMedia(pattern, offer string) bool {
	if pattern == "*/*" {
		return true
	}
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
