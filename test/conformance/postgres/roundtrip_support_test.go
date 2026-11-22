//go:build postgres

package postgres_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/goceleris/celeris/test/internal/supporttable"
)

// TestSupportTable builds the PostgreSQL "supported types" table from the
// declarations the round-trip matrix runs (rtCases, rtArgCases and the
// classifier rtKnownBug) and checks it against testdata/supported.json, which
// the documentation site renders. It needs no server and does not read
// rtKnownBugs, so it gives the same table with and without the pgrtbugs tag.
//
// When a fix removes a classification, this test fails until the file is
// regenerated, so the documented support never lags the matrix:
//
//	CELERIS_UPDATE_SUPPORT_TABLES=1 go test -tags postgres -run '^TestSupportTable$' ./test/conformance/postgres/
func TestSupportTable(t *testing.T) {
	tbl := supporttable.Table{
		Driver: "postgres",
		Regen:  "go test -tags postgres -run '^TestSupportTable$' ./test/conformance/postgres/",
	}
	tbl.Sections = []supporttable.Section{
		{ID: "column-types", Title: "Column types read back", Rows: rtSupportTypeRows(t)},
		{ID: "argument-kinds", Title: "Go values passed as arguments", Rows: rtSupportArgRows()},
	}
	for _, sec := range tbl.Sections {
		rtUniqueIDs(t, sec.Rows)
	}
	supporttable.Finish(t, &tbl, rtGaps)
	supporttable.Verify(t, "testdata/supported.json", tbl)
}

// rtVariants are the api/protocol pairs the matrix reads with (the same list
// rtOpenVariants builds). Arguments need the extended protocol, so the
// argument rows skip "simple".
var rtVariants = [][2]string{
	{"sql", "simple"}, {"sql", "ext"}, {"sql", "nocache"}, {"sql", "prep"},
	{"pool", "simple"}, {"pool", "ext"}, {"pool", "nocache"},
}

// rtCode puts the Go type at the start of s in a markdown code span.
func rtCode(s string) string {
	if strings.HasPrefix(s, "Not ") {
		return s
	}
	first, rest, _ := strings.Cut(s, " ")
	if rest != "" {
		rest = " " + rest
	}
	return "`" + first + "`" + rest
}

