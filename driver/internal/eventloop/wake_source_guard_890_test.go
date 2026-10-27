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
//   - wake's body is the single statement w.wakeFD.Signal();
//   - the eventfd number newWorker creates reaches only wakefd.New,
//     unix.EpollCtl and unix.Close: it is never stored in another field, where
//     a producer could write it itself (review round 1: a copy kept in the
//     worker and written by enqueueFlush has WAKEATOMIC's defect and left
//     wakeFD untouched);
//   - the only raw write(2) of the package is flushLocked's, to a conn's fd.
//
// Known limit: this is a syntactic pin of the shapes above, not a proof. A
// number reaching a producer by a route outside them (a global, a closure over
// an integer returned by a helper) is not seen; the probabilistic oracle of
// TestWriteRacingCloseNeverTouchesTheClosedEventfd862 is the backstop.
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
	for _, v := range scanEventfdNumber(t, ".") {
		t.Error(v)
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

// scanEventfdNumber checks the two rules about the raw eventfd number: in
// newWorker the identifier efd appears only as an argument of wakefd.New,
// unix.EpollCtl or unix.Close; and a raw write call (unix.Write, syscall.Write
// and their variants) appears only in flushLocked, on c.fd.
func scanEventfdNumber(t *testing.T, dir string) (bad []string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sawNewWorker, sawFlush := false, false
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
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			if fd.Recv == nil && fd.Name.Name == "newWorker" {
				sawNewWorker = true
				bad = append(bad, efdUses(fset, fd)...)
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || (pkg.Name != "unix" && pkg.Name != "syscall") || !strings.HasPrefix(sel.Sel.Name, "Write") && !strings.HasPrefix(sel.Sel.Name, "Pwrite") {
					return true
				}
				first := ""
				if len(call.Args) > 0 {
					first = string(src[fset.Position(call.Args[0].Pos()).Offset:fset.Position(call.Args[0].End()).Offset])
				}
				if fd.Name.Name == "flushLocked" && first == "c.fd" {
					sawFlush = true
					return true
				}
				bad = append(bad, fmt.Sprintf("%s: %s calls %s.%s(%s, ...); the only raw write allowed is flushLocked's, on c.fd (a producer's wake must be wakefd.WakeFD.Signal, celeris#890 WAKEATOMIC)",
					fset.Position(call.Pos()), fd.Name.Name, pkg.Name, sel.Sel.Name, first))
				return true
			})
		}
	}
	if !sawNewWorker || !sawFlush {
		bad = append(bad, fmt.Sprintf("the scan did not find newWorker (%v) or flushLocked's write (%v): the code changed shape; update this guard with it", sawNewWorker, sawFlush))
	}
	return bad
}

// efdUses reports every use of the identifier efd in fd that is not an
// argument of wakefd.New, unix.EpollCtl or unix.Close (anywhere beneath the
// EpollCtl call, for int32(efd) in the event).
func efdUses(fset *token.FileSet, fd *ast.FuncDecl) (bad []string) {
	allowed := map[*ast.Ident]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch pkg.Name + "." + sel.Sel.Name {
		case "wakefd.New", "unix.Close":
			for _, a := range call.Args {
				if id, ok := a.(*ast.Ident); ok && id.Name == "efd" {
					allowed[id] = true
				}
			}
		case "unix.EpollCtl":
			ast.Inspect(call, func(m ast.Node) bool {
				if id, ok := m.(*ast.Ident); ok && id.Name == "efd" {
					allowed[id] = true
				}
				return true
			})
		}
		return true
	})
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		// The definition (efd, err := unix.Eventfd(...)) is an Ident on the
		// left of a define; skip it.
		if as, ok := n.(*ast.AssignStmt); ok && as.Tok == token.DEFINE {
			for _, l := range as.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == "efd" {
					allowed[id] = true
				}
			}
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == "efd" && !allowed[id] {
			bad = append(bad, fmt.Sprintf("%s: newWorker uses the eventfd number efd outside wakefd.New, unix.EpollCtl and unix.Close (a copy of the number in another place can be written by a producer, celeris#890 WAKEATOMIC)", fset.Position(id.Pos())))
		}
		return true
	})
	return bad
}
