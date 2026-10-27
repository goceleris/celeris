package integration

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// celeris#890 item 96: a test of the adaptive files of this package that cannot
// run where the io_uring standby is unavailable skips through
// skipOrFailUpswitch890 (adaptive_standby_guard_886_linux_test.go, which
// skipOrFailIfStandbyAborted886 calls too), which fails instead under
// CELERIS_REQUIRE_UPSWITCH=1. It repeats, in this
// package, the gate that internal/adaptive keeps in skipOrFailUpswitch662: the
// two cannot share a function (separate packages, and the test files of
// internal/adaptive cannot be imported), so what keeps this package's copy
// honest is this guard, the counterpart of internal/adaptive's
// TestAdaptiveSkipRouting684.
//
// It parses every adaptive_*_test.go file of the package, and helpers_test.go, and lists each t.Skip,
// t.Skipf and t.SkipNow call that is none of:
//
//   - inside a gate helper (gateHelpers890);
//   - under an `if` whose condition is true only when testing.Short() is
//     (testing.Short() itself, or a && with it): a ||, a negation or an else
//     branch is not exempt;
//   - registered in unrouted890 with the reason it has not been routed.
//
// A new raw skip fails the guard until it goes through the gate or is
// registered. Nothing runs this package's tests in CI except this guard (the
// adaptive job runs it by name): the package needs a host, see mage.

// gateHelpers890 are the functions whose job is to skip or fail; a raw skip
// inside one is the gate itself.
var gateHelpers890 = map[string]bool{
	"skipOrFailUpswitch890": true, // the gate (CELERIS_REQUIRE_UPSWITCH=1)
}

// unrouted890 are the raw skips that stay for now, by file and the string
// literal that starts the skip's message, each with how many sites carry that
// message and why they stay. The count is exact: a further site with a
// registered message fails the guard as a stale entry would.
//
// Ten are in adaptive_hybrid_test.go and three in helpers_test.go (startEngine). #890 item 95 leaves the adaptive
// skips of this package to the lane that owns that file; the table is the count
// of what is left (the guard listed these ten, startEngine's three and one more, in
// adaptive_standby_886_linux_test.go, which now goes through the gate), and a
// new one cannot join them unseen.
type unrouted890Entry struct {
	sites int
	why   string
}

const (
	newFails890  = "adaptive.New fails on config validation or a port hold (internal/adaptive/engine.go New), not on a host without io_uring: a failure there is a defect, as for epoll.New in item 95"
	startFail890 = "an engine that does not start, bind or answer: this skip cannot tell a host without io_uring from a defect"
)

var unrouted890 = map[string]unrouted890Entry{
	"adaptive_hybrid_test.go|adaptive engine not available: %v":       {5, newFails890},
	"adaptive_hybrid_test.go|engine failed to start: %v":              {2, startFail890},
	"adaptive_hybrid_test.go|engine Listen returned nil without addr": {1, startFail890},
	"adaptive_hybrid_test.go|engine did not start in time":            {1, startFail890},
	"adaptive_hybrid_test.go|engine not functional: %v":               {1, startFail890},
	// startEngine, which the adaptive tests and the epoll arms of the slowloris and pipeline tests share.
	"helpers_test.go|engine failed to start: %v":           {2, startFail890},
	"helpers_test.go|engine not functional (skipping): %v": {1, startFail890},
}

// rawSkipViolations890 lists the t.Skip* calls of one Go source that the rules
// above do not admit, as "file:line: call(message)". It returns an error for a
// source that does not parse: the guard must fail on input it cannot read, not
// report no violation.
func rawSkipViolations890(name string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []string
	called := map[*ast.SelectorExpr]bool{} // Skip selectors seen as the function of a call
	report := func(pos token.Pos, what string) {
		out = append(out, fmt.Sprintf("%s:%d: %s", filepath.Base(name), fset.Position(pos).Line, what))
	}
	var walk func(n ast.Node, fn string, short bool)
	walk = func(n ast.Node, fn string, short bool) {
		ast.Inspect(n, func(c ast.Node) bool {
			switch x := c.(type) {
			case *ast.FuncDecl:
				if x != n {
					if x.Body != nil { // a bodyless declaration holds no call
						name := x.Name.Name
						if x.Recv != nil { // a method of that name is not the gate function
							name = "(method)" + name
						}
						walk(x.Body, name, short)
					}
					return false
				}
			case *ast.IfStmt:
				if x != n && onlyWhenShort890(x.Cond) {
					walk(x.Body, fn, true)
					if x.Else != nil {
						walk(x.Else, fn, short)
					}
					return false
				}
			case *ast.CallExpr:
				// Any receiver: a subtest closure may name its *testing.T anything.
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok || !isSkipSelector890(sel) {
					return true
				}
				called[sel] = true
				if short || gateHelpers890[fn] {
					return true
				}
				lit := skipMessage890(x)
				if _, ok := unrouted890[filepath.Base(name)+"|"+lit]; ok {
					return true
				}
				report(x.Pos(), fmt.Sprintf("%s.%s(%q)", types.ExprString(sel.X), sel.Sel.Name, lit))
			case *ast.SelectorExpr:
				// A method value (s := t.Skip; s(...), run(t.SkipNow)) skips without a
				// call the rules can see. A call's own selector was recorded above, as
				// the CallExpr is visited before its Fun.
				if isSkipSelector890(x) && !called[x] && !short && !gateHelpers890[fn] {
					report(x.Pos(), fmt.Sprintf("%s.%s as a method value", types.ExprString(x.X), x.Sel.Name))
				}
			}
			return true
		})
	}
	walk(f, "", false)
	return out, nil
}

