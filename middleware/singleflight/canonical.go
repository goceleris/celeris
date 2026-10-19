package singleflight

import (
	"strconv"
	"strings"

	"github.com/goceleris/celeris/internal/negotiate"
)

// maxCanonicalAE is the longest Accept-Encoding value that is canonicalised.
// The negotiator's parser grows a slice by one entry per list item with no
// limit, so running it over a value the client chose the length of (the std
// engine accepts a 16 MiB header by default, the native engines 64 KiB) cost
// about 66 times that length in memory and about 200 times the copy main
// made, per request and again per waiter. A longer value is used as sent:
// equal values negotiate alike, so the only cost is a coalescing missed
// between two spellings of one long list. A browser's value is 20 to 40 bytes.
const maxCanonicalAE = 256

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
// exactly when the negotiator gives them one answer: "gzip, deflate, br"
// and "gzip,deflate,br", "GZIP" and "gzip", "gzip;q=0.8" and "gzip; q=0.8",
// and "gzip;q=0.80" are one value, and so are "gzip;q=0" and "gzip;Q=0" (the
// name of the weight is case-insensitive, RFC 9110 12.4.2), while "gzip;q=0"
// and "gzip;q = 0" (which the negotiator reads as q=1: blanks around "=" make
// no weight) and "g zip" are not, because the negotiator tells them apart.
// A value folded by anything looser (blanks inside a token or around "=")
// would give a waiter an encoding it refused. Every other header keeps
// its value as the client sent it, and so does an Accept-Encoding longer than
// maxCanonicalAE bytes (see there).
func canonicalAppend(dst []byte, name, v string) []byte {
	if name != "accept-encoding" || len(v) > maxCanonicalAE {
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
	if name != "accept-encoding" || len(v) > maxCanonicalAE {
		return strings.Clone(v)
	}
	var buf [64]byte
	return string(canonicalAppend(buf[:0], name, v))
}

// sameCanonical reports whether v is, canonically, the value stored (a
// canonicalValue). It builds the canonical form of v in a stack buffer, so it
// allocates only for a value whose form is longer than 64 bytes, or that holds
// an upper-case coding name (the negotiator lower-cases it into a new string)
// or more than eight list entries. A value past maxCanonicalAE is compared as
// sent, and allocates nothing.
func sameCanonical(name, stored, v string) bool {
	if name != "accept-encoding" || len(v) > maxCanonicalAE {
		return v == stored
	}
	var buf [64]byte
	return string(canonicalAppend(buf[:0], name, v)) == stored
}
