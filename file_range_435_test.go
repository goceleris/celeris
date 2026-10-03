package celeris

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests for celeris#435: Context.File answers a Range request the way
// RFC 9110 §14 and §13.1.5 say.
//
//   - A range set that no byte of the file satisfies gets 416 Range Not
//     Satisfiable with "Content-Range: bytes */<size>", not the whole file
//     as a 200 (§14.2, §15.5.17).
//   - A last-pos past the end, or a suffix longer than the file, is still a
//     satisfiable range: it is cut to the file (§14.1.2), not answered 200.
//   - If-Range: the range is served only while the validator the client sends
//     still matches the response's current one, with the strong comparison
//     for an entity-tag (a weak tag never matches) and an exact match for a
//     date; otherwise the whole file is sent as a 200 (§13.1.5). Without it,
//     a client that resumes a download after the file changed splices bytes
//     of the new file onto its copy of the old one: the corrupt 206.
//   - Range is defined for GET only, so HEAD (and any other method) ignores
//     it (§14.2).
//   - An invalid range set and an unknown range unit are ignored (the whole
//     file, 200), and so, until multipart/byteranges lands (#831), is a set
//     with more than one satisfiable range.

const (
	file435     = "0123456789"
	lm435       = "Wed, 01 Jul 2026 10:00:00 GMT"
	lmOlder435  = "Tue, 30 Jun 2026 10:00:00 GMT"
	lmRFC850435 = "Wednesday, 01-Jul-26 10:00:00 GMT" // same instant as lm435
)

type fileRangeCase435 struct {
	name     string
	method   string
	rng      string // Range request header ("" = absent)
	ifRange  string // If-Range request header ("" = absent)
	etag     string // ETag the handler sets before c.File ("" = none)
	lastMod  string // Last-Modified the handler sets before c.File ("" = none)
	status   int
	body     string
	crange   string // wanted Content-Range ("" = must be absent)
	headBody bool   // HEAD: the body handed to the writer is not checked
}

