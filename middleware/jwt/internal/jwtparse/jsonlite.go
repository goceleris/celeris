package jwtparse

import (
	"strconv"
	"unicode/utf8"
)

// parseHeader extracts alg, kid, typ from a JOSE header.
// Uses byte-level key matching to avoid string allocations for keys.
func parseHeader(data []byte, h *Header) error {
	p := &jsonParser{data: data}
	p.skipWS()
	if !p.consume('{') {
		return errJSON
	}
	p.skipWS()
	if c, ok := p.peek(); ok && c == '}' {
		p.pos++
		return nil
	}
	for {
		p.skipWS()
		// Identify key by raw bytes (no string alloc).
		keyID := p.matchHeaderKey()
		if !p.consume(':') {
			return errJSON
		}
		p.skipWS()
		if keyID > 0 {
			val, err := p.parseHeaderValue()
			if err != nil {
				return err
			}
			switch keyID {
			case 1:
				h.Alg = val
			case 2:
				h.Kid = val
			case 3:
				h.Typ = val
			}
		} else {
			p.skipValue()
		}
		p.skipWS()
		if p.consume('}') {
			return nil
		}
		if !p.consume(',') {
			return errJSON
		}
	}
}

// parseHeaderValue reads a JSON string value and interns well-known values
// to avoid heap allocation.
func (p *jsonParser) parseHeaderValue() (string, error) {
	if !p.consume('"') {
		return "", errJSON
	}
	start := p.pos
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if c == '\\' {
			// Escape found; fall back to full parseString-style handling.
			p.pos = start - 1 // rewind to opening quote
			return p.parseString()
		}
		if c == '"' {
			seg := p.data[start:p.pos]
			p.pos++
			// Intern well-known header values to avoid allocating.
			if s, ok := internHeaderString(seg); ok {
				return s, nil
			}
			return string(seg), nil
		}
		p.pos++
	}
	return "", errJSON
}

// matchHeaderKey reads a JSON string key and returns an ID:
// 1=alg, 2=kid, 3=typ, 0=unknown. Avoids allocating a Go string.
func (p *jsonParser) matchHeaderKey() int {
	if !p.consume('"') {
		return -1
	}
	start := p.pos
	for p.pos < len(p.data) && p.data[p.pos] != '"' {
		if p.data[p.pos] == '\\' {
			p.pos++
		}
		p.pos++
	}
	end := p.pos
	if p.pos < len(p.data) {
		p.pos++ // consume closing '"'
	}
	seg := p.data[start:end]
	if len(seg) == 3 {
		if seg[0] == 'a' && seg[1] == 'l' && seg[2] == 'g' {
			return 1
		}
		if seg[0] == 'k' && seg[1] == 'i' && seg[2] == 'd' {
			return 2
		}
		if seg[0] == 't' && seg[1] == 'y' && seg[2] == 'p' {
			return 3
		}
	}
	return 0
}

// internHeaderString returns a pre-allocated string constant for well-known JWT header values.
func internHeaderString(b []byte) (string, bool) {
	switch len(b) {
	case 2:
		if b[0] == 'a' && b[1] == 't' {
			return "at", true // typ: at+jwt
		}
	case 3:
		if b[0] == 'J' && b[1] == 'W' && b[2] == 'T' {
			return "JWT", true
		}
	case 4:
		if b[0] == 'N' && b[1] == 'o' && b[2] == 'n' && b[3] == 'e' {
			return "None", true
		}
		if b[0] == 'n' && b[1] == 'o' && b[2] == 'n' && b[3] == 'e' {
			return "none", true
		}
	case 5:
		if b[0] == 'H' && b[1] == 'S' {
			switch {
			case b[2] == '2' && b[3] == '5' && b[4] == '6':
				return "HS256", true
			case b[2] == '3' && b[3] == '8' && b[4] == '4':
				return "HS384", true
			case b[2] == '5' && b[3] == '1' && b[4] == '2':
				return "HS512", true
			}
		}
		if b[0] == 'R' && b[1] == 'S' {
			switch {
			case b[2] == '2' && b[3] == '5' && b[4] == '6':
				return "RS256", true
			case b[2] == '3' && b[3] == '8' && b[4] == '4':
				return "RS384", true
			case b[2] == '5' && b[3] == '1' && b[4] == '2':
				return "RS512", true
			}
		}
		if b[0] == 'P' && b[1] == 'S' {
			switch {
			case b[2] == '2' && b[3] == '5' && b[4] == '6':
				return "PS256", true
			case b[2] == '3' && b[3] == '8' && b[4] == '4':
				return "PS384", true
			case b[2] == '5' && b[3] == '1' && b[4] == '2':
				return "PS512", true
			}
		}
		if b[0] == 'E' && b[1] == 'S' {
			switch {
			case b[2] == '2' && b[3] == '5' && b[4] == '6':
				return "ES256", true
			case b[2] == '3' && b[3] == '8' && b[4] == '4':
				return "ES384", true
			case b[2] == '5' && b[3] == '1' && b[4] == '2':
				return "ES512", true
			}
		}
		if b[0] == 'E' && b[1] == 'd' && b[2] == 'D' && b[3] == 'S' && b[4] == 'A' {
			return "EdDSA", true
		}
	}
	return "", false
}

