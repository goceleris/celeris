package std

import (
	"net/http"
	"testing"
)

// h2cSettings is the std engine's side of the one RFC 7540 3.2.1 rule (the
// cross-engine table is test/server TestH2CSettingsRuleEveryEngine7540): exactly
// one HTTP2-Settings value that decodes, which may be empty.
func TestH2CSettingsRule889(t *testing.T) {
	const v = "AAMAAABkAAQAoAAAAAIAAAAA"
	for _, tc := range []struct {
		name string
		vals []string // nil: no header
		ok   bool
		n    int // decoded length
	}{
		{"one", []string{v}, true, 18},
		{"one with OWS", []string{" " + v + "\t"}, true, 18},
		{"one empty", []string{""}, true, 0},
		// Only SP and HTAB are optional whitespace (RFC 9110 5.5): U+00A0 and
		// U+0085 are not trimmed, as on the native engines.
		{"one with NBSP", []string{v + "\xc2\xa0"}, false, 0},
		{"one with NEL", []string{"\xc2\x85" + v}, false, 0},
		{"missing", nil, false, 0},
		{"repeated", []string{v, v}, false, 0},
		{"repeated, one empty", []string{v, ""}, false, 0},
		{"undecodable", []string{"!!!"}, false, 0},
		{"standard alphabet", []string{"AAMA+/ABkAAQAoAAAAAIAAAAA"}, false, 0},
		{"5 bytes", []string{"AQIDBAU"}, false, 0},
		{"7 bytes", []string{"AQIDBAUGBw"}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.vals != nil {
				h["Http2-Settings"] = tc.vals
			}
			got, ok := h2cSettings(h)
			if ok != tc.ok || len(got) != tc.n {
				t.Fatalf("h2cSettings = (%d bytes, %v), want (%d, %v)", len(got), ok, tc.n, tc.ok)
			}
		})
	}
}
