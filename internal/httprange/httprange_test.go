package httprange

import (
	"fmt"
	"math"
	"strconv"
	"testing"
)

func TestParse(t *testing.T) {
	const big = "99999999999999999999999999"
	tests := []struct {
		header     string
		size       int64
		start, end int64
		out        Outcome
	}{
		// Satisfiable single ranges (RFC 9110 §14.1.2).
		{"bytes=0-4", 10, 0, 4, Partial},
		{"bytes=5-9", 10, 5, 9, Partial},
		{"bytes=-3", 10, 7, 9, Partial},
		{"bytes=7-", 10, 7, 9, Partial},
		{"bytes=0-0", 10, 0, 0, Partial},
		{"bytes=9-9", 10, 9, 9, Partial},
		{"bytes=5-100", 10, 5, 9, Partial},    // last-pos past the end: cut
		{"bytes=5-" + big, 10, 5, 9, Partial}, // saturating last-pos
		{"bytes=-20", 10, 0, 9, Partial},      // suffix longer than the file
		{"bytes=-" + big, 10, 0, 9, Partial},  // saturating suffix
		{"BYTES=1-2", 10, 1, 2, Partial},      // unit is case-insensitive
		{"bytes= 1-2", 10, 1, 2, Partial},     // OWS in the list
		{" bytes=1-2 ", 10, 1, 2, Partial},    // OWS around the value
		{"bytes=1-2,", 10, 1, 2, Partial},     // empty list element
		{"bytes=,1-2", 10, 1, 2, Partial},     // empty list element
		{"bytes=1-2, 50-", 10, 1, 2, Partial}, // one satisfiable of two
		{"bytes=50-, -3", 10, 7, 9, Partial},  // one satisfiable of two
		{"bytes=0-" + strconv.Itoa(1<<20), 1 << 30, 0, 1 << 20, Partial},
		// Unsatisfiable (§14.1.1): valid, but no range-spec is satisfiable.
		{"bytes=10-15", 10, 0, 0, Unsatisfiable},
		{"bytes=10-", 10, 0, 0, Unsatisfiable},
		{"bytes=-0", 10, 0, 0, Unsatisfiable},
		{"bytes=20-30, 40-", 10, 0, 0, Unsatisfiable},
		{"bytes=" + big + "-", 10, 0, 0, Unsatisfiable},
		{"bytes=-0,-0", 10, 0, 0, Unsatisfiable},
		// Ignored: more than one satisfiable range (#831).
		{"bytes=0-1,5-6", 10, 0, 0, Full},
		{"bytes=0-0,-1", 10, 0, 0, Full},
		// Ignored: invalid ranges-specifier (§14.2 MAY ignore).
		{"bytes=5-3", 10, 0, 0, Full},           // last-pos < first-pos
		{"bytes=" + big + "-5", 10, 0, 0, Full}, // last-pos < saturated first-pos
		{"bytes=0-4,5-3", 10, 0, 0, Full},       // one invalid spec spoils the set
		{"bytes=abc-def", 10, 0, 0, Full},
		{"bytes=abc", 10, 0, 0, Full},
		{"bytes=+1-2", 10, 0, 0, Full}, // sign is not DIGIT
		{"bytes=1-+2", 10, 0, 0, Full},
		{"bytes=--1", 10, 0, 0, Full},
		{"bytes=-", 10, 0, 0, Full},
		{"bytes=1 -2", 10, 0, 0, Full},
		{"bytes=", 10, 0, 0, Full},
		{"bytes=,", 10, 0, 0, Full},
		{"bytes", 10, 0, 0, Full},
		{"", 10, 0, 0, Full},
		// Ignored: unknown unit (§14.2 MUST ignore).
		{"chars=0-4", 10, 0, 0, Full},
		{"bytesx=0-4", 10, 0, 0, Full},
		// Ignored: empty representation (§14.2 MAY ignore).
		{"bytes=0-", 0, 0, 0, Full},
		{"bytes=-5", 0, 0, 0, Full},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q/%d", tt.header, tt.size), func(t *testing.T) {
			start, end, out := Parse(tt.header, tt.size)
			if out != tt.out {
				t.Fatalf("outcome %d, want %d", out, tt.out)
			}
			if out == Partial && (start != tt.start || end != tt.end) {
				t.Fatalf("range %d-%d, want %d-%d", start, end, tt.start, tt.end)
			}
		})
	}
}

