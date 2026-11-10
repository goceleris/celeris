//go:build linux

package epoll

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// (The scan also refuses the calls that strip a time.Time's monotonic reading:
// Round, Truncate, UTC, Local, In, and the loop's wall stamps.)
// The shutdown's deadlines on epoll are time.Time values (monotonic) or
// drainclock.Now (the gate's pacers); none is time.Now().UnixNano(), the wall
// clock as an integer (celeris#1176). The behaviour tests step the drainclock
// seam, which a direct wall reading does not go through, so the functions that
// hold them are pinned here not to make one.

// (celeris#1178) The loop's stamps are internal/mono readings (mono.Now and its
// conversions): the clock of cachedNow, lastActivity and the sweep, on the wall's
// scale, which is not the drain's. The gate hands closeIdleH1 one of them, made of
// the time.Now() it holds for its pacing (mono.FromTime), and no deadline of the
// shutdown reads one.
func unixReads1176(n ast.Node) (ints, monos int, inCloseIdle bool) {
	ast.Inspect(n, func(m ast.Node) bool {
		call, ok := m.(*ast.CallExpr)
		if !ok {
			return true
		}
		if se, ok := call.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == "closeIdleH1" {
			for _, a := range call.Args {
				if c, ok := a.(*ast.CallExpr); ok {
					if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "FromTime" {
						if id, ok := s.X.(*ast.Ident); ok && id.Name == "mono" {
							inCloseIdle = true
						}
					}
				}
			}
		}
		return true
	})
	ast.Inspect(n, func(m ast.Node) bool {
		if se, ok := m.(*ast.SelectorExpr); ok {
			switch se.Sel.Name {
			case "UnixNano", "Unix", "UnixMilli", "UnixMicro", "cachedNow", "lastSweep", "lastActivity":
				ints++
			case "Round", "Truncate", "UTC", "Local", "In":
				// These strip the monotonic reading of a time.Time.
				ints++
			case "Now", "FromTime", "FromWall", "Time":
				if id, ok := se.X.(*ast.Ident); ok && id.Name == "mono" {
					monos++
				}
			}
		}
		return true
	})
	return ints, monos, inCloseIdle
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
		if ints, monos, _ := unixReads1176(find(fn).Body); ints != 0 || monos != 0 {
			t.Errorf("%s makes %d wall-clock integers (UnixNano and friends) and %d mono readings: its deadlines are time.Time or drainclock.Now (celeris#1176)", fn, ints, monos)
		}
	}
	// poolWaitGate hands closeIdleH1 the stamp drainRead gives a conn's
	// lastActivity (mono.FromTime of the time.Now() it holds), and reads no other.
	if ints, monos, inCloseIdle := unixReads1176(find("poolWaitGate").Body); ints != 0 || monos != 1 || !inCloseIdle {
		t.Errorf("poolWaitGate has %d wall-clock integers and %d mono readings, in closeIdleH1's arguments: %v; want none and exactly the one closeIdleH1 is given", ints, monos, inCloseIdle)
	}
}