// isSkipSelector890 reports whether the selector names Skip, Skipf or SkipNow.
func isSkipSelector890(s *ast.SelectorExpr) bool {
	switch s.Sel.Name {
	case "Skip", "Skipf", "SkipNow":
		return true
	}
	return false
}

// onlyWhenShort890 reports whether the condition is true only when
// testing.Short() is: the call itself, parenthesised, or a && with such an
// operand. A ||, a negation or any other use of testing.Short() is not: the
// body would skip on a run that is not -short.
func onlyWhenShort890(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return onlyWhenShort890(v.X)
	case *ast.BinaryExpr:
		return v.Op == token.LAND && (onlyWhenShort890(v.X) || onlyWhenShort890(v.Y))
	case *ast.CallExpr:
		if s, ok := v.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "Short" {
			id, ok := s.X.(*ast.Ident)
			return ok && id.Name == "testing" && len(v.Args) == 0
		}
	}
	return false
}

// skipMessage890 is the string literal a Skip call's message starts with: the
// first argument, with the operands of a "+" concatenation of literals joined.
func skipMessage890(c *ast.CallExpr) string {
	if len(c.Args) == 0 {
		return ""
	}
	var lit func(e ast.Expr) string
	lit = func(e ast.Expr) string {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if s, err := strconv.Unquote(v.Value); err == nil {
					return s
				}
			}
		case *ast.BinaryExpr:
			if v.Op == token.ADD {
				return lit(v.X) + lit(v.Y)
			}
		}
		return ""
	}
	return lit(c.Args[0])
}

// TestAdaptiveSkipRoutingIntegration890 runs the guard on the adaptive files of
// this package.
func TestAdaptiveSkipRoutingIntegration890(t *testing.T) {
	files, err := filepath.Glob("adaptive_*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	// helpers_test.go: startEngine skips when Listen fails, and the adaptive tests call it
	// (review r1 of #1124, m3).
	files = append(files, "helpers_test.go")
	// A glob that finds nothing, or not the files this guard exists for, passes
	// vacuously.
	have := map[string]bool{}
	for _, f := range files {
		have[f] = true
	}
	for _, must := range []string{"adaptive_standby_guard_886_linux_test.go", "adaptive_hybrid_test.go", "adaptive_standby_886_linux_test.go", "helpers_test.go"} {
		if !have[must] {
			t.Fatalf("the guard did not see %s among %d adaptive test files: it would pass without checking it", must, len(files))
		}
	}
	sort.Strings(files)
	var bad []string
	seen := map[string]int{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		v, err := rawSkipViolations890(f, src)
		if err != nil {
			t.Fatalf("%s does not parse: %v", f, err)
		}
		bad = append(bad, v...)
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, f, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s does not parse: %v", f, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if s, ok := c.Fun.(*ast.SelectorExpr); ok && isSkipSelector890(s) {
					seen[f+"|"+skipMessage890(c)]++
				}
			}
			return true
		})
	}
	t.Logf("%d scanned test files, %d raw skips not admitted, %d registered in unrouted890", len(files), len(bad), len(unrouted890))
	if len(bad) > 0 {
		t.Errorf("t.Skip calls that bypass skipOrFailUpswitch890 (adaptive_standby_guard_886_linux_test.go) and are not registered in unrouted890:\n  %s",
			strings.Join(bad, "\n  "))
	}
	// Every registered skip must still exist, as many times as registered, or
	// the table keeps a reason for nothing or admits a site unseen.
	for k, e := range unrouted890 {
		if seen[k] != e.sites {
			t.Errorf("unrouted890 registers %q at %d sites, but the adaptive files have %d t.Skip calls with it: fix the entry", k, e.sites, seen[k])
		}
	}
}

