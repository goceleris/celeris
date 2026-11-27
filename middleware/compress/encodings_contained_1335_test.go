package compress

import (
	"testing"

	"github.com/goceleris/celeris/internal/negotiate"
)

// TestDefaultOrderMovesAreContained walks every Accept-Encoding header of up to
// four entries drawn from gzip, br, zstd, deflate, "*" and "x-gzip", each
// without a weight, with q=0 and with q=0.5 (111,151 headers), and compares the
// answer before celeris#1335 (negotiate.Accept with the old default order) with
// the answer now (the middleware's negotiator with the default order). Under the
// default configuration:
//
//   - no header starts or stops being compressed;
//   - a header that got gzip still gets gzip;
//   - a header that did not get br does not get br now.
//
// So every move of the table in TestChangedShapesDefault goes to gzip or zstd,
// the two encodings that cost about as much as gzip, and none to br.
func TestDefaultOrderMovesAreContained(t *testing.T) {
	var entries []string
	for _, coding := range []string{"gzip", "br", "zstd", "deflate", "*", "x-gzip"} {
		for _, weight := range []string{"", ";q=0", ";q=0.5"} {
			entries = append(entries, coding+weight)
		}
	}
	checked, moved := 0, 0
	var walk func(header string, depth int)
	walk = func(header string, depth int) {
		before := negotiate.Accept(header, mainDefaultEncodings)
		after := negotiateEncoding(header, defaultConfig.Encodings)
		checked++
		if before != after {
			moved++
		}
		switch {
		case (before == "") != (after == ""):
			t.Fatalf("Accept-Encoding %q: %q before, %q now: compression starts or stops", header, before, after)
		case before == "gzip" && after != "gzip":
			t.Fatalf("Accept-Encoding %q: %q before, %q now: a response leaves gzip", header, before, after)
		case after == "br" && before != "br":
			t.Fatalf("Accept-Encoding %q: %q before, %q now: a response moves to br", header, before, after)
		}
		if depth == 0 {
			return
		}
		for _, e := range entries {
			if header == "" {
				walk(e, depth-1)
			} else {
				walk(header+", "+e, depth-1)
			}
		}
	}
	walk("", 4)
	if moved == 0 {
		t.Fatalf("no header of the %d changed its answer: the walk checks nothing", checked)
	}
	t.Logf("%d headers checked, %d change their answer under the default order", checked, moved)
}