// rtGapID is the bug id a classification string starts with ("B3 no codec: ...").
func rtGapID(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

// rtDestName is the Go type of a destination, as a user writes it.
func rtDestName(dest any) string {
	return strings.ReplaceAll(fmt.Sprintf("%T", dest), "uint8", "byte")
}

// rtRowLabel names the row a case belongs to: its column type, with the
// lengths and the user-created enum named the way a user reads them.
func rtRowLabel(c *rtCase) string {
	switch {
	case c.kind == "datebc":
		return "date (BC, a year before 1)"
	case c.kind == "tsfar":
		return "timestamp, timestamptz (before the year 1708 or after 2292)"
	case c.kind == "arr2d":
		return "int4[][] (two-dimensional)"
	case c.typ == "%ENUM%":
		return "enum (a type you create)"
	case c.typ == "%ENUM%[]":
		return "enum[]"
	case strings.HasPrefix(c.typ, "varchar("):
		return "varchar(n)"
	case strings.HasPrefix(c.typ, "char("):
		return "char(n)"
	}
	return c.typ
}

// rtReturns is what a read into `any` gives, per row label. These are the
// driver's contract for each type (see the codecs in
// internal/driver/postgres/protocol); the matrix asserts them through rtWant.
var rtReturns = map[string]string{
	"bool": "bool", "int2": "int64", "int4": "int64", "int8": "int64",
	"float4": "float64", "float8": "float64", "numeric": "string",
	"text": "string", "varchar(n)": "string", "char(n)": "[]byte", "name": "[]byte",
	"bytea": "[]byte", "json": "[]byte", "uuid": "string",
	"date": "time.Time", "date (BC, a year before 1)": "time.Time", "timestamp": "time.Time", "timestamptz": "time.Time",
	"enum (a type you create)": "[]byte",
	// No codec: the PostgreSQL text form as []byte.
	"time": "[]byte (text form)", "timetz": "[]byte (text form)", "interval": "[]byte (text form)",
	"inet": "[]byte (text form)", "cidr": "[]byte (text form)", "macaddr": "[]byte (text form)", "oid": "[]byte (text form)",
	"numeric[]": "[]byte (text form)", "date[]": "[]byte (text form)", "timestamptz[]": "[]byte (text form)", "enum[]": "[]byte (text form)",
	// Arrays with a codec: a Go slice on binary results, the literal on text results.
	"int2[]": "[]driver.Value", "int4[]": "[]driver.Value", "int8[]": "[]driver.Value", "text[]": "[]driver.Value",
	"varchar[]": "[]driver.Value", "float8[]": "[]driver.Value", "bool[]": "[]driver.Value", "uuid[]": "[]driver.Value",
	"bytea[]": "[]driver.Value", "int4[][] (two-dimensional)": "Not supported (see the known gap)",
	"timestamp, timestamptz (before the year 1708 or after 2292)": "time.Time",
}

func rtSupportTypeRows(t *testing.T) []supporttable.Row {
	var order []string
	byLabel := map[string]*supporttable.Collector{}
	for _, c := range rtCases() {
		c := c
		label := rtRowLabel(&c)
		col := byLabel[label]
		if col == nil {
			ret, ok := rtReturns[label]
			if !ok {
				t.Fatalf("row %q has no entry in rtReturns", label)
			}
			col = supporttable.NewCollector(rtSlug(label), label, rtCode(ret))
			byLabel[label] = col
			order = append(order, label)
		}
		for _, v := range rtVariants {
			scope := v[0] + "/" + v[1]
			cell := func(name, dest string) {
				col.Cell(dest, scope, rtGapID(rtKnownBug(&c, v[0], v[1], name)))
			}
			cell("any", "any")
			for i, mk := range c.nat {
				d, _ := mk()
				cell(fmt.Sprintf("native%d", i), rtDestName(d))
			}
			if c.text != "" || c.kind == "text" {
				cell("str", "*string")
			}
			if c.text != "" || c.raw != nil {
				cell("bytes", "*[]byte")
			}
		}
		if c.null != nil {
			d, _ := c.null()
			// A NULL is a length of -1 in either format: no classification applies.
			if name := rtDestName(d); !slices.Contains(col.Row().Null, name) {
				col.NullDest(name)
			}
		}
	}
	rows := make([]supporttable.Row, 0, len(order))
	for _, l := range order {
		rows = append(rows, byLabel[l].Row())
	}
	return rows
}

func rtSupportArgRows() []supporttable.Row {
	var order []string
	byLabel := map[string]*supporttable.Collector{}
	for _, c := range rtArgCases() {
		label := rtArgLabel(c.arg)
		if c.null && c.arg != nil {
			label += " holding NULL"
		}
		col := byLabel[label]
		if col == nil {
			col = supporttable.NewCollector(rtSlug(label), label, "")
			byLabel[label] = col
			order = append(order, label)
		}
		for _, v := range rtVariants {
			if v[1] == "simple" {
				continue
			}
			dest := c.typ
			if c.qualifier != "" {
				dest += " (" + c.qualifier + ")"
			}
			col.Cell(dest, v[0]+"/"+v[1], rtArgGap(c.bug, v[0]))
		}
	}
	rows := make([]supporttable.Row, 0, len(order))
	for _, l := range order {
		rows = append(rows, byLabel[l].Row())
	}
	return rows
}

// rtArgLabel names a Go argument kind the way a user writes it.
func rtArgLabel(arg any) string {
	switch arg.(type) {
	case nil:
		return "nil (SQL NULL)"
	case rtNamedString:
		return "your own defined string type"
	case rtNamedInt:
		return "your own defined integer type"
	case rtValuer:
		return "a type that implements driver.Valuer"
	case json.RawMessage:
		return "json.RawMessage"
	}
	return rtDestName(arg)
}

// rtArgGap is the gap of an argument cell. The native Pool never goes through
// database/sql's CheckNamedValue, so the conversion bug that is fixed there
// (B6, and B7 which has the same cause) is a separate gap on the Pool (B6P).
func rtArgGap(bug, api string) string {
	id := rtGapID(bug)
	if api == "pool" && (id == "B6" || id == "B7") {
		return "B6P"
	}
	return id
}

func rtSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, "[]", " array "), "*", "pointer ")) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// rtUniqueIDs fails the test when two rows of a section share an id (the
// documentation anchors rows by id).
func rtUniqueIDs(t *testing.T, rows []supporttable.Row) {
	t.Helper()
	seen := map[string]string{}
	for _, r := range rows {
		if prev, dup := seen[r.ID]; dup {
			t.Errorf("rows %q and %q have the same id %q", prev, r.Label, r.ID)
		}
		seen[r.ID] = r.Label
	}
}