// skipValue skips a JSON value at the current position.
// Tolerates unterminated strings/structures at EOF (the caller's
// enclosing parse loop will detect the structural error).
func (p *jsonParser) skipValue() {
	p.skipWS()
	if p.pos >= len(p.data) {
		return
	}
	c := p.data[p.pos]
	switch c {
	case '"':
		p.pos++
		for p.pos < len(p.data) {
			switch p.data[p.pos] {
			case '\\':
				p.pos++
				if p.pos < len(p.data) {
					p.pos++
				}
			case '"':
				p.pos++
				return
			default:
				p.pos++
			}
		}
	case '{':
		p.skipBraced('{', '}')
	case '[':
		p.skipBraced('[', ']')
	default:
		for p.pos < len(p.data) {
			b := p.data[p.pos]
			if b == ',' || b == '}' || b == ']' {
				return
			}
			p.pos++
		}
	}
}

func (p *jsonParser) skipBraced(open, closeByte byte) {
	depth := 1
	p.pos++
	for p.pos < len(p.data) && depth > 0 {
		c := p.data[p.pos]
		if c == open {
			depth++
		} else if c == closeByte {
			depth--
		} else if c == '"' {
			p.pos++
			for p.pos < len(p.data) {
				if p.data[p.pos] == '\\' {
					p.pos++
					if p.pos < len(p.data) {
						p.pos++
					}
				} else if p.data[p.pos] == '"' {
					p.pos++
					break
				} else {
					p.pos++
				}
			}
			continue
		}
		p.pos++
	}
}

// parseMapClaims is a minimal JSON object parser that populates a MapClaims
// with zero unnecessary allocations. It supports the JSON types needed for JWT:
// strings, numbers (as float64), booleans, null, arrays of strings, and nested objects (as map[string]any).
// It is NOT a general-purpose JSON parser.
func parseMapClaims(data []byte, m MapClaims) error {
	p := &jsonParser{data: data}
	p.skipWS()
	if !p.consume('{') {
		return errJSON
	}
	return p.parseObject(m)
}

var errJSON = ErrTokenMalformed

// maxJSONDepth is the maximum nesting depth for JSON parsing.
const maxJSONDepth = 32

type jsonParser struct {
	data  []byte
	pos   int
	depth int
}

func (p *jsonParser) peek() (byte, bool) {
	if p.pos >= len(p.data) {
		return 0, false
	}
	return p.data[p.pos], true
}

func (p *jsonParser) consume(expected byte) bool {
	p.skipWS()
	if p.pos >= len(p.data) || p.data[p.pos] != expected {
		return false
	}
	p.pos++
	return true
}

func (p *jsonParser) skipWS() {
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.pos++
		} else {
			return
		}
	}
}

