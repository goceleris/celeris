package mono

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The scan test of the package (see the package documentation, "The scan test
// and its PENDING list"): syntax only, no type information, directory walks.
// Over-inclusive on purpose: a match that is not a wall stamp is listed in
// stays with its reason.

// site is one kind of match in one function of one file: never a line.
type site struct {
	File string // slash-separated, relative to the module root
	Func string // "Recv.Name", "Name", or "var name" for a package-level variable
	What string // ".UnixNano", "time.Unix", ".Round(0)", "mix", ...
}

func (s site) String() string { return s.File + " | " + s.Func + " | " + s.What }

// scanRoots are the directories the scan walks, recursively (a new package
// under one of them is scanned with no edit here), and the exclusions, each
// with its reason. "." is the module's root package alone.
var (
	scanRoots = []string{
		".", // the root package only (non-recursive)
		"internal/engine",
		"internal/conn",
		"internal/protocol",
		"middleware/websocket",
		"middleware/sse",
	}
	scanExcluded = map[string]string{
		"internal/engine/std": "net/http's own timeouts and time.Since(epoch) (a monotonic write clock); no stamp of this package's clock",
	}
)

var (
	wallMethods = map[string]bool{"UnixNano": true, "UnixMilli": true, "UnixMicro": true, "Unix": true}
	timeFuncs   = map[string]bool{"Unix": true, "UnixMilli": true, "UnixMicro": true}
	// stampFields are the fields whose value is a stamp of the clock (or a
	// deadline on it). A test that writes a wall reading into one is a fixture
	// that has to move to mono.Now().
	stampFields = map[string]bool{
		"lastActivity": true, "cachedNow": true, "lastSweep": true, "sweepNext": true,
		"closeSince": true, "writeSince": true, "closeSampled": true, "detectDeadline": true,
		"drainDeadline": true, "releaseAtNanos": true, "linkArmedAt": true, "recvStallSince": true,
		"HeaderDeadlineNs": true, "IdleDeadlineNs": true, "StartTimeNs": true, "NowNs": true,
		"startTimeNs": true, "armedAt": true, "ReqDL": true, "TombDL": true, "HdrDL": true, "WrDL": true,
		"d0At": true, "d0Check": true, "expiredReq": true,
	}
	// stampFuncs are the functions that take a stamp (a `now`) as an argument:
	// a test that hands one a wall reading is a fixture too.
	stampFuncs = map[string]bool{
		"Sweep": true, "ExtendIdle": true, "ExpiredRequests": true, "ExpiredWrites": true,
		"ExpireRequest": true, "DropTombstones": true, "ArmRequest": true, "ArmHeaderDeadlineAt": true,
		"sweepH2Deadlines": true, "closeIdleH1": true, "drainRead": true, "acceptAll": true,
		"processCQE": true, "handleSend": true, "handleRecv": true, "completeSend": true,
		"noteClosingAcked": true, "detachedStalled": true, "attachFD": true, "attachAdoptedFD": true,
		"drainAdoptQueue": true, "onAcceptedFD": true, "handleAccept": true, "closeStalled": true,
		"closeRefused": true, "sweepDue": true, "stallLookDue": true, "noteH2Owed": true,
		"holdUnreadAtGate": true, "parkedBelowRate": true, "sweepWriteClock": true,
	}
)

func moduleRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file))) // internal/mono/scan_test.go
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	return root
}

// funcName names the top-level declaration that holds n.
func funcName(d ast.Decl) string {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil && len(d.Recv.List) == 1 {
			rt := d.Recv.List[0].Type
			if st, ok := rt.(*ast.StarExpr); ok {
				rt = st.X
			}
			switch x := rt.(type) {
			case *ast.Ident:
				return x.Name + "." + d.Name.Name
			case *ast.IndexExpr:
				if id, ok := x.X.(*ast.Ident); ok {
					return id.Name + "." + d.Name.Name
				}
			case *ast.IndexListExpr:
				if id, ok := x.X.(*ast.Ident); ok {
					return id.Name + "." + d.Name.Name
				}
			}
		}
		return d.Name.Name
	case *ast.GenDecl:
		for _, sp := range d.Specs {
			switch sp := sp.(type) {
			case *ast.ValueSpec:
				return d.Tok.String() + " " + sp.Names[0].Name
			case *ast.TypeSpec:
				return "type " + sp.Name.Name
			}
		}
	}
	return "decl"
}

