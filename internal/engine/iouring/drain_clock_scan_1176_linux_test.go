//go:build linux

package iouring

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The behaviour tests of drain_clock_step_1176_linux_test.go step the clock of
// the drainclock seam, which a deadline that reads the wall clock directly
// (time.Now().UnixNano()) does not go through. So the functions and the branch
// that hold the shutdown's deadlines are pinned not to read the wall clock
// themselves: every reading is drainclock.Now or drainclock.At.

// wallReads1176 counts the calls of time.Now and of the selectors that make a
// time an integer on the wall scale (UnixNano and friends) in n.
func wallReads1176(n ast.Node) (nows, ints int) {
	ast.Inspect(n, func(m ast.Node) bool {
		se, ok := m.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch se.Sel.Name {
		case "UnixNano", "Unix", "UnixMilli", "UnixMicro":
			ints++
		case "Now":
			if id, ok := se.X.(*ast.Ident); ok && id.Name == "time" {
				nows++
			}
		}
		return true
	})
	return nows, ints
}

func funcDecl1176(t *testing.T, f *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd
		}
	}
	t.Fatalf("no func %s in the file: the scan is out of date", name)
	return nil
}

func TestShutdownDeadlinesReadNoWallClock1176(t *testing.T) {
	fset := token.NewFileSet()
	worker, err := parser.ParseFile(fset, "worker.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := parser.ParseFile(fset, "shutdown_gate_884.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	fdl, err := parser.ParseFile(fset, "fd_lifetime.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		file *ast.File
		fn   string
		nows int // time.Now calls allowed: the wall stamp a CQE's handler is given, which is not a deadline
	}{
		{worker, "drainEnd", 0},
		{worker, "h2PoolSettled", 0},
		{worker, "stopProducersPaced", 0},
		{gate, "closeIdlePaced", 0},
		{fdl, "endOwedOpsAtShutdown", 0},
		// cancelAccept hands processCQE `now`, as the loop does; its deadline and its loop
		// condition are drainclock.Now.
		{worker, "cancelAccept", 1},
	} {
		nows, ints := wallReads1176(funcDecl1176(t, c.file, c.fn).Body)
		if nows != c.nows || ints != c.nows {
			t.Errorf("%s reads the wall clock: %d time.Now calls and %d Unix* conversions, want %d of each "+
				"(a shutdown deadline is drainclock.Now, celeris#1176)", c.fn, nows, ints, c.nows)
		}
	}
	// The branch of Worker.run that runs once the context is cancelled: its
	// clock is the drain's (celeris#1176); the wall readings of the loop come
	// after it.
	run := funcDecl1176(t, worker, "run")
	var branch *ast.IfStmt
	ast.Inspect(run.Body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || branch != nil {
			return branch == nil
		}
		if be, ok := ifs.Cond.(*ast.BinaryExpr); ok {
			if call, ok := be.X.(*ast.CallExpr); ok {
				if se, ok := call.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == "Err" {
					if id, ok := se.X.(*ast.Ident); ok && id.Name == "ctx" {
						branch = ifs
						return false
					}
				}
			}
		}
		return true
	})
	if branch == nil {
		t.Fatal("Worker.run has no `if ctx.Err() != ...` branch: the scan is out of date")
	}
	if nows, ints := wallReads1176(branch.Body); nows != 0 || ints != 0 {
		t.Errorf("the cancelled-context branch of Worker.run reads the wall clock (%d time.Now, %d Unix*): "+
			"its deadlines are drainclock.Now", nows, ints)
	}
}
