// Package httprange decides how a file-serving path answers a byte Range
// request: RFC 9110 §14 (Range, 206, 416) and §13.1.5 (If-Range). It is
// shared by [github.com/goceleris/celeris.Context.File] and
// middleware/static, so both answer the same request the same way.
//
// Only a single range is served. A range set with more than one satisfiable
// range is answered with the whole representation (200), which §14.2 allows;
// multipart/byteranges is celeris#831.
package httprange

import (
	"math"
	"net/http"
	"strconv"
	"strings"
)

// Outcome is what the server sends for a request.
type Outcome uint8

const (
	// Full means: ignore the Range header and send the whole representation (200).
	// Used when there is no Range header, the method is not GET, If-Range
	// does not hold, the unit is not "bytes", the range set is invalid, the
	// representation is empty, or more than one range is satisfiable.
	Full Outcome = iota
	// Partial means: send 206 with the single range [start, end] (inclusive) and
	// Content-Range: bytes start-end/size.
	Partial
	// Unsatisfiable means: send 416 with Content-Range: bytes */size. No byte of
	// the representation satisfies the range set.
	Unsatisfiable
)

// Decide applies §13.2.2 step 5 and §14.2 to a request for a representation
// of size bytes whose current validators are etag and lastModified (the ETag
// and Last-Modified response field values; either may be empty). method,
// rangeHdr and ifRange are the request's method, Range and If-Range field
// values. Preconditions that come earlier (If-None-Match, If-Modified-Since:
// a 304) are the caller's to evaluate first.
func Decide(method, rangeHdr, ifRange, etag, lastModified string, size int64) (start, end int64, out Outcome) {
	if rangeHdr == "" {
		return 0, 0, Full
	}
	// §14.2: GET is the only method for which range handling is defined;
	// a server MUST ignore Range on any other (HEAD included).
	if method != "GET" {
		return 0, 0, Full
	}
	if ifRange != "" && !IfRange(ifRange, etag, lastModified) {
		return 0, 0, Full
	}
	return Parse(rangeHdr, size)
}

// IfRange evaluates an If-Range field value against the selected
// representation's current ETag and Last-Modified field values (§13.1.5). It
// reports whether the Range header may be applied.
//
// An entity-tag (a DQUOTE in its first three characters) holds only when it
// equals the current ETag under the strong comparison (§8.8.3.2): neither tag
// weak, opaque-tags identical. An HTTP-date holds only when it is the same
// instant as the current Last-Modified. Anything else, including a missing
// current validator or a value that is neither, does not hold. A client may
// send a date only when it has deduced it is a strong validator (§8.8.2.2:
// its response's Date was at least a second after Last-Modified), which is
// what makes an exact date match safe; such a client never holds a date
// that two versions of the file share.
func IfRange(ifRange, etag, lastModified string) bool {
	v := trimOWS(ifRange)
	if isEntityTag(v) {
		cur := trimOWS(etag)
		return validStrongTag(v) && v == cur
	}
	lm := trimOWS(lastModified)
	if lm == "" {
		return false
	}
	// The client echoes the field value it was given: an exact match needs
	// no date parsing (two http.ParseTime calls cost ~0.5 µs). A different
	// spelling of the same instant (RFC 850, asctime) is still accepted below.
	if v == lm {
		return true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return false
	}
	cur, err := http.ParseTime(lm)
	if err != nil {
		return false
	}
	return t.Equal(cur)
}

// isEntityTag reports whether an If-Range value is an entity-tag rather than
// an HTTP-date: §13.1.5 tells them apart by a DQUOTE in the first three
// characters.
func isEntityTag(v string) bool {
	n := min(len(v), 3)
	return strings.IndexByte(v[:n], '"') >= 0
}

// validStrongTag reports whether v is a well-formed strong entity-tag:
// DQUOTE *etagc DQUOTE, etagc = %x21 / %x23-7E / obs-text (§8.8.3).
func validStrongTag(v string) bool {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return false
	}
	for i := 1; i < len(v)-1; i++ {
		if c := v[i]; c == '"' || c < 0x21 || c == 0x7f {
			return false
		}
	}
	return true
}

