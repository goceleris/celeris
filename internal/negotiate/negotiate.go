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
// but the original offer string is returned. Entries with q=0 are treated as
// "not acceptable" per RFC 9110 §12.5.1 and exclude matching offers even
// from wildcard entries.
func Accept(header string, offers []string) string {
	entries := Parse(header)

	// Build exclusion set: types with q=0 are explicitly rejected (RFC 9110).
	// These override wildcard matches — e.g., "*, br;q=0" excludes br.
	var excluded []string
	for _, e := range entries {
		if e.Quality <= 0 {
			excluded = append(excluded, e.MediaType)
		}
	}

	bestOffer := ""
	bestQ := -1.0
	bestIdx := len(entries) // higher = worse; prefer earlier Accept entries on tie
	for _, offer := range offers {
		lo := strings.ToLower(offer)
		// Check if this offer is explicitly excluded by a q=0 entry.
		if isExcluded(lo, excluded) {
			continue
		}
		for idx, e := range entries {
			if e.Quality <= 0 {
				continue
			}
			if !MatchMedia(e.MediaType, lo) {
				continue
			}
			if e.Quality > bestQ || (e.Quality == bestQ && idx < bestIdx) {
				bestQ = e.Quality
				bestOffer = offer
				bestIdx = idx
			}
		}
	}
	if bestQ <= 0 {
		return ""
	}
	return bestOffer
}

// isExcluded returns true if offer matches any excluded pattern via MatchMedia.
func isExcluded(offer string, excluded []string) bool {
	for _, ex := range excluded {
		if MatchMedia(ex, offer) {
			return true
		}
	}
	return false
}

// Parse parses an Accept header value into a slice of AcceptItems.
// MediaType values are lowercased for case-insensitive matching.
func Parse(header string) []AcceptItem {
	var entries []AcceptItem
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
				if strings.HasPrefix(p, "q=") {
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
