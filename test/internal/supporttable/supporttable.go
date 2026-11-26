// Package supporttable is the shared shape and the golden-file check of the
// per-driver "supported types" tables (test/conformance/<driver>/testdata/
// supported.json). The tables are produced by a test in each conformance
// package from the same declarations the round-trip matrix runs (its cases and
// its known-bug classifiers), without a server, so the table cannot say
// something the matrix does not. The documentation site (goceleris/docs)
// renders its driver pages from these files.
//
// A test calls Verify with the table it built. Verify fails when the committed
// file differs from the table, and rewrites the file when
// CELERIS_UPDATE_SUPPORT_TABLES=1 is set:
//
//	CELERIS_UPDATE_SUPPORT_TABLES=1 go test -tags postgres -run '^TestSupportTable$' ./test/conformance/postgres/
package supporttable

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// Version is the schema version of the files. The documentation site refuses a
// file with a version it does not know.
const Version = 1

// UpdateEnv is the environment variable that makes Verify rewrite the file.
const UpdateEnv = "CELERIS_UPDATE_SUPPORT_TABLES"

// Release names the release a gap is meant to be closed in.
const (
	V160 = "v1.6.0" // a correctness fix that ships with v1.6.0
	V161 = "v1.6.1" // a known gap that stays through v1.6.0, tracked for v1.6.1
)

// Table is one driver's table.
type Table struct {
	Version  int       `json:"version"`
	Driver   string    `json:"driver"`
	Regen    string    `json:"regenerate"`
	Sections []Section `json:"sections"`
	// Gaps describes every gap a row refers to, by id. Only the gaps that some
	// row refers to are listed.
	Gaps []Gap `json:"gaps"`
}

// Section is one table of a page.
type Section struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Rows  []Row  `json:"rows"`
}

// Row is one type, reply or behaviour.
type Row struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Returns is what the driver hands back for the row, in Go terms (a
	// destination type, a RESP type, an error), per the section's convention.
	Returns string `json:"returns,omitempty"`
	// Works lists the destinations (Go types, helper methods, calls) that give
	// the right value in every variant the matrix runs.
	Works []string `json:"works"`
	// Null lists the destinations that take a NULL of the type (sql.Null*).
	Null []string `json:"null,omitempty"`
	// ByDesign lists destinations that return an error on purpose, for
	// example Client.DoInt on a bulk string that is not a number.
	ByDesign []string `json:"by_design,omitempty"`
	// Fails lists the destinations that give a wrong value or an error today,
	// with the gap and the variants it applies to.
	Fails []Failure `json:"fails,omitempty"`
	// Covers names the tests that exercise the row (behaviour rows only).
	Covers []string `json:"covers,omitempty"`
}

// Failure is one destination that does not work in some variants.
type Failure struct {
	Dest  string   `json:"dest"`
	Gap   string   `json:"gap"`
	Scope []string `json:"scope"`
}

// Gap is a known gap: what goes wrong for a user, what fixes it and where it is
// tracked. Release is V160 when the fix is meant for v1.6.0 and V161 when the
// behaviour stays through v1.6.0. Fix and Issue are "#N" or empty when none has
// been opened yet (the documentation build lists the empty ones).
type Gap struct {
	ID      string `json:"id"`
	Release string `json:"release"`
	// Summary is one plain sentence for a user of the driver.
	Summary string `json:"summary"`
	// Error is the text the user sees, or empty when the wrong value is silent.
	Error string `json:"error,omitempty"`
	Fix   string `json:"fix,omitempty"`
	Issue string `json:"issue,omitempty"`
}

// Collector turns the cells of a matrix into rows: it records, per row and
// destination, the gaps and the scopes in which the destination fails.
type Collector struct {
	row   *Row
	fails map[string]map[string]map[string]bool // dest -> gap -> scope set
	order []string                              // dest order of first appearance
	dests map[string]bool
}

// NewCollector starts a row.
func NewCollector(id, label, returns string) *Collector {
	return &Collector{
		row:   &Row{ID: id, Label: label, Returns: returns},
		fails: map[string]map[string]map[string]bool{},
		dests: map[string]bool{},
	}
}