// Parse evaluates a Range field value against a representation of size bytes
// (§14.1, §14.2). Only the "bytes" unit (case-insensitive) is understood;
// any other unit is ignored. A syntactically invalid range set is ignored
// (§14.2 allows ignoring or rejecting it). A valid set none of whose ranges is
// satisfiable is Unsatisfiable. A last-pos at or past the end, and a suffix
// longer than the representation, are cut to it (§14.1.2). Positions too
// large for an int64 saturate, so they never fail to parse (§14.1.2 asks
// recipients to anticipate large numerals).
func Parse(header string, size int64) (start, end int64, out Outcome) {
	h := trimOWS(header)
	eq := strings.IndexByte(h, '=')
	if eq < 0 || !strings.EqualFold(h[:eq], "bytes") {
		return 0, 0, Full
	}
	// §14.2: a server MAY ignore Range when the representation is empty;
	// no 206 can describe a part of nothing.
	if size <= 0 {
		return 0, 0, Full
	}
	set := h[eq+1:]
	specs, satisfiable := 0, 0
	for len(set) > 0 {
		var elem string
		if i := strings.IndexByte(set, ','); i >= 0 {
			elem, set = set[:i], set[i+1:]
		} else {
			elem, set = set, ""
		}
		elem = trimOWS(elem)
		if elem == "" {
			continue // #rule: empty list elements are allowed and ignored
		}
		specs++
		s, e, ok, valid := parseSpec(elem, size)
		if !valid {
			return 0, 0, Full
		}
		if ok {
			satisfiable++
			if satisfiable == 1 {
				start, end = s, e
			}
		}
	}
	switch {
	case specs == 0:
		return 0, 0, Full // "bytes=" with no range-spec: invalid
	case satisfiable == 0:
		return 0, 0, Unsatisfiable
	case satisfiable > 1:
		return 0, 0, Full // multipart/byteranges: celeris#831
	}
	return start, end, Partial
}

// parseSpec parses one bytes range-spec. valid is false for anything that is
// not an int-range or suffix-range, or an int-range whose last-pos is less
// than its first-pos. ok reports whether the spec is satisfiable, and then
// [start, end] is the range cut to the representation.
func parseSpec(spec string, size int64) (start, end int64, ok, valid bool) {
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false, false
	}
	if dash == 0 { // suffix-range = "-" suffix-length
		n, okN := digits(spec[1:])
		if !okN {
			return 0, 0, false, false
		}
		if n == 0 {
			return 0, 0, false, true
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, true
	}
	first, okF := digits(spec[:dash])
	if !okF {
		return 0, 0, false, false
	}
	last := int64(math.MaxInt64)
	if rest := spec[dash+1:]; rest != "" {
		l, okL := digits(rest)
		if !okL {
			return 0, 0, false, false
		}
		if l < first {
			return 0, 0, false, false
		}
		last = l
	}
	if first >= size {
		return 0, 0, false, true
	}
	if last >= size {
		last = size - 1
	}
	return first, last, true, true
}

// digits parses 1*DIGIT, saturating at math.MaxInt64. No sign, no space.
func digits(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		d := int64(c - '0')
		if n > (math.MaxInt64-d)/10 {
			n = math.MaxInt64
			continue
		}
		n = n*10 + d
	}
	return n, true
}

// trimOWS strips optional whitespace (SP, HTAB) from both ends.
func trimOWS(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// AppendContentRange appends "bytes start-end/size" (§14.4) to dst.
func AppendContentRange(dst []byte, start, end, size int64) []byte {
	dst = append(dst, "bytes "...)
	dst = strconv.AppendInt(dst, start, 10)
	dst = append(dst, '-')
	dst = strconv.AppendInt(dst, end, 10)
	dst = append(dst, '/')
	return strconv.AppendInt(dst, size, 10)
}

// AppendUnsatisfied appends "bytes */size" (§14.4), the Content-Range of a
// 416, to dst.
func AppendUnsatisfied(dst []byte, size int64) []byte {
	dst = append(dst, "bytes */"...)
	return strconv.AppendInt(dst, size, 10)
}
