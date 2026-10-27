package eventloop

// celeris#890 (the #932 wake test, WAKEATOMIC): a producer's wake must go
// through wakefd.WakeFD.Signal, whose write(2) and the eventfd's close share a
// lock. A wake that loads the eventfd's number (w.wakeFD.FD()) and writes it
// itself races Loop.Close: it can load the number before the close and write
// it after, onto whatever socket or eventfd has taken the number meanwhile.
//
// No test can order that shape. FD() returns -1 once the close has run, so a
// hook placed after the close (testHookAfterWakeClose,
// TestAWakeAfterShutdownClosedTheEventfdWritesNothing890) never sees a stale
// number; the race detector reports nothing (the number is an atomic and the
// write is a syscall); and the only oracle that can catch it,
// TestWriteRacingCloseNeverTouchesTheClosedEventfd862, needs a wake to land in
// the few microseconds between the close and the reuse of the number (0 of 10
// processes without -race, 1 of 10 with it). The window sits between two
// statements of the mutant's own wake, where the production code has no hook
// and, by design, no such window. So the property is pinned at the source:
// this test parses the package's non-test files and requires that
//
//   - every use of the worker's wakeFD field is a method call, and the method
//     is the one its function is allowed (newWorker builds it, run reads the
//     number on the loop thread, wake signals, shutdown closes);
//   - wake's body is the single statement w.wakeFD.Signal().
//
// It has no build tag: the files are parsed, not compiled, so it runs on every
// platform the package builds on. A mutant fails it with the position of the
// offending use, for example "(*worker).wake calls wakeFD.FD".

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// wakeFDUses is the set of (function, method) pairs allowed to touch the
// worker's wakeFD field. "<field>" stands for the composite-literal key.
var wakeFDUses = map[string]bool{
	"newWorker <field>": true,
	"run FD":            true,
	"wake Signal":       true,
	"shutdown Close":    true,
}

func TestWakeGoesThroughSignalOnly890(t *testing.T) {
	uses, bad, wakeBody := scanWakeFDUses(t, ".")
	for _, v := range bad {
		t.Error(v)
	}
	for _, u := range uses {
		if !wakeFDUses[u.key()] {
			t.Errorf("%s: %s uses wakeFD.%s; only %s are allowed (a producer's wake must be wakefd.WakeFD.Signal, celeris#890 WAKEATOMIC)",
				u.pos, u.fn, u.method, allowedList())
		}
	}
	// A guard that scans nothing passes for the wrong reason: every allowed use
	// must be there, so a rename of the field or of a function fails here
	// instead of silently emptying the scan.
	seen := map[string]bool{}
	for _, u := range uses {
		seen[u.key()] = true
	}
	for k := range wakeFDUses {
		if !seen[k] {
			t.Errorf("the scan found no %q: the worker's wakeFD usage changed shape; update this guard with it", k)
		}
	}
	if wakeBody != "w.wakeFD.Signal()" {
		t.Errorf("(*worker).wake is %q, want exactly the statement w.wakeFD.Signal()", wakeBody)
	}
}

type wakeFDUse struct {
	pos    token.Position
	fn     string
	method string
}

func (u wakeFDUse) key() string { return u.fn + " " + u.method }

func allowedList() string {
	var ks []string
	for k := range wakeFDUses {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}

// scanWakeFDUses parses the non-test .go files of dir. It returns every
// wakeFD use that is a method call or a composite-literal key, the uses that
// are anything else (a bare read of the field, a copy, a method value) as
// messages, and the source of (*worker).wake's body.
func scanWakeFDUses(t *testing.T, dir string) (uses []wakeFDUse, bad []string, wakeBody string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	parsed := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			if fd.Name.Name == "wake" && fd.Recv != nil && recvName(fd) == "worker" {
				wakeBody = bodySource(fset, src, fd.Body)
			}
			handled := map[*ast.SelectorExpr]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
						if in, ok := sel.X.(*ast.SelectorExpr); ok && in.Sel.Name == "wakeFD" {
							handled[in] = true
							uses = append(uses, wakeFDUse{fset.Position(n.Pos()), fd.Name.Name, sel.Sel.Name})
						}
					}
				case *ast.KeyValueExpr:
					if id, ok := n.Key.(*ast.Ident); ok && id.Name == "wakeFD" {
						uses = append(uses, wakeFDUse{fset.Position(n.Pos()), fd.Name.Name, "<field>"})
					}
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "wakeFD" && !handled[sel] {
					bad = append(bad, fmt.Sprintf("%s: %s reads the wakeFD field without calling a method on it (a copy or a method value escapes WakeFD's lock)",
						fset.Position(sel.Pos()), fd.Name.Name))
				}
				return true
			})
		}
	}
	if parsed == 0 {
		t.Fatalf("no non-test .go file in %s", dir)
	}
	return uses, bad, wakeBody
}

func recvName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	switch x := fd.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.Ident:
		return x.Name
	}
	return ""
}

// bodySource is the statements of a function body, without braces and
// surrounding space, comments excluded by taking the statement sources.
func bodySource(fset *token.FileSet, src []byte, b *ast.BlockStmt) string {
	var parts []string
	for _, s := range b.List {
		parts = append(parts, string(src[fset.Position(s.Pos()).Offset:fset.Position(s.End()).Offset]))
	}
	return strings.Join(parts, "; ")
}
