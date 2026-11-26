//go:build redis

package redis_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/goceleris/celeris/test/internal/supporttable"
)

// TestSupportTable builds the Redis "supported replies" table from the
// declarations the RESP round-trip matrix runs (rtReplyCells, rtHelperRows,
// rtBulkSizes and the known-bug ids they carry) and checks it against
// testdata/supported.json, which the documentation site renders. It needs no
// server and does not read rtKnownBugs, so it gives the same table with and
// without the redisrtbugs tag. After a fix drops a known-bug id, regenerate:
//
//	CELERIS_UPDATE_SUPPORT_TABLES=1 go test -tags redis -run '^TestSupportTable$' ./test/conformance/redis/
func TestSupportTable(t *testing.T) {
	tbl := supporttable.Table{
		Driver: "redis",
		Regen:  "go test -tags redis -run '^TestSupportTable$' ./test/conformance/redis/",
		Sections: []supporttable.Section{
			{ID: "reply-types", Title: "Reply types", Rows: rtSupportReplyRows(t)},
			{ID: "typed-helpers", Title: "Typed helpers on each reply", Rows: rtSupportHelperRows()},
			{ID: "other", Title: "Other behaviour", Rows: rtSupportBehaviourRows()},
		},
	}
	supporttable.Finish(t, &tbl, rtGaps)
	supporttable.CheckCovers(t, []string{"resp_roundtrip_test.go"}, tbl.Sections)
	supporttable.Verify(t, "testdata/supported.json", tbl)
}

// rtReplyGroups names the row a reply cell belongs to, by the prefix of its
// name, in the order the page lists them.
var rtReplyGroups = []struct{ prefix, id, label string }{
	{"simple_string", "simple-string", "Simple string (+OK)"},
	{"integer", "integer", "Integer"},
	{"bulk", "bulk-string", "Bulk string"},
	{"null", "null", "Null"},
	{"array", "array", "Array (also nested, empty, with nulls)"},
	{"map", "map", "Map (RESP3)"},
	{"set", "set", "Set (RESP3)"},
	{"double", "double", "Double (RESP3)"},
	{"bool", "boolean", "Boolean (RESP3)"},
	{"big_number", "big-number", "Big number (RESP3)"},
	{"verbatim_string", "verbatim-string", "Verbatim string (RESP3)"},
}

// rtValueField is where Client.Do puts the payload of each reply type.
var rtValueField = map[string]string{
	"simple": "Str", "bulk": "Str", "verbatim": "Str", "int": "Int", "double": "Float", "bool": "Bool",
	"big": "BigN", "array": "Array", "set": "Array", "map": "Map", "null": "",
}

// rtTyName is the exported Type constant of a render prefix.
var rtTyName = map[string]string{
	"simple": "TySimple", "int": "TyInt", "bulk": "TyBulk", "null": "TyNull", "bool": "TyBool",
	"double": "TyDouble", "big": "TyBigInt", "verbatim": "TyVerbatim", "array": "TyArray", "set": "TySet", "map": "TyMap",
}

func rtRenderKind(r string) string {
	if i := strings.IndexAny(r, "([{"); i >= 0 {
		return r[:i]
	}
	return r
}

func rtSupportReplyRows(t *testing.T) []supporttable.Row {
	cells := rtReplyCells("k")
	var rows []supporttable.Row
	for _, g := range rtReplyGroups {
		var first *rtReplyCell
		for i := range cells {
			c := &cells[i]
			if c.name == g.prefix || strings.HasPrefix(c.name, g.prefix+"_") {
				if first == nil {
					first = c
				}
			}
		}
		if first == nil {
			t.Fatalf("reply group %q has no cell", g.prefix)
		}
		k3, k2 := rtRenderKind(first.r3), rtRenderKind(first.r2)
		ret := fmt.Sprintf("`Value.Type` is `%s`", rtTyName[k3])
		if f := rtValueField[k3]; f != "" {
			ret += ", the payload is in `Value." + f + "`"
		}
		if k3 != k2 {
			ret += fmt.Sprintf("; on RESP2 the server sends `%s` instead", rtTyName[k2])
			if f := rtValueField[k2]; f != "" {
				ret += ", with the payload in `Value." + f + "`"
			}
		}
		col := supporttable.NewCollector(g.id, g.label, ret)
		col.Cell("Do", "resp3", "")
		col.Cell("Do", "resp2", "")
		row := col.Row()
		row.Covers = []string{"TestRESPReplyTypes"}
		rows = append(rows, row)
	}
	return rows
}

// rtHelperLabels name the typed-helper rows.
var rtHelperLabels = map[string]string{
	"bulk": "Bulk string", "integer": "Integer", "status": "Simple string",
	"double": "Double (RESP3)", "bool_true": "Boolean true (RESP3)", "bool_false": "Boolean false (RESP3)",
	"big_number": "Big number (RESP3)", "verbatim": "Verbatim string (RESP3)", "set": "Set (RESP3)",
	"array_of_bulk": "Array of bulk strings", "array_of_int": "Array of integers (an EVAL reply)",
	"map_flat_on_resp2": "Map (RESP3), a flat array on RESP2",
}