// TestAdaptiveSkipRoutingGuardSelfTest890 checks the guard on fixtures, and on
// the real gate file with a planted regression.
func TestAdaptiveSkipRoutingGuardSelfTest890(t *testing.T) {
	const head = "package integration\n\nimport \"testing\"\n\n"
	for _, c := range []struct {
		name string
		src  string
		want int // violations; -1 = the guard must report a parse error
	}{
		{"raw skip for a missing io_uring fails", head + `func TestX(t *testing.T) { t.Skip("io_uring unavailable here") }`, 1},
		{"raw Skipf fails", head + `func TestX(t *testing.T) { t.Skipf("adaptive engine not available: %v", 1) }`, 1},
		{"raw SkipNow fails", head + `func TestX(t *testing.T) { t.SkipNow() }`, 1},
		{"a raw skip in a subtest closure fails", head + `func TestX(t *testing.T) { t.Run("a", func(t *testing.T) { t.Skip("io_uring unavailable") }) }`, 1},
		{"the gate passes", head + `func TestX(t *testing.T) { skipOrFailUpswitch890(t, "io_uring unavailable") }`, 0},
		{"a skip inside the gate helper passes", head + `func skipOrFailUpswitch890(t *testing.T, m string) { t.Skip(m) }`, 0},
		{"a registered message is not exempt in an unregistered file", head + `func TestX(t *testing.T) { t.Skipf("engine failed to start: %v", 1) }`, 1},
		{"a -short gate passes", head + `func TestX(t *testing.T) { if testing.Short() { t.Skip("integration") } }`, 0},
		{"a -short gate with another clause passes", head + `func TestX(t *testing.T) { if x && testing.Short() { t.Skip("integration") } }`, 0},
		{"a raw skip after a -short gate fails", head + `func TestX(t *testing.T) { if testing.Short() { t.Skip("integration") }; t.Skip("io_uring unavailable") }`, 1},
		{"a raw skip in the else of a -short gate fails", head + `func TestX(t *testing.T) { if testing.Short() { } else { t.Skip("io_uring unavailable") } }`, 1},
		{"an || with testing.Short() still skips when not short and fails", head + `func TestX(t *testing.T) { if testing.Short() || !avail() { t.Skip("io_uring unavailable") } }`, 1},
		{"a negated testing.Short() gate fails", head + `func TestX(t *testing.T) { if !testing.Short() { t.Skip("io_uring unavailable") } }`, 1},
		{"a skip through a receiver named otherwise fails", head + `func TestX(t *testing.T) { t.Run("a", func(st *testing.T) { st.Skip("io_uring unavailable") }) }`, 1},
		{"a Skip method value fails", head + `func TestX(t *testing.T) { s := t.Skip; s("io_uring unavailable") }`, 1},
		{"a SkipNow method value passed as an argument fails", head + `func TestX(t *testing.T) { run(t.SkipNow) }`, 1},
		{"a Skip method value under a -short gate passes", head + `func TestX(t *testing.T) { if testing.Short() { s := t.Skip; s("integration") } }`, 0},
		{"a method named like the gate is not the gate", head + `func (s *S) skipOrFailUpswitch890(t *testing.T, m string) { t.Skip(m) }`, 1},
		{"a bodyless function declaration is not a violation or a panic", head + `func foo()`, 0},
		{"input that does not parse is an error, not a pass", head + `func TestX(t *testing.T) { t.Skip(`, -1},
		{"empty input is an error, not a pass", ``, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, err := rawSkipViolations890("fixture_test.go", []byte(c.src))
			if c.want == -1 {
				if err == nil {
					t.Fatalf("a source that does not parse gave no error (violations %v)", v)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(v) != c.want {
				t.Fatalf("violations %v, want %d", v, c.want)
			}
		})
	}

	// The real gate file with the regression this guard is for: the gate's own
	// skip, copied out of the helper into a test body, must be listed, and the
	// file as it is must not.
	const file = "adaptive_standby_guard_886_linux_test.go"
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := rawSkipViolations890(file, src); err != nil || len(v) != 0 {
		t.Fatalf("%s as it is: violations %v, err %v, want none", file, v, err)
	}
	const gate = "\tt.Skip(msg)\n}"
	if strings.Count(string(src), gate) != 1 {
		t.Fatalf("%s no longer ends the helper with %q: update the control", file, gate)
	}
	mutant := strings.Replace(string(src), gate, "\tt.Skip(msg)\n}\n\nfunc TestPlanted890(t *testing.T) { t.Skip(\"io_uring unavailable\") }", 1)
	v, err := rawSkipViolations890(file, []byte(mutant))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 {
		t.Fatalf("the planted raw skip was listed %d times (%v), want 1", len(v), v)
	}
}