// parseObject parses the interior of a JSON object (after the opening '{') into m.
func (p *jsonParser) parseObject(m map[string]any) error {
	p.depth++
	if p.depth > maxJSONDepth {
		return errJSON
	}
	defer func() { p.depth-- }()

	p.skipWS()
	if c, ok := p.peek(); ok && c == '}' {
		p.pos++
		return nil
	}

	for {
		p.skipWS()
		key, err := p.parseInternedString()
		if err != nil {
			return err
		}

		if !p.consume(':') {
			return errJSON
		}

		p.skipWS()
		val, err := p.parseValue()
		if err != nil {
			return err
		}
		m[key] = val

		p.skipWS()
		if p.consume('}') {
			return nil
		}
		if !p.consume(',') {
			return errJSON
		}
	}
}

// parseInternedString parses a JSON string, interning well-known JWT claim keys.
func (p *jsonParser) parseInternedString() (string, error) {
	if !p.consume('"') {
		return "", errJSON
	}
	start := p.pos
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if c == '\\' {
			// Has escapes -- fall back to full parser.
			p.pos = start - 1
			return p.parseString()
		}
		if c == '"' {
			seg := p.data[start:p.pos]
			p.pos++
			if s, ok := internClaimKey(seg); ok {
				return s, nil
			}
			return string(seg), nil
		}
		p.pos++
	}
	return "", errJSON
}

func internClaimKey(b []byte) (string, bool) {
	switch len(b) {
	case 2:
		if b[0] == 'a' && b[1] == 't' {
			return "at", true
		}
	case 3:
		if b[0] == 's' && b[1] == 'u' && b[2] == 'b' {
			return "sub", true
		}
		if b[0] == 'e' && b[1] == 'x' && b[2] == 'p' {
			return "exp", true
		}
		if b[0] == 'n' && b[1] == 'b' && b[2] == 'f' {
			return "nbf", true
		}
		if b[0] == 'i' && b[1] == 'a' && b[2] == 't' {
			return "iat", true
		}
		if b[0] == 'i' && b[1] == 's' && b[2] == 's' {
			return "iss", true
		}
		if b[0] == 'a' && b[1] == 'u' && b[2] == 'd' {
			return "aud", true
		}
		if b[0] == 'j' && b[1] == 't' && b[2] == 'i' {
			return "jti", true
		}
		if b[0] == 'a' && b[1] == 'z' && b[2] == 'p' {
			return "azp", true
		}
	case 4:
		if b[0] == 'n' && b[1] == 'a' && b[2] == 'm' && b[3] == 'e' {
			return "name", true
		}
		if b[0] == 'r' && b[1] == 'o' && b[2] == 'l' && b[3] == 'e' {
			return "role", true
		}
	case 5:
		if b[0] == 'e' && b[1] == 'm' && b[2] == 'a' && b[3] == 'i' && b[4] == 'l' {
			return "email", true
		}
		if b[0] == 's' && b[1] == 'c' && b[2] == 'o' && b[3] == 'p' && b[4] == 'e' {
			return "scope", true
		}
		if b[0] == 'r' && b[1] == 'o' && b[2] == 'l' && b[3] == 'e' && b[4] == 's' {
			return "roles", true
		}
	}
	return "", false
}

func (p *jsonParser) parseValue() (any, error) {
	p.skipWS()
	c, ok := p.peek()
	if !ok {
		return nil, errJSON
	}

	switch {
	case c == '"':
		return p.parseString()
	case c == '{':
		p.pos++
		nested := make(map[string]any)
		if err := p.parseObject(nested); err != nil {
			return nil, err
		}
		return nested, nil
	case c == '[':
		return p.parseArray()
	case c == 't':
		return p.parseLiteral("true", true)
	case c == 'f':
		return p.parseLiteral("false", false)
	case c == 'n':
		return p.parseLiteral("null", nil)
	case c == '-' || (c >= '0' && c <= '9'):
		return p.parseNumber()
	default:
		return nil, errJSON
	}
}

func (p *jsonParser) parseString() (string, error) {
	if !p.consume('"') {
		return "", errJSON
	}
	start := p.pos
	hasEscape := false

	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if c == '\\' {
			hasEscape = true
			p.pos++
			if p.pos >= len(p.data) {
				return "", errJSON
			}
			p.pos++
			continue
		}
		if c == '"' {
			s := string(p.data[start:p.pos])
			p.pos++
			if hasEscape {
				return unescapeJSON(s)
			}
			return s, nil
		}
		// Reject bare control characters (U+0000-U+001F) per RFC 8259 section 7.
		if c < 0x20 {
			return "", errJSON
		}
		p.pos++
	}
	return "", errJSON
}

