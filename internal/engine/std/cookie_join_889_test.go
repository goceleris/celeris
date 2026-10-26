package std

import (
	"net/http"
	"runtime"
	"strings"
	"testing"
)

// celeris#889 item 16: the std bridge delivers several Cookie lines as one
// field joined with "; ", in one pass however many lines there are (net/http
// bounds the header bytes, not the number of fields).
func TestAppendRequestHeadersJoinsCookieLines889(t *testing.T) {
	h := http.Header{}
	h.Add("Cookie", "lang=en")
	h.Add("X-Other", "1")
	h.Add("Cookie", "sid=ALICE")
	var cookies []string
	var others int
	for _, f := range AppendRequestHeaders(nil, h) {
		switch f[0] {
		case "cookie":
			cookies = append(cookies, f[1])
		case "x-other":
			others++
		}
	}
	if len(cookies) != 1 || cookies[0] != "lang=en; sid=ALICE" || others != 1 {
		t.Fatalf("cookie fields %q, %d x-other; want one \"lang=en; sid=ALICE\" and one x-other", cookies, others)
	}
}

func TestAppendRequestHeadersJoinIsLinear889(t *testing.T) {
	const lines = 5000
	h := http.Header{"Cookie": make([]string, lines)}
	for i := range h["Cookie"] {
		h["Cookie"][i] = "a=b"
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out := AppendRequestHeaders(nil, h)
	runtime.ReadMemStats(&after)
	if len(out) != 1 || len(out[0][1]) != lines*3+(lines-1)*2 || !strings.HasPrefix(out[0][1], "a=b; a=b") {
		t.Fatalf("got %d fields, first of %d bytes", len(out), len(out[0][1]))
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
		t.Errorf("joining %d cookie lines allocated %d bytes: the join is not linear", lines, got)
	}
}
