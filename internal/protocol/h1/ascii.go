package h1

import (
	"encoding/binary"
	"math"
)

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
//
// targetBadTable[c] is true iff c may not appear in a request-target: a control
// (0x00-0x1F, notably NUL, CR and LF, and HTAB) or DEL (0x7F). SP cannot occur
// there (the request line is split at it). A request-target is a URI
// reference (RFC 3986), which has no control character, and RFC 9112 section
// 3 has a recipient answer a request line it cannot parse with 400.
var (
	tcharTable      [256]bool
	fieldVCharTable [256]bool
	targetBadTable  [256]bool
)

func init() {
	for c := 0; c < 256; c++ {
		targetBadTable[c] = c < 0x20 || c == 0x7F
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

// isValidRequestTarget reports whether b holds no byte of targetBadTable. It
// reads eight bytes at a time and looks at single bytes only for a word that
// has a byte below 0x20 or a 0x7F (a bit-twiddling test with no false
// negative), because every request pays for this scan.
func isValidRequestTarget(b []byte) bool {
	const (
		ones = 0x0101010101010101
		high = 0x8080808080808080
	)
	i := 0
	for ; i+8 <= len(b); i += 8 {
		w := binary.LittleEndian.Uint64(b[i:])
		x := w ^ (0x7F * ones) // a 0x7F byte of w is a zero byte of x
		if ((w-0x20*ones)&^w|(x-ones)&^x)&high != 0 {
			for _, c := range b[i : i+8] {
				if targetBadTable[c] {
					return false
				}
			}
		}
	}
	for ; i < len(b); i++ {
		if targetBadTable[b[i]] {
			return false
		}
	}
	return true
}

// isToken reports whether b is a non-empty all-tchar token (RFC 9110 section
// 5.6.2), the grammar of a request method.
func isToken(b []byte) bool { return isValidHeaderName(b) }

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
