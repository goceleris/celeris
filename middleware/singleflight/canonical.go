package singleflight

import "strings"

// canonicalAppend appends to dst the canonical form of the value v of request
// header name (lowercase), the form the default key holds and the Vary
// comparison uses, and returns the extended slice. It is the one place that
// decides which values of a header are the same request (celeris#888 items 23
// and 24).
//
// Accept-Encoding is canonical without blanks and in lowercase: the
// encodings it names are case-insensitive tokens, and a blank around a comma,
// a ";" or a "=" is not significant, so "gzip, deflate, br" and
// "gzip,deflate,br" are one value (and so are "GZIP" and "gzip", and
// "gzip;q=0.8" and "gzip; q=0.8"). The order is kept, and so are empty list
// elements: two lists that differ in either are not folded together, which
// costs a missed coalescing, never a wrong response. Every other header keeps
// its value as the client sent it.
func canonicalAppend(dst []byte, name, v string) []byte {
	if name != "accept-encoding" {
		return append(dst, v...)
	}
	for i := 0; i < len(v); i++ {
		switch ch := v[i]; {
		case ch == ' ' || ch == '\t':
		case 'A' <= ch && ch <= 'Z':
			dst = append(dst, ch+('a'-'A'))
		default:
			dst = append(dst, ch)
		}
	}
	return dst
}

// canonicalValue is canonicalAppend as a string that shares nothing with v:
// on epoll and io_uring v is a view of the connection's receive buffer.
func canonicalValue(name, v string) string {
	if name != "accept-encoding" {
		return strings.Clone(v)
	}
	var buf [64]byte
	return string(canonicalAppend(buf[:0], name, v))
}

// sameCanonical reports whether v is, canonically, the value stored (a
// canonicalValue), without allocating.
func sameCanonical(name, stored, v string) bool {
	var buf [64]byte
	return string(canonicalAppend(buf[:0], name, v)) == stored
}
