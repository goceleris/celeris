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

// workerNames returns the names a function can reach a *Worker by: its
// receiver and its parameters of type Worker or *Worker, whatever they are
// called (a method with another receiver name than w is still a Worker's). The
// guard's remaining limit: a Worker copied into a differently named local
// (x := w) is not followed, nor is a table reached through an interface; no
// non-test code in the package does either (the only other field called conns
// is closedEntry's, a different table).
func workerNames(fn *ast.FuncDecl) map[string]bool {
	names := map[string]bool{}
	isWorker := func(e ast.Expr) bool {
		if st, ok := e.(*ast.StarExpr); ok {
			e = st.X
		}
		id, ok := e.(*ast.Ident)
		return ok && id.Name == "Worker"
	}
	add := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			if !isWorker(f.Type) {
				continue
			}
			for _, n := range f.Names {
				names[n.Name] = true
			}
		}
	}
	add(fn.Recv)
	add(fn.Type.Params)
	return names
}

// walkConnTable calls visit for every use of a Worker's conn table in fn, with
// the use's selector, its parent and its grandparent. The table is found by the
// field's name on any name workerNames gives for the function (the identifier
// is not assumed to be w), so a store through another receiver name is seen.
func walkConnTable(fn *ast.FuncDecl, visit func(sel *ast.SelectorExpr, parent, grand ast.Node)) {
	names := workerNames(fn)
	if len(names) == 0 {
		return
	}
	var stack []ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "conns" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || !names[id.Name] {
			return true
		}
		var parent, grand ast.Node
		if len(stack) >= 2 {
			parent = stack[len(stack)-2]
		}
		if len(stack) >= 3 {
			grand = stack[len(stack)-3]
		}
		visit(sel, parent, grand)
		return true
	})
}

// bareConnTableWrites returns the position and enclosing function of every use
// of a Worker's conn table outside the helpers that can write it or let
// another name write it: a store (w.conns[i] = x, w.conns = x, an increment or
// an address-of on an element), clear / copy / append on it, and any use that
// hands the table or an element's address on (an alias: c := w.conns, a slice
// expression, &w.conns[i], an argument, a return). What is allowed is a read:
// w.conns[i] as a value, len / cap of the table, ranging over it. The table is
// found through the function's own Worker receiver or parameter, whatever it is
// called (receiver names other than a Worker's, such as the closed-identity
// entry's own conns, are another table).
func bareConnTableWrites(fset *token.FileSet, f *ast.File) []string {
	var out []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if fn.Name.Name == "setConnSlot" || fn.Name.Name == "clearConnSlot" {
			continue
		}
		flag := func(pos token.Pos, what string) {
			out = append(out, fset.Position(pos).String()+" in "+fn.Name.Name+" ("+what+")")
		}
		walkConnTable(fn, func(sel *ast.SelectorExpr, parent, grand ast.Node) {
			switch p := parent.(type) {
			case *ast.CallExpr:
				// len(w.conns) and cap(w.conns) read the fixed length; any
				// other call (clear, copy, append, a helper) can write.
				if id, ok := p.Fun.(*ast.Ident); ok && (id.Name == "len" || id.Name == "cap") {
					return
				}
				flag(sel.Pos(), "passed to a call")
			case *ast.RangeStmt:
				if p.X == ast.Expr(sel) {
					return
				}
				flag(sel.Pos(), "written by a range")
			case *ast.IndexExpr:
				if p.X != ast.Expr(sel) {
					flag(sel.Pos(), "used as an index")
					return
				}
				switch g := grand.(type) {
				case *ast.AssignStmt:
					for _, l := range g.Lhs {
						if l == ast.Expr(p) {
							flag(sel.Pos(), "slot store")
						}
					}
				case *ast.IncDecStmt:
					flag(sel.Pos(), "slot store")
				case *ast.UnaryExpr:
					if g.Op == token.AND {
						flag(sel.Pos(), "address of a slot")
					}
				case *ast.RangeStmt:
					if g.Key == ast.Expr(p) || g.Value == ast.Expr(p) {
						flag(sel.Pos(), "slot store")
					}
				}
			case *ast.AssignStmt:
				for _, l := range p.Lhs {
					if l == ast.Expr(sel) {
						flag(sel.Pos(), "table replaced")
						return
					}
				}
				flag(sel.Pos(), "table aliased")
			case *ast.BinaryExpr:
				// a comparison, w.conns == nil: a read
			default:
				flag(sel.Pos(), "table escapes: slice, address, return or alias")
			}
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
		for _, r := range registerConnTableReads(fset, f) {
			t.Errorf("%s: RegisterConn reads the conn table itself; it must ask connSlotBusy (celeris#959)", r)
		}
		for _, w := range bareConnTableWrites(fset, f) {
			t.Errorf("%s writes a conn-table slot outside setConnSlot/clearConnSlot: RegisterConn reads it from another goroutine under connsMu (celeris#959)", w)
		}
	}
	if scanned < 20 {
		t.Fatalf("apparatus: scanned only %d source files", scanned)
	}
}

// registerConnTableReads returns the position of every use of a Worker's conn
// table inside RegisterConn, the one reader on another goroutine: it must ask
// connSlotBusy and never look at w.conns itself. The race test cannot stand in
// for this for the check under driverMu, whose read follows a locked one that
// already orders the worker's earlier writes before it.
func registerConnTableReads(fset *token.FileSet, f *ast.File) []string {
	var out []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Name.Name != "RegisterConn" {
			continue
		}
		walkConnTable(fn, func(sel *ast.SelectorExpr, _, _ ast.Node) {
			out = append(out, fset.Position(sel.Pos()).String())
		})
	}
	return out
}

