package h1

import "math"

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