// wallWhat reports what a call is, if it reads or strips a wall-clock reading.
func wallWhat(c *ast.CallExpr) string {
	se, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	name := se.Sel.Name
	if id, ok := se.X.(*ast.Ident); ok && id.Name == "time" && timeFuncs[name] && len(c.Args) > 0 {
		return "time." + name
	}
	switch {
	case wallMethods[name] && len(c.Args) == 0:
		return "." + name
	case name == "Round" && len(c.Args) == 1:
		if bl, ok := c.Args[0].(*ast.BasicLit); ok && bl.Value == "0" {
			return ".Round(0)"
		}
	case (name == "UTC" || name == "Local") && len(c.Args) == 0:
		return "." + name
	case name == "In" && len(c.Args) == 1:
		return ".In"
	}
	return ""
}

// scanSource returns the wall matches of one non-test file, by site, with
// their counts, and the sites whose statement mixes a mono reading with a
// drainclock or deferlinger one.
func scanSource(filename string, src any) (map[site]int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	out := map[site]int{}
	for _, d := range f.Decls {
		fn := funcName(d)
		ast.Inspect(d, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if w := wallWhat(c); w != "" {
					out[site{filename, fn, w}]++
				}
			}
			return true
		})
		// The mix check counts the OUTERMOST statement or expression in which
		// the two meet, once.
		ast.Inspect(d, func(n ast.Node) bool {
			if n != nil && stmtMixes(n) {
				out[site{filename, fn, "mixes mono with drainclock/deferlinger"}]++
				return false
			}
			return true
		})
	}
	return out, nil
}

// stmtMixes reports whether n is a statement or expression in which a call of
// this package and a call of drainclock (Now, At) or deferlinger (Now) meet: a
// comparison, an arithmetic expression, an assignment, a declaration, a
// return or a call's arguments. The scales are ~1.8e18 against a small one;
// either order is a deadline that fires at once or never. Nested blocks and
// function literals are not part of the statement.
func stmtMixes(n ast.Node) bool {
	switch n.(type) {
	case *ast.BinaryExpr, *ast.AssignStmt, *ast.ReturnStmt, *ast.ValueSpec, *ast.CallExpr, *ast.KeyValueExpr:
	default:
		return false
	}
	var small, wall bool
	ast.Inspect(n, func(m ast.Node) bool {
		switch m.(type) {
		case *ast.BlockStmt, *ast.FuncLit:
			return false
		}
		c, ok := m.(*ast.CallExpr)
		if !ok {
			return true
		}
		se, ok := c.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := se.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch {
		case id.Name == "drainclock" && (se.Sel.Name == "Now" || se.Sel.Name == "At"), id.Name == "deferlinger" && se.Sel.Name == "Now":
			small = true
		case id.Name == "mono" && (se.Sel.Name == "Now" || se.Sel.Name == "FromWall" || se.Sel.Name == "Time" || se.Sel.Name == "FromTime"):
			wall = true
		}
		return true
	})
	return small && wall
}

// fixtureFuncs returns the test functions of one _test.go file that write a
// wall reading into a stamp field or hand one to a function that takes a
// stamp, as the names of the functions (a function literal belongs to its
// top-level declaration).
func fixtureFuncs(filename string, src any) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range f.Decls {
		var reads, touches bool
		ast.Inspect(d, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if w := wallWhat(n); w == ".UnixNano" || w == ".UnixMilli" || w == ".UnixMicro" || strings.HasPrefix(w, "time.") {
					reads = true
				}
				switch fun := n.Fun.(type) {
				case *ast.SelectorExpr:
					if stampFuncs[fun.Sel.Name] {
						touches = true
					}
					if fun.Sel.Name == "Store" || fun.Sel.Name == "Add" || fun.Sel.Name == "CompareAndSwap" || fun.Sel.Name == "Swap" {
						if inner, ok := fun.X.(*ast.SelectorExpr); ok && stampFields[inner.Sel.Name] {
							touches = true
						}
					}
				case *ast.Ident:
					if stampFuncs[fun.Name] {
						touches = true
					}
				}
			case *ast.AssignStmt:
				for _, l := range n.Lhs {
					switch l := l.(type) {
					case *ast.SelectorExpr:
						if stampFields[l.Sel.Name] {
							touches = true
						}
					case *ast.IndexExpr:
						if se, ok := l.X.(*ast.SelectorExpr); ok && stampFields[se.Sel.Name] {
							touches = true
						}
					}
				}
			case *ast.KeyValueExpr:
				if id, ok := n.Key.(*ast.Ident); ok && stampFields[id.Name] {
					touches = true
				}
			}
			return true
		})
		if reads && touches {
			out = append(out, funcName(d))
		}
	}
	return out, nil
}

