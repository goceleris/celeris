//go:build linux

package iouring

// celeris#884 item 57: in async mode hijackConn runs on the connection's
// dispatch goroutine (h1State.HijackFn), not on the worker's, and it refuses
// there. The refusal came after `cs := w.conns[fd]`, a read of the conn table
// off the worker goroutine that connsMu does not guard (connSlotBusy, the
// RegisterConn read, is meant to be the only one): the dispatch goroutine
// raced the worker's slot writes, and a missing conn answered "connection not
// found" where an async worker's answer is always the refusal. The refusal
// now comes first.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAsyncHijackRefusesBeforeTheConnTable884: whatever the slot holds, an
// async worker answers hijackConn with the refusal, and -race sees no read of
// the table while the worker's side keeps writing slots.
func TestAsyncHijackRefusesBeforeTheConnTable884(t *testing.T) {
	defer watchdog959(t, time.Minute)()
	w := &Worker{conns: make([]*connState, 8), async: true}
	for _, fd := range []int{3, 5} {
		_, err := w.hijackConn(fd)
		if err == nil || !strings.Contains(err.Error(), "AsyncHandlers") {
			t.Errorf("hijackConn(%d) on an async worker with an empty slot: %v, want the AsyncHandlers refusal", fd, err)
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { // the worker goroutine: installs and clears slot 3
		defer wg.Done()
		cs := &connState{}
		for {
			select {
			case <-stop:
				return
			default:
			}
			w.setConnSlot(3, cs)
			w.clearConnSlot(3)
		}
	}()
	for range 20000 {
		if _, err := w.hijackConn(3); err == nil || !strings.Contains(err.Error(), "AsyncHandlers") {
			t.Fatalf("hijackConn(3) racing the slot writes: %v, want the AsyncHandlers refusal", err)
		}
	}
	close(stop)
	wg.Wait()
}

// asyncRefusalBeforeTable884 checks fn (a hijackConn): the `if w.async`
// refusal is a statement of its top-level body, and it comes before every use
// of the conn table. It returns what is wrong, or "".
func asyncRefusalBeforeTable884(fset *token.FileSet, fn *ast.FuncDecl) string {
	refusal := token.NoPos
	for _, st := range fn.Body.List {
		ifs, ok := st.(*ast.IfStmt)
		if !ok {
			continue
		}
		found := false
		ast.Inspect(ifs.Cond, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "async" {
				found = true
			}
			return true
		})
		if found {
			refusal = ifs.Pos()
			break
		}
	}
	if refusal == token.NoPos {
		return "no top-level `if <worker>.async` refusal"
	}
	bad := ""
	walkConnTable(fn, func(sel *ast.SelectorExpr, _, _ ast.Node) {
		if sel.Pos() < refusal && bad == "" {
			bad = fset.Position(sel.Pos()).String() + " reads the conn table before the async refusal"
		}
	})
	return bad
}

// TestHijackConnRefusesAsyncBeforeTheConnTable884: the structure pin, for the
// order the behaviour test above sees only as a race.
func TestHijackConnRefusesAsyncBeforeTheConnTable884(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "worker.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil && fn.Name.Name == "hijackConn" {
			checked++
			if bad := asyncRefusalBeforeTable884(fset, fn); bad != "" {
				t.Errorf("hijackConn: %s (celeris#884)", bad)
			}
		}
	}
	if checked != 1 {
		t.Fatalf("found %d hijackConn declarations, want 1", checked)
	}
}

// TestHijackOrderScanSeesTheWrongOrder884 is the scan's own control.
func TestHijackOrderScanSeesTheWrongOrder884(t *testing.T) {
	const src = `package iouring
func (w *Worker) good(fd int) { if w.async { return }; cs := w.conns[fd]; _ = cs }
func (w *Worker) readFirst(fd int) { cs := w.conns[fd]; if w.async { return }; _ = cs }
func (w *Worker) nested(fd int) { if fd > 0 { if w.async { return } }; cs := w.conns[fd]; _ = cs }
func (w *Worker) none(fd int) { cs := w.conns[fd]; _ = cs }
func (w *Worker) viaOther(fd int) { if w.h1Only { return }; _ = w.conns[fd] }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"good": false, "readFirst": true, "nested": true, "none": true, "viaOther": true}
	for _, d := range f.Decls {
		fn := d.(*ast.FuncDecl)
		got := asyncRefusalBeforeTable884(fset, fn) != ""
		if got != want[fn.Name.Name] {
			t.Errorf("%s: reported=%v, want %v", fn.Name.Name, got, want[fn.Name.Name])
		}
	}
}
