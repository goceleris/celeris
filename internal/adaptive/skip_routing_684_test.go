package adaptive

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

// celeris#684: a test of this package that cannot run where io_uring is unavailable must skip through
// skipOrFailUpswitch662, which fails instead under CELERIS_REQUIRE_UPSWITCH=1 and names the test and the
// cause. The adaptive CI job forbids any SKIP line anyway, but a raw t.Skip turns the job red at the tally
// without saying which test skipped or why; a skip in a run that does not forbid it goes unseen.
//
// This guard parses every *_test.go file of the package and lists each t.Skip, t.Skipf and t.SkipNow call that
// is none of:
//
//   - inside a gate helper (gateHelpers684);
//   - under an `if` whose condition is true only when testing.Short() is (testing.Short() itself or a && with
//     it; the -short gates, which CI never sets): a ||, a negation or an else branch is not exempt;
//   - registered in rawSkips684 with the reason the skip has nothing to do with io_uring.
//
// A new raw skip fails the guard until it goes through the gate or is registered with a reason.

// gateHelpers684 are the functions whose job is to skip or fail; a raw skip inside one is the gate itself.
var gateHelpers684 = map[string]bool{
	"skipOrFailUpswitch662": true, // the io_uring / up-switch gate (CELERIS_REQUIRE_UPSWITCH=1)
	"requireDualStack616":   true, // the dual-stack gate (CELERIS_REQUIRE_DUALSTACK=1)
	"requireDualStack925":   true,
}

// rawSkips684 are the raw skips that stay, by file and the string literal that starts the skip's message,
// each with why it is not an io_uring-availability skip.
var rawSkips684 = map[string]string{
	"prebound_listener_test.go|unix listener unavailable: %v": "t.TempDir() cannot hold a Unix socket path (too long for sun_path)",
	"ramp_transplant_test.go|CELERIS_RAMP_MAX caps the peak at %d conns, but %d epoll workers need %d to reach %.0f conns/worker " +
		"(%.2fx the controller's %.0f upThreshold): the ramp could not drive a switch": "an explicit CELERIS_RAMP_MAX override, not the host",
	"ramp_transplant_test.go|derived bands collapse on this box (low=%d high=%d, epoll_workers=%d io_uring_workers=%d)": "a host with too few CPUs for the ramp's bands; the step above it already failed on a missing io_uring tier",
	"revert_listener_gap_683_linux_test.go|net.Listen: %v":                                                              "a loopback listen failing, not io_uring",
	"port_hold_616_linux_test.go|net.Listen(%q) fails on this host: %v":                                                 "a loopback listen of the test's own address failing, not io_uring",
	"revert_listener_gap_683_linux_test.go|reuseport listen: %v":                                                        "a loopback SO_REUSEPORT listen failing, not io_uring",
}

// rawSkipViolations684 lists the t.Skip* calls of one Go source that the rules above do not admit, as
// "file:line: call(message)". It returns an error for a source that does not parse: the guard must fail on
// input it cannot read, not report no violation.
func rawSkipViolations684(name string, src []byte) ([]string, error) {
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
					if x.Body != nil { // a bodyless declaration (assembly, linkname) holds no call
						walk(x.Body, x.Name.Name, short)
					}
					return false
				}
			case *ast.IfStmt:
				if x != n && onlyWhenShort684(x.Cond) {
					walk(x.Body, fn, true)
					if x.Else != nil {
						walk(x.Else, fn, short)
					}
					return false
				}
			case *ast.CallExpr:
				// Any receiver: a subtest closure may name its *testing.T anything.
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok || !isSkipSelector684(sel) {
					return true
				}
				called[sel] = true
				if short || gateHelpers684[fn] {
					return true
				}
				lit := skipMessage684(x)
				if _, ok := rawSkips684[filepath.Base(name)+"|"+lit]; ok {
					return true
				}
				report(x.Pos(), fmt.Sprintf("%s.%s(%q)", types.ExprString(sel.X), sel.Sel.Name, lit))
			case *ast.SelectorExpr:
				// A method value (s := t.Skip; s(...), run(t.SkipNow)) skips without a call the rules can see.
				// A call's own selector was recorded above, as the CallExpr is visited before its Fun.
				if isSkipSelector684(x) && !called[x] && !short && !gateHelpers684[fn] {
					report(x.Pos(), fmt.Sprintf("%s.%s as a method value", types.ExprString(x.X), x.Sel.Name))
				}
			}
			return true
		})
	}
	walk(f, "", false)
	return out, nil
}

