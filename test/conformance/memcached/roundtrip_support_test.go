//go:build memcached

package memcached_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	celmc "github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/test/internal/supporttable"
)

// TestSupportTable builds the memcached "supported behaviour" table and checks
// it against testdata/supported.json, which the documentation site renders.
// It needs no server and does not read mcKnownBugs.
//
// Most of the memcached matrix is imperative (each test runs commands and
// checks replies), so the rows are declared here, each naming the test that
// checks it; supporttable.CheckCovers fails when a matrix test has no row or a
// row names a test that no longer exists. The rows that come from tables in the
// matrix (the values, the too-large sizes, the Set value kinds) are built from
// those tables, and the known-bug column comes from the classifier mcKnownBug
// that the matrix itself uses to leave the failing Gets cells out of the
// default build.
//
//	CELERIS_UPDATE_SUPPORT_TABLES=1 go test -tags memcached -run '^TestSupportTable$' ./test/conformance/memcached/
func TestSupportTable(t *testing.T) {
	tbl := supporttable.Table{
		Driver: "memcached",
		Regen:  "go test -tags memcached -run '^TestSupportTable$' ./test/conformance/memcached/",
		Sections: []supporttable.Section{
			{ID: "behaviour", Title: "Values, keys and commands", Rows: mcSupportRows()},
		},
	}
	supporttable.Finish(t, &tbl, mcGaps)
	supporttable.CheckCovers(t, []string{"roundtrip_values_test.go"}, tbl.Sections)
	supporttable.Verify(t, "testdata/supported.json", tbl)
}

// mcGapID is the bug id a classification string starts with ("M1 ...").
func mcGapID(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

func mcSupportRows() []supporttable.Row {
	var kinds []string
	for _, c := range mcSetTypeCases {
		if k := strings.ReplaceAll(fmt.Sprintf("%T", c.v), "uint8", "byte"); !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	var tooLarge []string
	for _, c := range mcTooLargeCases {
		tooLarge = append(tooLarge, strings.Replace(c.name, "MiB", " MiB", 1))
	}

	gets := func(id, label string, n int) supporttable.Row {
		col := supporttable.NewCollector(id, label, "an `Item` with the value, the flags and the CAS token")
		for _, p := range []celmc.Protocol{celmc.ProtocolText, celmc.ProtocolBinary} {
			col.Cell("Gets", p.String(), mcGapID(mcKnownBug(p, n)))
		}
		r := col.Row()
		r.Covers = []string{"TestRoundTripValues"}
		return r
	}
	rows := []supporttable.Row{
		{ID: "values", Label: "Values of any bytes, from empty up to the server's 1 MiB item limit, including bytes that look like protocol (CRLF, END, VALUE)",
			Returns: "the same bytes, with `Get` as a string or `GetBytes` as a `[]byte`", Works: []string{"Set", "SetBytes", "Get", "GetBytes", "GetMulti", "GetMultiBytes"},
			Covers: []string{"TestRoundTripValues"}},
		gets("gets", "Gets on a value that is not empty", 1),
		gets("gets-empty", "Gets on an empty value", 0),
		{ID: "too-large", Label: "A value over the server's item size limit (" + strings.Join(tooLarge, " and ") + " are tested)",
			Returns: "the server's error; the connection stays in step, and the next `Set` and `Get` work",
			Works:   []string{"SetBytes"}, Covers: []string{"TestRoundTripValues"}},
		{ID: "keys", Label: "Keys: up to 250 bytes, UTF-8 and symbols; longer keys, empty keys, and keys with a space, a control character or a line break",
			Returns: "a valid key works; an invalid one fails before anything is sent, with `ErrMalformedKey` (a key with CRLF in it cannot inject a command)",
			Works:   []string{"Set", "Get"}, Covers: []string{"TestRoundTripKeys"}},
		{ID: "counters", Label: "Counters: Incr and Decr at the edges",
			Returns: "`uint64`; `Incr` wraps from the largest value to 0, `Decr` stops at 0; a value that is not a number is the server's error; a missing key is `ErrCacheMiss`",
			Works:   []string{"Incr", "Decr"}, Covers: []string{"TestRoundTripCounters"}},
		{ID: "missing-key", Label: "A key that is not there",
			Returns: "`ErrCacheMiss` from `Get`, `GetBytes`, `Gets`, `Touch`, `Delete` and `CAS`; `ErrNotStored` from `Append` and `Prepend`; `GetMulti` gives an empty map and no error",
			Works:   []string{"Get", "GetBytes", "Gets", "Touch", "Delete", "Append", "Prepend", "CAS", "GetMulti"}, Covers: []string{"TestRoundTripMissingKey"}},
		{ID: "flags-cas", Label: "Item flags (0 to 4294967295) and CAS tokens",
			Returns: "`Item.Flags` and `Item.CAS`; every store gives a new token; `CAS` with an old token is `ErrCASConflict`. The client cannot set flags itself; it reads what another client stored",
			Works:   []string{"Gets", "Get", "GetMulti", "CAS"}, Covers: []string{"TestRoundTripFlagsAndCAS"}},
		{ID: "set-kinds", Label: "Go values accepted by Set: " + strings.Join(kinds, ", "),
			Returns: "stored as their text form (true as 1, false as 0)",
			Works:   []string{"Set"}, Covers: []string{"TestRoundTripSetTypes"}},
		{ID: "multi-get", Label: "GetMulti of many keys with hits, misses, empty and large values",
			Returns: "a map with one entry per key that exists; a key listed twice gives one entry", Works: []string{"GetMulti", "GetMultiBytes"},
			Covers: []string{"TestRoundTripMultiGet"}},
	}
	return rows
}

// mcGaps is the one hand-maintained part of the table; see rtGaps in the
// postgres and redis packages. TestSupportTable fails for an entry no row
// refers to, so drop an entry when its classification leaves mcKnownBug.
var mcGaps = []supporttable.Gap{
	{ID: "M1", Release: supporttable.V160,
		Summary: "With the binary protocol, `Gets` on an item whose value is empty reports a cache miss, and the item's flags and CAS token are lost, so it cannot be updated with `CAS`. The text protocol is correct.",
		Error:   "celeris-memcached: cache miss"},
}