func TestIfRange(t *testing.T) {
	const (
		lm     = "Wed, 01 Jul 2026 10:00:00 GMT"
		lm850  = "Wednesday, 01-Jul-26 10:00:00 GMT"
		lmANSI = "Wed Jul  1 10:00:00 2026"
		older  = "Wed, 01 Jul 2026 09:59:59 GMT"
	)
	tests := []struct {
		name, ifRange, etag, lastMod string
		want                         bool
	}{
		{"etag/equal", `"v1"`, `"v1"`, "", true},
		{"etag/equal-ows", ` "v1" `, `"v1"`, "", true},
		{"etag/differs", `"v1"`, `"v2"`, lm, false},
		{"etag/empty-opaque", `""`, `""`, "", true},
		{"etag/weak-request", `W/"v1"`, `"v1"`, "", false},
		{"etag/weak-current", `"v1"`, `W/"v1"`, "", false},
		{"etag/weak-both", `W/"v1"`, `W/"v1"`, "", false},
		{"etag/no-current", `"v1"`, "", lm, false},
		{"etag/unterminated", `"v1`, `"v1`, "", false},
		{"etag/inner-quote", `"v"1"`, `"v"1"`, "", false},
		{"etag/inner-space", `"v 1"`, `"v 1"`, "", false},
		{"etag/case-differs", `"V1"`, `"v1"`, "", false},
		{"date/equal", lm, `"v1"`, lm, true},
		{"date/equal-rfc850", lm850, "", lm, true},
		{"date/equal-asctime", lmANSI, "", lm, true},
		{"date/older", older, "", lm, false},
		{"date/no-current", lm, `"v1"`, "", false},
		{"date/current-unparseable", lm, "", "yesterday", false},
		{"date/garbage", "yesterday", `"v1"`, lm, false},
		{"date/looks-like-tag-later", `xy"z`, `"z"`, lm, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IfRange(tt.ifRange, tt.etag, tt.lastMod); got != tt.want {
				t.Fatalf("IfRange(%q, etag %q, last-modified %q) = %v, want %v",
					tt.ifRange, tt.etag, tt.lastMod, got, tt.want)
			}
		})
	}
}

func TestDecide(t *testing.T) {
	const lm = "Wed, 01 Jul 2026 10:00:00 GMT"
	tests := []struct {
		name, method, rng, ifRange, etag, lastMod string
		out                                       Outcome
	}{
		{"no-range", "GET", "", `"v1"`, `"v1"`, lm, Full},
		{"get-range", "GET", "bytes=1-2", "", "", "", Partial},
		{"head-ignores", "HEAD", "bytes=1-2", "", "", "", Full},
		{"head-ignores-416", "HEAD", "bytes=50-", "", "", "", Full},
		{"post-ignores", "POST", "bytes=1-2", "", "", "", Full},
		{"if-range-holds", "GET", "bytes=1-2", `"v1"`, `"v1"`, lm, Partial},
		{"if-range-fails", "GET", "bytes=1-2", `"v0"`, `"v1"`, lm, Full},
		{"if-range-fails-before-416", "GET", "bytes=50-", `"v0"`, `"v1"`, lm, Full},
		{"if-range-holds-416", "GET", "bytes=50-", lm, `"v1"`, lm, Unsatisfiable},
		{"unsatisfiable", "GET", "bytes=50-", "", "", "", Unsatisfiable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, out := Decide(tt.method, tt.rng, tt.ifRange, tt.etag, tt.lastMod, 10); out != tt.out {
				t.Fatalf("outcome %d, want %d", out, tt.out)
			}
		})
	}
}

func TestAppend(t *testing.T) {
	if got := string(AppendContentRange(nil, 2, 5, 10)); got != "bytes 2-5/10" {
		t.Fatalf("AppendContentRange = %q", got)
	}
	if got := string(AppendContentRange(nil, 0, math.MaxInt64-1, math.MaxInt64)); got != "bytes 0-9223372036854775806/9223372036854775807" {
		t.Fatalf("AppendContentRange = %q", got)
	}
	if got := string(AppendUnsatisfied(nil, 10)); got != "bytes */10" {
		t.Fatalf("AppendUnsatisfied = %q", got)
	}
}

// FuzzParse checks that Parse never panics and that a Partial range always
// lies inside the representation.
func FuzzParse(f *testing.F) {
	for _, s := range []string{"bytes=0-4", "bytes=-3", "bytes=7-", "bytes=5-3", "bytes=0-1,5-6",
		"bytes=-0", "bytes=", "chars=1-2", "bytes= 1-2 , 3-", "bytes=99999999999999999999-"} {
		f.Add(s, int64(10))
	}
	f.Add("bytes=0-", int64(0))
	f.Fuzz(func(t *testing.T, header string, size int64) {
		start, end, out := Parse(header, size)
		if out == Partial && (start < 0 || end < start || end >= size) {
			t.Fatalf("Parse(%q, %d) = %d-%d outside the representation", header, size, start, end)
		}
	})
}

// FuzzIfRange checks that IfRange never panics and never holds without a
// current validator.
func FuzzIfRange(f *testing.F) {
	f.Add(`"v1"`, `"v1"`, "")
	f.Add(`W/"v1"`, `W/"v1"`, "")
	f.Add("Wed, 01 Jul 2026 10:00:00 GMT", "", "Wed, 01 Jul 2026 10:00:00 GMT")
	f.Add(`"`, "", "")
	f.Fuzz(func(t *testing.T, ifRange, etag, lastMod string) {
		if IfRange(ifRange, etag, lastMod) && etag == "" && lastMod == "" {
			t.Fatalf("IfRange(%q) held with no current validator", ifRange)
		}
	})
}