// isSkipSelector684 reports whether the selector names Skip, Skipf or SkipNow.
func isSkipSelector684(s *ast.SelectorExpr) bool {
	switch s.Sel.Name {
	case "Skip", "Skipf", "SkipNow":
		return true
	}
	return false
}

// onlyWhenShort684 reports whether the condition is true only when testing.Short() is: the call itself,
// parenthesised, or a && with such an operand. A ||, a negation or any other use of testing.Short() is
// not: the body would skip on a run that is not -short.
func onlyWhenShort684(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return onlyWhenShort684(v.X)
	case *ast.BinaryExpr:
		return v.Op == token.LAND && (onlyWhenShort684(v.X) || onlyWhenShort684(v.Y))
	case *ast.CallExpr:
		if s, ok := v.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "Short" {
			id, ok := s.X.(*ast.Ident)
			return ok && id.Name == "testing" && len(v.Args) == 0
		}
	}
	return false
}

// skipMessage684 is the string literal a Skip call's message starts with: the first argument, with the
// operands of a "+" concatenation of literals joined.
func skipMessage684(c *ast.CallExpr) string {
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

// TestAdaptiveSkipRouting684 runs the guard on this package's test files.
func TestAdaptiveSkipRouting684(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	// A glob that finds nothing, or not the files this guard exists for, passes vacuously.
	have := map[string]bool{}
	for _, f := range files {
		have[f] = true
	}
	for _, must := range []string{"port_hold_616_linux_test.go", "driver_provider_test.go", "skip_routing_684_test.go"} {
		if !have[must] {
			t.Fatalf("the guard did not see %s among %d test files: it would pass without checking it", must, len(files))
		}
	}
	sort.Strings(files)
	var bad []string
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		v, err := rawSkipViolations684(f, src)
		if err != nil {
			t.Fatalf("%s does not parse: %v", f, err)
		}
		bad = append(bad, v...)
	}
	if len(bad) > 0 {
		t.Errorf("t.Skip calls that bypass skipOrFailUpswitch662 (upswitch_gate_linux_test.go) and are not registered in rawSkips684:\n  %s",
			strings.Join(bad, "\n  "))
	}
	// Every registered raw skip must still exist, or the table keeps a reason for nothing.
	seen := map[string]bool{}
	for _, f := range files {
		src, _ := os.ReadFile(f)
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, f, src, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if s, ok := c.Fun.(*ast.SelectorExpr); ok && (s.Sel.Name == "Skip" || s.Sel.Name == "Skipf") {
					seen[f+"|"+skipMessage684(c)] = true
				}
			}
			return true
		})
	}
	for k := range rawSkips684 {
		if !seen[k] {
			t.Errorf("rawSkips684 registers %q, which no t.Skip in the package has any more: delete the entry", k)
		}
	}
}