func (p *jsonParser) parseNumber() (float64, error) {
	start := p.pos

	// Optional leading minus.
	if p.pos < len(p.data) && p.data[p.pos] == '-' {
		p.pos++
	}

	// Integer part: '0' or non-zero digit followed by more digits.
	if p.pos >= len(p.data) || p.data[p.pos] < '0' || p.data[p.pos] > '9' {
		return 0, errJSON
	}
	if p.data[p.pos] == '0' {
		p.pos++
		// Leading zero must not be followed by another digit (e.g. 01).
		if p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			return 0, errJSON
		}
	} else {
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
	}

	// Optional fraction.
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		p.pos++
		if p.pos >= len(p.data) || p.data[p.pos] < '0' || p.data[p.pos] > '9' {
			return 0, errJSON
		}
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
	}

	// Optional exponent.
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		if p.pos >= len(p.data) || p.data[p.pos] < '0' || p.data[p.pos] > '9' {
			return 0, errJSON
		}
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
	}

	if p.pos == start {
		return 0, errJSON
	}
	return strconv.ParseFloat(string(p.data[start:p.pos]), 64)
}

func (p *jsonParser) parseArray() (any, error) {
	p.depth++
	if p.depth > maxJSONDepth {
		return nil, errJSON
	}
	defer func() { p.depth-- }()

	p.pos++ // consume '['
	p.skipWS()

	if c, ok := p.peek(); ok && c == ']' {
		p.pos++
		return []any{}, nil
	}

	var arr []any
	for {
		p.skipWS()
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)

		p.skipWS()
		if p.consume(']') {
			return arr, nil
		}
		if !p.consume(',') {
			return nil, errJSON
		}
	}
}

func (p *jsonParser) parseLiteral(lit string, val any) (any, error) {
	end := p.pos + len(lit)
	if end > len(p.data) {
		return nil, errJSON
	}
	if string(p.data[p.pos:end]) != lit {
		return nil, errJSON
	}
	p.pos = end
	return val, nil
}

// unescapeJSON handles JSON string escape sequences.
func unescapeJSON(s string) (string, error) {
	buf := make([]byte, 0, len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '\\' {
			buf = append(buf, c)
			i++
			continue
		}
		i++
		if i >= len(s) {
			return "", errJSON
		}
		switch s[i] {
		case '"', '\\', '/':
			buf = append(buf, s[i])
		case 'b':
			buf = append(buf, '\b')
		case 'f':
			buf = append(buf, '\f')
		case 'n':
			buf = append(buf, '\n')
		case 'r':
			buf = append(buf, '\r')
		case 't':
			buf = append(buf, '\t')
		case 'u':
			if i+4 >= len(s) {
				return "", errJSON
			}
			r, err := strconv.ParseUint(s[i+1:i+5], 16, 32)
			if err != nil {
				return "", errJSON
			}
			i += 4
			// Handle UTF-16 surrogate pairs: \uD800-\uDBFF followed by \uDC00-\uDFFF.
			if r >= 0xD800 && r <= 0xDBFF {
				if i+6 < len(s) && s[i+1] == '\\' && s[i+2] == 'u' {
					lo, err2 := strconv.ParseUint(s[i+3:i+7], 16, 32)
					if err2 != nil {
						return "", errJSON
					}
					if lo >= 0xDC00 && lo <= 0xDFFF {
						r = 0x10000 + (r-0xD800)*0x400 + (lo - 0xDC00)
						i += 6
					} else {
						return "", errJSON
					}
				} else {
					return "", errJSON
				}
			} else if r >= 0xDC00 && r <= 0xDFFF {
				// Lone low surrogate is invalid.
				return "", errJSON
			}
			var ubuf [4]byte
			n := utf8.EncodeRune(ubuf[:], rune(r))
			buf = append(buf, ubuf[:n]...)
		default:
			return "", errJSON
		}
		i++
	}
	return string(buf), nil
}