// Cell records one destination in one variant: gap is empty when it works.
func (c *Collector) Cell(dest, scope, gap string) {
	if !c.dests[dest] {
		c.dests[dest] = true
		c.order = append(c.order, dest)
	}
	if gap == "" {
		return
	}
	if c.fails[dest] == nil {
		c.fails[dest] = map[string]map[string]bool{}
	}
	if c.fails[dest][gap] == nil {
		c.fails[dest][gap] = map[string]bool{}
	}
	c.fails[dest][gap][scope] = true
}

// NullDest records a destination that receives a NULL of the row's type.
func (c *Collector) NullDest(dest string) {
	c.row.Null = append(c.row.Null, dest)
}

// Row returns the finished row. A destination that fails in no variant is in
// Works; one that fails in some is in Fails (and not in Works).
func (c *Collector) Row() Row {
	r := *c.row
	r.Works = []string{}
	for _, d := range c.order {
		gaps := c.fails[d]
		if len(gaps) == 0 {
			r.Works = append(r.Works, d)
			continue
		}
		ids := make([]string, 0, len(gaps))
		for g := range gaps {
			ids = append(ids, g)
		}
		sort.Strings(ids)
		for _, g := range ids {
			scopes := make([]string, 0, len(gaps[g]))
			for s := range gaps[g] {
				scopes = append(scopes, s)
			}
			sort.Strings(scopes)
			r.Fails = append(r.Fails, Failure{Dest: d, Gap: g, Scope: scopes})
		}
	}
	return r
}

// Finish fills Gaps with the entries of meta that a row refers to, and fails
// the test for a reference with no entry and for an entry no row refers to
// (a stale entry would keep a fixed gap on the page).
func Finish(t testing.TB, tbl *Table, meta []Gap) {
	t.Helper()
	byID := map[string]Gap{}
	for _, g := range meta {
		if _, dup := byID[g.ID]; dup {
			t.Fatalf("gap %s is declared twice", g.ID)
		}
		byID[g.ID] = g
	}
	used := map[string]bool{}
	for _, s := range tbl.Sections {
		for _, r := range s.Rows {
			for _, f := range r.Fails {
				if _, ok := byID[f.Gap]; !ok {
					t.Errorf("row %s/%s refers to gap %q, which has no entry in the gap list", s.ID, r.ID, f.Gap)
				}
				used[f.Gap] = true
			}
		}
	}
	tbl.Gaps = nil
	for _, g := range meta {
		if !used[g.ID] {
			t.Errorf("gap %s is in the gap list but no row refers to it: drop it (its fix landed) or fix the classifier", g.ID)
			continue
		}
		if g.Release != V160 && g.Release != V161 {
			t.Errorf("gap %s: release %q, want %q or %q", g.ID, g.Release, V160, V161)
		}
		tbl.Gaps = append(tbl.Gaps, g)
	}
	sort.Slice(tbl.Gaps, func(i, j int) bool { return tbl.Gaps[i].ID < tbl.Gaps[j].ID })
}

// Verify compares tbl with the golden file at path and fails on a difference;
// with UpdateEnv=1 it rewrites the file instead.
func Verify(t testing.TB, path string, tbl Table) {
	t.Helper()
	tbl.Version = Version
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&tbl); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if os.Getenv(UpdateEnv) == "1" {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (generate it with %s=1)", path, err, UpdateEnv)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Fatalf("%s is out of date with the round-trip matrix: regenerate it with\n\t%s=1 %s\nand commit the result (the documentation pages are built from it)", path, UpdateEnv, tbl.Regen)
	}
}

// CheckCovers fails the test unless the tests named by the rows' Covers exist
// in the given test files, and every top-level test function of those files is
// named by some row. It is how a hand-declared row (a behaviour the matrix
// checks inside a test body, not in a table) stays tied to the test that checks
// it: a new matrix test without a row, or a row whose test was removed or
// renamed, fails here.
func CheckCovers(t testing.TB, files []string, secs []Section) {
	t.Helper()
	fset := token.NewFileSet()
	have := map[string]bool{}
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && strings.HasPrefix(fd.Name.Name, "Test") {
				have[fd.Name.Name] = true
			}
		}
	}
	covered := map[string]bool{}
	for _, s := range secs {
		for _, r := range s.Rows {
			for _, c := range r.Covers {
				covered[c] = true
				if !have[c] {
					t.Errorf("row %s/%s names test %s, which does not exist in %v", s.ID, r.ID, c, files)
				}
			}
		}
	}
	for n := range have {
		if !covered[n] {
			t.Errorf("test %s is not named by any row of the table: add a row for what it checks", n)
		}
	}
}
