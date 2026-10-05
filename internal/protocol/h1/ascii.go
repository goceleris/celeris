package h1

import "math"

// Byte-class lookup tables for header validation (RFC 9110 §5.1, §5.5). A
// table lookup keeps validation off the branch-per-char path in the parser's
// hottest loop.
//
// tcharTable[c] is true iff c is a valid token character (field-name char):
//
//	"!" / "#" / "$" / "%" / "&" / "'" / "*" / "+" / "-" / "." / "^" / "_" /
//	"`" / "|" / "~" / DIGIT / ALPHA
//
// fieldVCharTable[c] is true iff c is allowed in a field value: VCHAR
// (0x21–0x7E), obs-text (0x80–0xFF), SP (0x20), and HTAB (0x09). All other
// controls — notably NUL (0x00) and bare CR (0x0D) — are rejected.
var (
	tcharTable      [256]bool
	fieldVCharTable [256]bool
)

func init() {
	for c := 0; c < 256; c++ {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
			tcharTable[c] = true
		}
		switch byte(c) {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			tcharTable[c] = true
		}
		switch {
		case c == 0x09: // HTAB
			fieldVCharTable[c] = true
		case c == 0x20: // SP
			fieldVCharTable[c] = true
		case 0x21 <= c && c <= 0x7E: // VCHAR
			fieldVCharTable[c] = true
		case c >= 0x80: // obs-text
			fieldVCharTable[c] = true
		}
	}
}

// isValidHeaderName reports whether b is a non-empty all-tchar field-name.
func isValidHeaderName(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if !tcharTable[c] {
			return false
		}
	}
	return true
}

// isValidHeaderValue reports whether b contains only field-value-legal bytes
// (VCHAR / obs-text / SP / HTAB). Leading/trailing OWS having already been
// trimmed by the caller, this rejects embedded NUL, bare CR, and controls.
func isValidHeaderValue(b []byte) bool {
	for _, c := range b {
		if !fieldVCharTable[c] {
			return false
		}
	}
	return true
}

func asciiEqualFold(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := 0; i < len(b); i++ {
		cb := b[i]
		cs := s[i]
		if 'A' <= cb && cb <= 'Z' {
			cb |= 0x20
		}
		if 'A' <= cs && cs <= 'Z' {
			cs |= 0x20
		}
		if cb != cs {
			return false
		}
	}
	return true
}

func asciiContainsFoldString(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	n := len(s)
	m := len(sub)
	if m > n {
		return false
	}
	for i := 0; i <= n-m; i++ {
		match := true
		for j := 0; j < m; j++ {
			cs := s[i+j]
			ct := sub[j]
			if 'A' <= cs && cs <= 'Z' {
				cs |= 0x20
			}
			if 'A' <= ct && ct <= 'Z' {
				ct |= 0x20
			}
			if cs != ct {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func asciiContainsFoldBytes(b []byte, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	m := len(sub)
	if m > len(b) {
		return false
	}
	for i := 0; i <= len(b)-m; i++ {
		match := true
		for j := 0; j < m; j++ {
			cb := b[i+j]
			cs := sub[j]
			if 'A' <= cb && cb <= 'Z' {
				cb |= 0x20
			}
			if 'A' <= cs && cs <= 'Z' {
				cs |= 0x20
			}
			if cb != cs {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func parseInt64Bytes(b []byte) (int64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	var n int64
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		digit := int64(c - '0')
		if n > (math.MaxInt64-digit)/10 {
			return 0, false
		}
		n = n*10 + digit
	}
	return n, true
}

func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t') {
		start++
	}
	end := len(b)
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t') {
		end--
	}
	return b[start:end]
}