// rtBugHelpers says which helpers a known-bug note on a helper row is about.
func rtBugHelpers(piece string) []string {
	switch {
	case strings.Contains(piece, "DoString/DoInt"):
		return []string{"DoString", "DoInt"}
	case strings.Contains(piece, "DoSlice"):
		return []string{"DoSlice"}
	}
	panic("helper bug note names no helper: " + piece)
}

func rtSupportHelperRows() []supporttable.Row {
	var rows []supporttable.Row
	for _, r := range rtHelperRows() {
		label, ok := rtHelperLabels[r.name]
		if !ok {
			panic("no label for helper row " + r.name)
		}
		col := supporttable.NewCollector("helpers-"+strings.ReplaceAll(r.name, "_", "-"), label, "")
		var byDesign []string
		for _, h := range []struct{ name, want string }{{"DoString", r.str}, {"DoInt", r.integer}, {"DoBool", r.boolean}, {"DoSlice", r.slice}} {
			if h.want == "ERR" {
				byDesign = append(byDesign, h.name)
				continue
			}
			for _, p := range []struct{ scope, bug string }{{"resp3", r.bug3}, {"resp2", r.bug2}} {
				gap := ""
				for _, piece := range strings.Split(p.bug, "; ") {
					if piece == "" {
						continue
					}
					for _, bh := range rtBugHelpers(piece) {
						if bh == h.name {
							// A helper can be hit by two notes (R1 and R2 on DoSlice of a map);
							// both are recorded below.
							col.Cell(h.name, p.scope, strings.Fields(piece)[0])
							gap = "recorded"
						}
					}
				}
				if gap == "" {
					col.Cell(h.name, p.scope, "")
				}
			}
		}
		row := col.Row()
		row.Covers = []string{"TestRESPTypedHelpers"}
		row.ByDesign = byDesign
		rows = append(rows, row)
	}
	return rows
}

func rtSupportBehaviourRows() []supporttable.Row {
	mib := func(n int) string { return fmt.Sprintf("%d MiB", n>>20) }
	maxOK, minBad := 0, 1<<62
	for _, n := range rtBulkSizes {
		maxOK = max(maxOK, n)
	}
	for _, n := range rtBulkSizesOverLinuxCap {
		minBad = min(minBad, n)
	}
	ok := supporttable.NewCollector("large-values", fmt.Sprintf("Values up to %s, and values that contain CRLF, $, + or any byte", mib(maxOK)), "the same bytes back")
	for _, d := range []string{"Set", "SetBytes", "Get", "GetBytes", "Do"} {
		ok.Cell(d, "all", "")
	}
	okRow := ok.Row()
	okRow.Covers = []string{"TestRESPLargeAndBinaryBulk"}
	big := supporttable.NewCollector("huge-values", fmt.Sprintf("Values of %s and more", mib(minBad)), "")
	for _, d := range []string{"Set", "SetBytes"} {
		big.Cell(d, "linux", "R3")
	}
	bigRow := big.Row()
	bigRow.Covers = []string{"TestRESPLargeAndBinaryBulk"}
	return []supporttable.Row{
		okRow, bigRow,
		{ID: "error-replies", Label: "Error replies", Returns: "a `*RedisError` whose `Prefix` is the first word (`WRONGTYPE`, `ERR`, or your script's own); the connection stays usable", Works: []string{"Do"}, Covers: []string{"TestRESPReplyTypes"}},
		{ID: "typed-doubles", Label: "Commands that reply with a number with a fraction", Returns: "`float64`, infinities included, the same on RESP2 and RESP3", Works: []string{"ZScore", "IncrByFloat", "HIncrByFloat", "ZIncrBy"}, Covers: []string{"TestRESPTypedDoubles"}},
		{ID: "typed-aggregates", Label: "Hashes, sets, lists and MGet", Returns: "`map[string]string` or `[]string`; a missing key gives an empty map or slice; fields and members may be empty or not ASCII", Works: []string{"HGetAll", "SMembers", "SIsMember", "LRange", "MGet"}, Covers: []string{"TestRESPTypedAggregates"}},
		{ID: "push", Label: "Push messages on a command connection (client-side caching)", Returns: "delivered to the `WithOnPush` callback; the replies are not disturbed", Works: []string{"WithOnPush"}, Covers: []string{"TestRESPPushOnCommandConn"}},
	}
}

// rtGaps is the one hand-maintained part of the table: what each classified
// failure means for a user, the error they see (empty when the value is silently
// wrong), and where the fix and the issue are. Release is supporttable.V160 for a
// fix meant for v1.6.0 and supporttable.V161 for a gap that stays through v1.6.0.
// TestSupportTable fails for an entry no row refers to, so drop an entry when its
// classification leaves the matrix.
var rtGaps = []supporttable.Gap{
	{ID: "R1", Release: supporttable.V160,
		Summary: "On RESP3 (the default), `DoString` and `DoInt` fail on a double, boolean or big number reply, and `DoSlice` fails on a map reply, where RESP2 returns the value.",
		Error:   "celeris-redis: expected string reply, got double"},
	{ID: "R2", Release: supporttable.V160,
		Summary: "`DoSlice` returns an empty string for every element that is an integer, a double, a boolean or a nested array."},
	{ID: "R3", Release: supporttable.V160,
		Summary: "On Linux, a single value of 4 MiB or more cannot be written (`SET`, `RPUSH`, `HSET` and the other commands that carry one large argument). macOS and Windows do not have this limit.",
		Error:   "celeris/engine: worker write queue full"},
}
