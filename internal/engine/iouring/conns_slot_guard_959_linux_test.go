//go:build linux

package iouring

// celeris#959: what the -race test (TestRegisterConnRacesAcceptAndClose959)
// cannot reach, the structure test pins. The race test drives the accept
// install and finishClose; finishCloseDetached, hijackConn, handleClose and
// the two transplant paths are not reachable by an HTTP client on a plain
// engine, so one of them left with a bare store would pass it. Every write of
// a conn-table slot must go through setConnSlot or clearConnSlot, which hold
// connsMu; this test reads the package's source for the ones that do not.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// bareConnTableWrites returns the position and enclosing function of every
// statement of src that stores into, or replaces, a Worker's conn table
// outside the two helpers: w.conns[i] = x, w.conns = x, clear(w.conns),
// copy(w.conns, ...) (receiver names other than w, such as the closed-identity
// entry's own conns, are another table).
func bareConnTableWrites(fset *token.FileSet, f *ast.File) []string {
	isTable := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "conns" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == "w"
	}
	var out []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if fn.Name.Name == "setConnSlot" || fn.Name.Name == "clearConnSlot" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					if ix, ok := l.(*ast.IndexExpr); (ok && isTable(ix.X)) || isTable(l) {
						out = append(out, fset.Position(x.Pos()).String()+" in "+fn.Name.Name)
					}
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && (id.Name == "clear" || id.Name == "copy" || id.Name == "append") && len(x.Args) > 0 && isTable(x.Args[0]) {
					out = append(out, fset.Position(x.Pos()).String()+" in "+fn.Name.Name+" ("+id.Name+")")
				}
			}
			return true
		})
	}
	return out
}

// TestConnSlotWritesGoThroughTheHelpers959: no non-test file stores into a
// Worker's conn table except setConnSlot and clearConnSlot.
func TestConnSlotWritesGoThroughTheHelpers959(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("apparatus: no source files (%v)", err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, w := range bareConnTableWrites(fset, f) {
			t.Errorf("%s writes a conn-table slot outside setConnSlot/clearConnSlot: RegisterConn reads it from another goroutine under connsMu (celeris#959)", w)
		}
	}
	if scanned < 20 {
		t.Fatalf("apparatus: scanned only %d source files", scanned)
	}
}

// TestConnSlotGuardSeesABareWrite959 is the guard's own control: the scan
// reports each kind of bare store in a snippet, and nothing in the helpers.
func TestConnSlotGuardSeesABareWrite959(t *testing.T) {
	const src = `package iouring
func (w *Worker) a(fd int, cs *connState) { w.conns[fd] = cs }
func (w *Worker) b(fd int)                { w.conns[fd] = nil }
func (w *Worker) c()                      { clear(w.conns) }
func (w *Worker) d()                      { w.conns = nil }
func (w *Worker) e(fd int)                { x := w.conns[fd]; _ = x }
func (ce *closedEntry) f()                { ce.conns[0] = nil }
func (w *Worker) setConnSlot(fd int, cs *connState) { w.conns[fd] = cs }
func (w *Worker) clearConnSlot(fd int)              { w.conns[fd] = nil }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := bareConnTableWrites(fset, f)
	if len(got) != 4 {
		t.Fatalf("the scan found %d bare stores in the snippet, want 4 (a, b, c, d): %v", len(got), got)
	}
	for i, fn := range []string{" in a", " in b", " in c", " in d"} {
		if !strings.Contains(got[i], fn) {
			t.Errorf("finding %d = %q, want it in %q", i, got[i], strings.TrimPrefix(fn, " in "))
		}
	}
}

// TestConnSlotHelpersTakeConnsMu959 pins the lock itself without -race: with
// connsMu held, neither a slot write nor RegisterConn's read can finish, and
// both finish once it is released.
func TestConnSlotHelpersTakeConnsMu959(t *testing.T) {
	defer watchdog959(t, time.Minute)()
	w := &Worker{conns: make([]*connState, 8)}
	cs := &connState{}

	w.connsMu.Lock()
	var done atomic.Int32
	ops := []func(){
		func() { w.setConnSlot(3, cs) },
		func() { w.clearConnSlot(3) },
		func() { _ = w.connSlotBusy(3) },
	}
	for _, op := range ops {
		go func() { op(); done.Add(1) }()
	}
	time.Sleep(200 * time.Millisecond)
	if n := done.Load(); n != 0 {
		t.Errorf("%d of 3 slot operations finished while connsMu was held", n)
	}
	w.connsMu.Unlock()
	for deadline := time.Now().Add(5 * time.Second); done.Load() != 3; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d of 3 slot operations finished after connsMu was released", done.Load())
		}
	}
	// The three ran in some order; the table says which was last, and the
	// reads are consistent with it.
	if got := w.connSlotBusy(3); got != (w.conns[3] != nil) {
		t.Errorf("connSlotBusy(3) = %v, conns[3] = %v", got, w.conns[3])
	}
	if w.connSlotBusy(-1) || w.connSlotBusy(len(w.conns)) {
		t.Error("connSlotBusy answered true for a number outside the table")
	}
}
