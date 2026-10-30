//go:build linux

package epoll

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The shutdown's deadlines on epoll are time.Time values (monotonic) or
// drainclock.Now (the gate's pacers); none is time.Now().UnixNano(), the wall
// clock as an integer (celeris#1176). The behaviour tests step the drainclock
// seam, which a direct wall reading does not go through, so the functions that
// hold them are pinned here not to make one.

func unixReads1176(n ast.Node) (ints int, inCloseIdle bool) {
	ast.Inspect(n, func(m ast.Node) bool {
		call, ok := m.(*ast.CallExpr)
		if !ok {
			return true
		}
		if se, ok := call.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == "closeIdleH1" {
			for _, a := range call.Args {
				if c, ok := a.(*ast.CallExpr); ok {
					if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "UnixNano" {
						inCloseIdle = true
					}
				}
			}
		}
		return true
	})
	ast.Inspect(n, func(m ast.Node) bool {
		if se, ok := m.(*ast.SelectorExpr); ok {
			switch se.Sel.Name {
			case "UnixNano", "Unix", "UnixMilli", "UnixMicro":
				ints++
			}
		}
		return true
	})
	return ints, inCloseIdle
}

func TestShutdownDeadlinesReadNoWallClock1176(t *testing.T) {
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, name := range []string{"loop.go", "poolwait_gate_885.go"} {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = f
	}
	find := func(fn string) *ast.FuncDecl {
		for _, f := range files {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn {
					return fd
				}
			}
		}
		t.Fatalf("no func %s: the scan is out of date", fn)
		return nil
	}
	for _, fn := range []string{"sendDrainWait", "drainSends", "h2PoolSettled", "poolWaitSettled", "h1InProgress"} {
		if ints, _ := unixReads1176(find(fn).Body); ints != 0 {
			t.Errorf("%s makes %d wall-clock integers (UnixNano and friends): its deadlines are time.Time or drainclock.Now (celeris#1176)", fn, ints)
		}
	}
	// poolWaitGate hands closeIdleH1 the wall stamp drainRead gives a conn's
	// lastActivity, and reads no other.
	if ints, inCloseIdle := unixReads1176(find("poolWaitGate").Body); ints != 1 || !inCloseIdle {
		t.Errorf("poolWaitGate has %d wall-clock integers, in closeIdleH1's arguments: %v; want exactly the one closeIdleH1 is given", ints, inCloseIdle)
	}
}
