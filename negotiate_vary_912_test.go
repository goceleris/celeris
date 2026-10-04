package celeris

import (
	"strings"
	"testing"
)

// varyValues returns the response's Vary header lines.
func varyValues(c *Context) []string {
	var out []string
	for _, h := range c.respHeaders {
		if h[0] == "vary" {
			out = append(out, h[1])
		}
	}
	return out
}

// TestNegotiationNamesItsHeaderInVary912: celeris#912's family. Respond and
// Negotiate choose the representation from Accept, and AcceptsEncodings and
// AcceptsLanguages from Accept-Encoding and Accept-Language, but none said
// so in Vary. A shared cache, or middleware/singleflight (whose default key
// leaves Accept out and which compares the headers a response names in Vary
// before it hands a waiter the leader's response), then gave a client that
// asked for XML another client's JSON. Each now adds its header to Vary,
// once, whatever the request sent, the header absent included.
func TestNegotiationNamesItsHeaderInVary912(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reqHdrs [][2]string
		preset  [][2]string // response headers set before the call
		call    func(c *Context)
		want    []string
	}{
		{"Negotiate", [][2]string{{"accept", "application/xml"}}, nil,
			func(c *Context) { c.Negotiate("application/json", "application/xml") }, []string{"Accept"}},
		{"Negotiate without Accept", nil, nil,
			func(c *Context) { c.Negotiate("application/json", "application/xml") }, []string{"Accept"}},
		{"Respond", [][2]string{{"accept", "application/xml"}}, nil,
			func(c *Context) { _ = c.Respond(200, map[string]string{"k": "v"}) }, []string{"Accept"}},
		{"AcceptsEncodings", [][2]string{{"accept-encoding", "gzip"}}, nil,
			func(c *Context) { c.AcceptsEncodings("br", "gzip") }, []string{"Accept-Encoding"}},
		{"AcceptsLanguages", [][2]string{{"accept-language", "de"}}, nil,
			func(c *Context) { c.AcceptsLanguages("en", "de") }, []string{"Accept-Language"}},
		{"twice", [][2]string{{"accept", "application/xml"}}, nil,
			func(c *Context) { c.Negotiate("application/json"); c.Negotiate("application/xml") }, []string{"Accept"}},
		{"kept beside another Vary", [][2]string{{"accept", "application/xml"}}, [][2]string{{"vary", "Origin"}},
			func(c *Context) { c.Negotiate("application/json") }, []string{"Origin", "Accept"}},
		{"already listed", [][2]string{{"accept", "application/xml"}}, [][2]string{{"vary", "Origin, accept"}},
			func(c *Context) { c.Negotiate("application/json") }, []string{"Origin, accept"}},
		{"Vary *", [][2]string{{"accept", "application/xml"}}, [][2]string{{"vary", "*"}},
			func(c *Context) { c.Negotiate("application/json") }, []string{"*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStream("GET", "/doc")
			defer s.Release()
			s.Headers = append(s.Headers, tc.reqHdrs...)
			c := acquireContext(s)
			defer releaseContext(c)
			for _, h := range tc.preset {
				c.AddHeader(h[0], h[1])
			}
			tc.call(c)
			if got := varyValues(c); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("Vary lines %q, want %q", got, tc.want)
			}
		})
	}
}