// TestAdaptiveSkipRoutingGuardSelfTest684 checks the guard on fixtures, and on the real tree with a
// planted regression.
func TestAdaptiveSkipRoutingGuardSelfTest684(t *testing.T) {
	const head = "package adaptive\n\nimport \"testing\"\n\n"
	for _, c := range []struct {
		name string
		src  string
		want int // violations; -1 = the guard must report a parse error
	}{
		{"raw skip for a missing io_uring fails", head + `func TestX(t *testing.T) { t.Skip("io_uring unavailable here") }`, 1},
		{"raw Skipf fails", head + `func TestX(t *testing.T) { t.Skipf("start engine %v", 1) }`, 1},
		{"raw SkipNow fails", head + `func TestX(t *testing.T) { t.SkipNow() }`, 1},
		{"a raw skip in a subtest closure fails", head + `func TestX(t *testing.T) { t.Run("a", func(t *testing.T) { t.Skip("io_uring unavailable") }) }`, 1},
		{"the gate passes", head + `func TestX(t *testing.T) { skipOrFailUpswitch662(t, "io_uring unavailable") }`, 0},
		{"a skip inside the gate helper passes", head + `func skipOrFailUpswitch662(t *testing.T, f string) { t.Skip(f) }`, 0},
		{"a -short gate passes", head + `func TestX(t *testing.T) { if testing.Short() { t.Skip("integration") } }`, 0},
		{"a -short gate with another clause passes", head + `func TestX(t *testing.T) { if x && testing.Short() { t.Skip("integration") } }`, 0},
		{"a raw skip after a -short gate fails", head + `func TestX(t *testing.T) { if testing.Short() { t.Skip("integration") }; t.Skip("io_uring unavailable") }`, 1},
		{"a raw skip in the else of a -short gate fails", head + `func TestX(t *testing.T) { if testing.Short() { } else { t.Skip("io_uring unavailable") } }`, 1},
		{"an || with testing.Short() still skips when not short and fails", head + `func TestX(t *testing.T) { if testing.Short() || !avail() { t.Skip("io_uring unavailable") } }`, 1},
		{"a negated testing.Short() gate fails", head + `func TestX(t *testing.T) { if !testing.Short() { t.Skip("io_uring unavailable") } }`, 1},
		{"a -short condition that is a call of another function fails", head + `func TestX(t *testing.T) { if short() { t.Skip("io_uring unavailable") } }`, 1},
		{"a skip through a receiver named otherwise fails", head + `func TestX(t *testing.T) { t.Run("a", func(st *testing.T) { st.Skip("io_uring unavailable") }) }`, 1},
		{"a skip through a field receiver fails", head + `func TestX(t *testing.T) { h.t.Skipf("io_uring unavailable %v", 1) }`, 1},
		{"a Skip method value fails", head + `func TestX(t *testing.T) { s := t.Skip; s("io_uring unavailable") }`, 1},
		{"a SkipNow method value passed as an argument fails", head + `func TestX(t *testing.T) { run(t.SkipNow) }`, 1},
		{"a Skip method value under a -short gate passes", head + `func TestX(t *testing.T) { if testing.Short() { s := t.Skip; s("integration") } }`, 0},
		{"a parenthesised testing.Short() gate passes", head + `func TestX(t *testing.T) { if (testing.Short()) { t.Skip("integration") } }`, 0},
		{"a bodyless function declaration is not a violation or a panic", head + `func foo()`, 0},
		{"a registered message is not exempt in an unregistered file", head + `func TestX(t *testing.T) { t.Skipf("net.Listen: %v", 1) }`, 1},
		{"input that does not parse is an error, not a pass", head + `func TestX(t *testing.T) { t.Skip(`, -1},
		{"empty input is an error, not a pass", ``, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, err := rawSkipViolations684("fixture_test.go", []byte(c.src))
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
	// A registered entry is exempt only in its own file.
	v, err := rawSkipViolations684("revert_listener_gap_683_linux_test.go", []byte(head+`func TestX(t *testing.T) { t.Skipf("net.Listen: %v", 1) }`))
	if err != nil || len(v) != 0 {
		t.Errorf("a registered raw skip in its own file: violations %v, err %v, want none", v, err)
	}

	// The real tree with the regression this guard is for: the gate in port_hold_616 put back to a raw skip
	// must be listed (once per site), and the tree as it is must not.
	src, err := os.ReadFile("port_hold_616_linux_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := rawSkipViolations684("port_hold_616_linux_test.go", src); err != nil || len(v) != 0 {
		t.Fatalf("port_hold_616_linux_test.go as it is: violations %v, err %v, want none", v, err)
	}
	const gate = `skipOrFailUpswitch662(t, "io_uring unavailable here: cannot start adaptive on it")`
	n := strings.Count(string(src), gate)
	if n == 0 {
		t.Fatal("port_hold_616_linux_test.go no longer holds the gate call this control reverts: update the control")
	}
	mutant := strings.ReplaceAll(string(src), gate, `t.Skip("io_uring unavailable here: cannot start adaptive on it")`)
	v, err = rawSkipViolations684("port_hold_616_linux_test.go", []byte(mutant))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != n {
		t.Fatalf("the reverted gate was listed %d times (%v), want %d", len(v), v, n)
	}
}
