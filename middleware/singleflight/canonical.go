package singleflight

import (
	"strconv"
	"strings"

	"github.com/goceleris/celeris/internal/negotiate"
)

// canonicalAppend appends to dst the canonical form of the value v of request
// header name (lowercase), the form the default key holds and the Vary
// comparison uses, and returns the extended slice. It is the one place that
// decides which values of a header are the same request (celeris#888 items 23
// and 24).
//
// Accept-Encoding is canonical as the negotiator reads it: the list that
// internal/negotiate.ParseInto makes of it (what c.AcceptsEncodings, and so
// compress, uses), written as the lowercase coding and, unless it is 1, its
// weight, entries separated by ",", in order. So two values have one form
// exactly when every negotiation gives them one answer: "gzip, deflate, br"
// and "gzip,deflate,br", "GZIP" and "gzip", "gzip;q=0.8" and "gzip; q=0.8",
// and "gzip;q=0.80" are one value, while "gzip;q=0" and "gzip;Q=0" (which the
// negotiator reads as q=1: it matches "q=" in lower case only), "gzip;q = 0"
// and "g zip" are not, because the negotiator tells them apart. A value
// folded by anything looser (blanks inside a token or around "=", case of the
// weight) would give a waiter an encoding it refused. Every other header keeps
// its value as the client sent it.
func canonicalAppend(dst []byte, name, v string) []byte {
	if name != "accept-encoding" {
		return append(dst, v...)
	}
	var buf [8]negotiate.AcceptItem
	for i, e := range negotiate.ParseInto(buf[:0], v) {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, e.MediaType...)
		if e.Quality != 1 {
			dst = append(dst, ";q="...)
			dst = strconv.AppendFloat(dst, e.Quality, 'g', -1, 64)
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