func fileRangeCases435() []fileRangeCase435 {
	return []fileRangeCase435{
		// 416: nothing in the set is satisfiable.
		{name: "416/first-pos-at-size", method: "GET", rng: "bytes=10-", status: 416, crange: "bytes */10"},
		{name: "416/first-pos-past-size", method: "GET", rng: "bytes=50-100", status: 416, crange: "bytes */10"},
		{name: "416/suffix-zero", method: "GET", rng: "bytes=-0", status: 416, crange: "bytes */10"},
		{name: "416/every-spec-past-size", method: "GET", rng: "bytes=20-30, 40-", status: 416, crange: "bytes */10"},
		{name: "416/huge-first-pos", method: "GET", rng: "bytes=99999999999999999999999-", status: 416, crange: "bytes */10"},

		// 206: satisfiable, cut to the file.
		{name: "206/single", method: "GET", rng: "bytes=2-5", status: 206, body: "2345", crange: "bytes 2-5/10"},
		{name: "206/open-end", method: "GET", rng: "bytes=7-", status: 206, body: "789", crange: "bytes 7-9/10"},
		{name: "206/suffix", method: "GET", rng: "bytes=-3", status: 206, body: "789", crange: "bytes 7-9/10"},
		{name: "206/last-pos-past-end", method: "GET", rng: "bytes=5-100", status: 206, body: "56789", crange: "bytes 5-9/10"},
		{name: "206/huge-last-pos", method: "GET", rng: "bytes=5-99999999999999999999999", status: 206, body: "56789", crange: "bytes 5-9/10"},
		{name: "206/suffix-longer-than-file", method: "GET", rng: "bytes=-20", status: 206, body: file435, crange: "bytes 0-9/10"},
		{name: "206/unit-case-insensitive", method: "GET", rng: "Bytes=2-5", status: 206, body: "2345", crange: "bytes 2-5/10"},
		{name: "206/ows-in-set", method: "GET", rng: "bytes= 2-5 ", status: 206, body: "2345", crange: "bytes 2-5/10"},
		{name: "206/one-satisfiable-of-two", method: "GET", rng: "bytes=2-5,50-60", status: 206, body: "2345", crange: "bytes 2-5/10"},

		// 200: Range ignored.
		{name: "200/no-range", method: "GET", status: 200, body: file435},
		{name: "200/invalid-reversed", method: "GET", rng: "bytes=5-3", status: 200, body: file435},
		{name: "200/invalid-sign", method: "GET", rng: "bytes=+2-5", status: 200, body: file435},
		{name: "200/invalid-garbage", method: "GET", rng: "bytes=abc", status: 200, body: file435},
		{name: "200/invalid-empty-set", method: "GET", rng: "bytes=", status: 200, body: file435},
		{name: "200/unknown-unit", method: "GET", rng: "chars=0-4", status: 200, body: file435},
		{name: "200/multi-range-until-831", method: "GET", rng: "bytes=0-1,5-6", status: 200, body: file435},
		{name: "200/head-ignores-range", method: "HEAD", rng: "bytes=2-5", status: 200, headBody: true},
		{name: "200/head-ignores-unsatisfiable", method: "HEAD", rng: "bytes=50-", status: 200, headBody: true},

		// If-Range with an entity-tag: strong comparison.
		{name: "if-range/etag-match", method: "GET", rng: "bytes=2-5", ifRange: `"v2"`, etag: `"v2"`, status: 206, body: "2345", crange: "bytes 2-5/10"},
		{name: "if-range/etag-changed", method: "GET", rng: "bytes=5-", ifRange: `"v1"`, etag: `"v2"`, status: 200, body: file435},
		{name: "if-range/etag-weak-both", method: "GET", rng: "bytes=5-", ifRange: `W/"v2"`, etag: `W/"v2"`, status: 200, body: file435},
		{name: "if-range/etag-current-weak", method: "GET", rng: "bytes=5-", ifRange: `"v2"`, etag: `W/"v2"`, status: 200, body: file435},
		{name: "if-range/etag-none-on-response", method: "GET", rng: "bytes=5-", ifRange: `"v2"`, lastMod: lm435, status: 200, body: file435},
		{name: "if-range/etag-changed-unsatisfiable", method: "GET", rng: "bytes=50-", ifRange: `"v1"`, etag: `"v2"`, status: 200, body: file435},

		// If-Range with an HTTP-date: exact match with Last-Modified.
		{name: "if-range/date-match", method: "GET", rng: "bytes=2-5", ifRange: lm435, lastMod: lm435, status: 206, body: "2345", crange: "bytes 2-5/10"},
		{name: "if-range/date-match-rfc850", method: "GET", rng: "bytes=2-5", ifRange: lmRFC850435, lastMod: lm435, status: 206, body: "2345", crange: "bytes 2-5/10"},
		{name: "if-range/date-changed", method: "GET", rng: "bytes=5-", ifRange: lmOlder435, lastMod: lm435, status: 200, body: file435},
		{name: "if-range/date-none-on-response", method: "GET", rng: "bytes=5-", ifRange: lm435, etag: `"v2"`, status: 200, body: file435},
		{name: "if-range/garbage", method: "GET", rng: "bytes=5-", ifRange: "yesterday", lastMod: lm435, etag: `"v2"`, status: 200, body: file435},
		{name: "if-range/without-range", method: "GET", ifRange: `"v1"`, etag: `"v2"`, status: 200, body: file435},
	}
}

func TestFileRange435(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte(file435), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fileRangeCases435() {
		t.Run(tc.name, func(t *testing.T) {
			s, rw := newTestStream(tc.method, "/f")
			defer s.Release()
			if tc.rng != "" {
				s.Headers = append(s.Headers, [2]string{"range", tc.rng})
			}
			if tc.ifRange != "" {
				s.Headers = append(s.Headers, [2]string{"if-range", tc.ifRange})
			}
			c := acquireContext(s)
			defer releaseContext(c)
			if tc.etag != "" {
				c.SetHeader("etag", tc.etag)
			}
			if tc.lastMod != "" {
				c.SetHeader("last-modified", tc.lastMod)
			}
			if err := c.File(path); err != nil {
				t.Fatalf("File: %v", err)
			}
			if rw.status != tc.status {
				t.Fatalf("status %d, want %d (body %q)", rw.status, tc.status, rw.body)
			}
			if !tc.headBody && string(rw.body) != tc.body {
				t.Fatalf("body %q, want %q", rw.body, tc.body)
			}
			var cr []string
			for _, h := range rw.headers {
				if h[0] == "content-range" {
					cr = append(cr, h[1])
				}
			}
			switch {
			case tc.crange == "" && len(cr) != 0:
				t.Fatalf("content-range %q on a %d, want none", cr, rw.status)
			case tc.crange != "" && (len(cr) != 1 || cr[0] != tc.crange):
				t.Fatalf("content-range %q, want exactly [%q]", cr, tc.crange)
			}
		})
	}
}