// scanTree walks the roots and returns the production matches and the
// fixture functions ("file | func").
func scanTree(t testing.TB, root string) (prod map[site]int, fixtures map[string]bool) {
	t.Helper()
	prod, fixtures = map[site]int{}, map[string]bool{}
	for _, r := range scanRoots {
		dir := filepath.Join(root, filepath.FromSlash(r))
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if _, skip := scanExcluded[rel]; skip || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				if r == "." && path != dir {
					return filepath.SkipDir // the root package alone
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				names, err := fixtureFuncs(rel, mustRead(t, path))
				if err != nil {
					return fmt.Errorf("%s: %w", rel, err)
				}
				for _, n := range names {
					fixtures[rel+" | "+n] = true
				}
				return nil
			}
			m, err := scanSource(rel, mustRead(t, path))
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			for k, v := range m {
				prod[k] += v
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", r, err)
		}
	}
	return prod, fixtures
}

func mustRead(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestNoWallStamps: every wall read of the scanned tree is listed, once, with
// its count, in stays (for good) or pending (to move), and every entry of both
// is still there. A routing PR deletes its pending entries; a wall read added
// later, or re-added by a revert, is in neither table and fails here.
func TestNoWallStamps(t *testing.T) {
	prod, _ := scanTree(t, moduleRoot(t))
	listed := map[site]bool{}
	for s := range stays {
		listed[s] = true
	}
	for s := range pending {
		if listed[s] {
			t.Errorf("%s is in stays and in pending: decide which", s)
		}
		listed[s] = true
	}
	var unlisted, stale, count []string
	for s, n := range prod {
		e, ok := stays[s]
		if p, isP := pending[s]; isP {
			e, ok = p, true
		}
		switch {
		case !ok:
			unlisted = append(unlisted, fmt.Sprintf("%s (x%d)", s, n))
		case e.Count != n:
			count = append(count, fmt.Sprintf("%s: found %d, listed %d", s, n, e.Count))
		}
	}
	for s := range listed {
		if _, ok := prod[s]; !ok {
			stale = append(stale, s.String())
		}
	}
	for _, l := range []struct {
		what string
		xs   []string
		hint string
	}{
		{"wall reads that are not listed", unlisted, "route the stamp through internal/mono (mono.Now, mono.FromTime, ...), or, if it is not a stamp, add it to stays with its reason"},
		{"entries whose count is not the count found", count, "fix the count; a routing PR deletes the entry instead"},
		{"stale entries (the match is gone)", stale, "delete the entry: the site moved"},
	} {
		if len(l.xs) > 0 {
			sort.Strings(l.xs)
			t.Errorf("%s (%d):\n  %s\n%s", l.what, len(l.xs), strings.Join(l.xs, "\n  "), l.hint)
		}
	}
}

// TestNoWallFixtures is the same for the test files: a test function that
// writes a wall reading into a stamp field, or hands one to a function that
// takes a stamp, is correct only while the clock and the wall agree, which the
// skew canary does not allow. It moves to mono.Now() with the routing PR of
// its package, and its entry goes.
func TestNoWallFixtures(t *testing.T) {
	_, fixtures := scanTree(t, moduleRoot(t))
	var unlisted, stale []string
	for f := range fixtures {
		if !pendingFixtures[f] {
			unlisted = append(unlisted, f)
		}
	}
	for f := range pendingFixtures {
		if !fixtures[f] {
			stale = append(stale, f)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	if len(unlisted) > 0 {
		t.Errorf("test functions that write a wall reading into a stamp field (%d): use mono.Now() instead (the stamps are on its clock):\n  %s", len(unlisted), strings.Join(unlisted, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("stale pendingFixtures entries (%d): delete them:\n  %s", len(stale), strings.Join(stale, "\n  "))
	}
}

// TestPendingIsEmptyWhenFlipped holds the flip back in code: with flipped true,
// no site and no fixture may be left to move.
func TestPendingIsEmptyWhenFlipped(t *testing.T) {
	if flipped && (len(pending) != 0 || len(pendingFixtures) != 0) {
		t.Fatalf("flipped is true with %d pending sites and %d pending fixtures: route them first", len(pending), len(pendingFixtures))
	}
	if !flipped {
		t.Logf("routing phase: %d sites and %d fixtures still to move", len(pending), len(pendingFixtures))
	}
}

// TestScanDump prints what the scan sees, as the tables' lines, for writing or
// updating them: MONO_SCAN_DUMP=1 go test -run TestScanDump -v ./internal/mono
func TestScanDump(t *testing.T) {
	if os.Getenv("MONO_SCAN_DUMP") == "" {
		return // not a skip: CI's tally fails the step on any SKIP line
	}
	prod, fixtures := scanTree(t, moduleRoot(t))
	var lines []string
	for s, n := range prod {
		lines = append(lines, fmt.Sprintf("{%q, %q, %q}: %d", s.File, s.Func, s.What, n))
	}
	slices.Sort(lines)
	fmt.Println("PROD")
	fmt.Println(strings.Join(lines, "\n"))
	lines = lines[:0]
	for f := range fixtures {
		lines = append(lines, f)
	}
	slices.Sort(lines)
	fmt.Println("FIXTURES")
	fmt.Println(strings.Join(lines, "\n"))
}

// The scanner itself.

func TestScannerMatches(t *testing.T) {
	src := `package p
import ("time"; "github.com/goceleris/celeris/internal/drainclock"; "github.com/goceleris/celeris/internal/mono")
var nowNano = func() int64 { return time.Now().UnixNano() }
type W struct{ lastActivity int64 }
func (w *W) a() { w.lastActivity = time.Now().UnixNano(); _ = time.Now().Unix(); _ = time.Unix(0, 5); _ = time.Now().Round(0).UTC().Local() }
func b(t time.Time) { _ = t.UnixMilli(); _ = t.In(nil); _ = t.Round(time.Second) }
func c() { _ = mono.Now() + drainclock.Now() }
func d() { x := mono.Now(); y := drainclock.Now(); _, _ = x, y }
func e() int64 { return drainclock.At(time.Time{}) - mono.FromTime(time.Time{}) }
`
	got, err := scanSource("p.go", src)
	if err != nil {
		t.Fatal(err)
	}
	want := map[site]int{
		{"p.go", "var nowNano", ".UnixNano"}:                    1,
		{"p.go", "W.a", ".UnixNano"}:                            1,
		{"p.go", "W.a", ".Unix"}:                                1,
		{"p.go", "W.a", "time.Unix"}:                            1,
		{"p.go", "W.a", ".Round(0)"}:                            1,
		{"p.go", "W.a", ".UTC"}:                                 0, // placeholder removed below
		{"p.go", "b", ".UnixMilli"}:                             1,
		{"p.go", "b", ".In"}:                                    1,
		{"p.go", "c", "mixes mono with drainclock/deferlinger"}: 1,
		{"p.go", "e", "mixes mono with drainclock/deferlinger"}: 1,
	}
	delete(want, site{"p.go", "W.a", ".UTC"})
	want[site{"p.go", "W.a", ".Local"}] = 1
	want[site{"p.go", "W.a", ".UTC"}] = 1
	for s, n := range want {
		if got[s] != n {
			t.Errorf("%s: got %d, want %d", s, got[s], n)
		}
	}
	for s, n := range got {
		if _, ok := want[s]; !ok {
			t.Errorf("unexpected match %s (x%d)", s, n)
		}
	}
	// Two statements, one value of each scale in each: not a mix.
	if got[site{"p.go", "d", "mixes mono with drainclock/deferlinger"}] != 0 {
		t.Error("separate statements were reported as a mix")
	}
}

func TestScannerFixtures(t *testing.T) {
	src := `package p
import "time"
func TestA(t *testing.T) { cs.lastActivity = time.Now().UnixNano() - 5 }
func TestB(t *testing.T) { h.HeaderDeadlineNs.Store(time.Now().UnixNano()) }
func TestC(t *testing.T) { w.checkSweep(time.Now().UnixNano()); m.ExpiredWrites(time.Now().UnixNano(), nil) }
func TestD(t *testing.T) { _ = time.Now().UnixNano() }
func TestE(t *testing.T) { cs.lastActivity = 0 }
func TestF(t *testing.T) { s := &state{lastActivity: time.Now().UnixNano()}; _ = s }
`
	got, err := fixtureFuncs("x_test.go", src)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"TestA", "TestB", "TestC", "TestF"}; !slices.Equal(got, want) {
		t.Fatalf("fixtures = %v, want %v", got, want)
	}
}

// The scan reaches the packages it names, so that a rename of a root is not
// silent: every root must hold at least one scanned production file.
func TestScanRootsExist(t *testing.T) {
	root := moduleRoot(t)
	for _, r := range scanRoots {
		n := 0
		_ = filepath.WalkDir(filepath.Join(root, filepath.FromSlash(r)), func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				n++
			}
			return nil
		})
		if n == 0 {
			t.Errorf("scan root %q holds no non-test Go file (renamed or removed?)", r)
		}
	}
	for r := range scanExcluded {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(r))); err != nil {
			t.Errorf("excluded directory %q does not exist (the exclusion is out of date): %v", r, err)
		}
	}
}