// rtGaps is the one hand-maintained part of the table: what each classified
// failure means for a user of the driver, the error they see (empty when the
// value is silently wrong), and where the fix and the issue are. Release is
// supporttable.V160 for a fix meant for v1.6.0 and supporttable.V161 for a gap
// that stays through v1.6.0. A gap must be dropped here when its classification
// leaves rtKnownBug (TestSupportTable fails for a stale entry).
var rtGaps = []supporttable.Gap{
	{ID: "B1", Release: supporttable.V160, Fix: "#1333", Issue: "#1334",
		Summary: "With the native `Pool`, scanning an integer, numeric or uuid column into a `string` or a `[]byte` on a query with arguments gave the raw binary wire bytes instead of the text."},
	{ID: "B2", Release: supporttable.V160, Fix: "#1333",
		Summary: "With the native `Pool` and a query without arguments, scanning a bytea column into a `[]byte` gave the text form (`\\x00ff...`) instead of the bytes."},
	{ID: "B3", Release: supporttable.V160,
		Summary: "On a query with arguments, a prepared statement or a query the driver caches, a column of a type the driver has no decoder for (time, timetz, interval, inet, cidr, macaddr, oid, and arrays of numeric, date, timestamptz or enum) came back as raw binary bytes. The same query without arguments returned the text."},
	{ID: "B4", Release: supporttable.V161,
		Summary: "An array column cannot be scanned into a `string` or a `[]byte` on a query with arguments. With `database/sql` you get the error below. With the native `Pool` you get the raw binary bytes today and an error as well from v1.6.0 on. Scan it into an `any` and read the `[]driver.Value`, or cast it to text in the query (`SELECT col::text`). Whether an array comes back as a Go slice or as the text literal also depends on whether the query had arguments.",
		Error:   "sql: Scan error on column index 0, name \"c\": unsupported Scan, storing driver.Value type []driver.Value into type *string"},
	{ID: "B5", Release: supporttable.V160, Fix: "#1341",
		Summary: "A BC date cannot be read from a query without arguments. The same value is read correctly when the query has arguments.",
		Error:   "parsing time \"0044-03-15 BC\": extra text: \" BC\""},
	{ID: "B6", Release: supporttable.V160, Fix: "#1340",
		Summary: "With `database/sql`, the wrappers and integer widths that it normally converts for you are rejected as arguments: `sql.NullString`, `sql.NullInt64` and the other `sql.Null*` types, anything that implements `driver.Valuer`, your own defined types, pointers, and `int16`, `uint8`, `uint32` and `uint64`. Writing a NULL with `sql.NullString{}` fails the same way.",
		Error:   "celeris-postgres: encode arg 1: celeris-postgres: unsupported argument type sql.NullString; convert to bool/int64/float64/string/[]byte/time.Time or register a codec"},
	{ID: "B6P", Release: supporttable.V161,
		Summary: "The native `Pool` rejects the same values (`sql.Null*`, `driver.Valuer`, defined types, pointers, `json.RawMessage`, `int16`, `uint8`, `uint32`, `uint64`) as arguments, because it does not use the conversion `database/sql` applies. Pass the plain value instead, or use `database/sql`.",
		Error:   "celeris-postgres: encode arg 1: celeris-postgres: unsupported argument type sql.NullString; convert to bool/int64/float64/string/[]byte/time.Time or register a codec"},
	{ID: "B7", Release: supporttable.V160, Fix: "#1340",
		Summary: "With `database/sql`, `json.RawMessage` is rejected as an argument, because it is a defined type, as your own types are. Go reports its type as `jsontext.Value`.",
		Error:   "celeris-postgres: encode arg 1: celeris-postgres: unsupported argument type jsontext.Value; convert to bool/int64/float64/string/[]byte/time.Time or register a codec"},
	{ID: "B8", Release: supporttable.V161,
		Summary: "Go slices (`[]string`, `[]int64`) are not accepted as array arguments. Pass the PostgreSQL array text, such as `{a,b}`, as a `string`.",
		Error:   "celeris-postgres: encode arg 1: celeris-postgres: unsupported argument type []string; convert to bool/int64/float64/string/[]byte/time.Time or register a codec"},
	{ID: "B9", Release: supporttable.V161,
		Summary: "A `time.Time` cannot be passed to a `time` column, and a `time.Time` before year 1 cannot be passed to `timestamptz`. Pass the value as a `string`.",
		Error:   "ERROR: invalid input syntax for type time: \"2025-01-15T12:34:56.789123Z\" (SQLSTATE 22007)"},
	{ID: "B11", Release: supporttable.V161,
		Summary: "A `[16]byte` array is not accepted as a uuid argument. Pass the uuid as a `string`, or the 16 bytes as a `[]byte`.",
		Error:   "celeris-postgres: encode arg 1: celeris-postgres: unsupported argument type [16]uint8; convert to bool/int64/float64/string/[]byte/time.Time or register a codec"},
	{ID: "B12", Release: supporttable.V161,
		Summary: "A two-dimensional array is returned flattened, as one list of all its elements. A change that makes it an error is planned for v1.6.0. Read it as text, `SELECT col::text`, until it is supported."},
	{ID: "B13", Release: supporttable.V160,
		Summary: "On a query with arguments, a prepared statement or a query the driver caches, a `timestamp` or `timestamptz` before about the year 1708 or after about 2292 is read as a different time. The same query without arguments is correct."},
}