// TestConnSlotGuardSeesABareWrite959 is the guard's own control: the scan
// reports each kind of bare store in a snippet (through the receiver whatever
// it is called, and through an alias or an address), and nothing in the
// helpers or in the reads the worker does without a lock.
func TestConnSlotGuardSeesABareWrite959(t *testing.T) {
	const src = `package iouring
func (w *Worker) a(fd int, cs *connState)  { w.conns[fd] = cs }
func (w *Worker) b(fd int)                 { w.conns[fd] = nil }
func (w *Worker) c()                       { clear(w.conns) }
func (w *Worker) d()                       { w.conns = nil }
func (wk *Worker) g(fd int)                { wk.conns[fd] = nil }
func (w *Worker) h(fd int)                 { c := w.conns; c[fd] = nil }
func (w *Worker) i(fd int)                 { p := &w.conns[fd]; *p = nil }
func (w *Worker) j(fd int)                 { s := w.conns[fd:]; s[0] = nil }
func k(wrk *Worker, fd int)                { wrk.conns[fd] = nil }
func (w *Worker) l()                       { for i := range w.conns { w.conns[i] = nil } }
func (w *Worker) m() []*connState          { return w.conns }
func (w *Worker) n(fd int)                 { copy(w.conns, nil) }
func (w *Worker) o(fd int)                 { w.conns = append(w.conns, nil) }
func (w *Worker) e(fd int)                 { x := w.conns[fd]; _ = x }
func (w *Worker) e2(fd int) bool           { return fd < len(w.conns) && w.conns[fd] != nil && w.conns != nil }
func (w *Worker) e3() int                  { n := 0; for _, cs := range w.conns { if cs != nil { n++ } }; for range len(w.conns) { n++ }; return n }
func (w *Worker) e4(fd int)                { if cs := w.conns[fd]; cs != nil { cs.fd = 0 } }
func (ce *closedEntry) f()                 { ce.conns[0] = nil }
func other(fd int, conns []*connState)     { conns[fd] = nil }
func (w *Worker) RegisterConn(fd int) error         { if fd < len(w.conns) && w.conns[fd] != nil { return nil }; return nil }
func (w *Worker) setConnSlot(fd int, cs *connState) { w.conns[fd] = cs }
func (w *Worker) clearConnSlot(fd int)              { w.conns[fd] = nil }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := bareConnTableWrites(fset, f)
	if reads := registerConnTableReads(fset, f); len(reads) != 2 {
		t.Errorf("the scan found %d table reads in the snippet's RegisterConn, want 2: %v", len(reads), reads)
	}
	// Each finding names its function after " in "; l is found twice
	// (the range key store is the index store).
	seen := map[string]int{}
	for _, g := range got {
		_, rest, _ := strings.Cut(g, " in ")
		fn, _, _ := strings.Cut(rest, " ")
		seen[fn]++
	}
	want := []string{"a", "b", "c", "d", "g", "h", "i", "j", "k", "l", "m", "n", "o"}
	for _, fn := range want {
		if seen[fn] == 0 {
			t.Errorf("the scan missed the bare store in %s: %v", fn, got)
		}
		delete(seen, fn)
	}
	for fn, n := range seen {
		t.Errorf("the scan reported %d finding(s) in %s, which only reads the table or is no Worker's: %v", n, fn, got)
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
