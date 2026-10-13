//go:build linux

package iouring

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"testing"

	"github.com/goceleris/celeris/internal/conn"
)

// celeris#884 item 50, review round 1: the pool wait's gate walked every live
// connection on every pass of the loop, for as long as the HTTP/2 handlers
// ran (a serving phase, unlike the send drain: traffic goes on at its full
// rate). stopProducersPaced is that walk at the pool wait's own cadence,
// h2PoolDrainPoll: a producer whose lock was busy is tried again within it,
// and no pass pays the walk twice in a poll.

// TestStopProducersPacedRunsOncePerPoll884: the first call walks; a call
// within h2PoolDrainPoll of it does not (the conn it would end is left alone);
// one a poll later does, and ends it.
func TestStopProducersPacedRunsOncePerPoll884(t *testing.T) {
	w, cs := detachStrandRig806(t, nil)
	t0 := int64(1_000_000_000)

	cs.detachMu.Lock() // a handler running: the first walk's TryLock fails
	if !w.stopProducersPaced(t0) {
		t.Fatal("the first call did not walk")
	}
	cs.detachMu.Unlock()
	if cs.detachClosed {
		t.Fatal("setup: the walk ended a conn whose lock was held")
	}

	if w.stopProducersPaced(t0 + int64(h2PoolDrainPoll)/2) {
		t.Error("a call half a poll after a walk walked again")
	}
	if cs.detachClosed {
		t.Error("a paced-out call ended the conn's writes")
	}

	if !w.stopProducersPaced(t0 + int64(h2PoolDrainPoll)) {
		t.Fatal("a call a full poll after a walk did not walk: a conn whose lock was busy would never be tried again")
	}
	if !cs.detachClosed {
		t.Error("the walk a poll later did not end the conn's writes")
	}
}

// BenchmarkStopDetachedProducers884 is one pass of the walk over n live
// connections that are all detached with their writes already ended, which is
// what every pass after the first costs while the HTTP/2 handlers run: a
// TryLock and an unlock per conn. Multiply by the passes the pool wait makes
// per second (unpaced: one per loop pass) or 1000/h2PoolDrainPoll (paced).
func BenchmarkStopDetachedProducers884(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("conns=%d", n), func(b *testing.B) {
			w := &Worker{listenFD: -1, conns: make([]*connState, n+16)}
			for fd := 8; fd < n+8; fd++ {
				cs := &connState{fd: fd, liveIdx: -1, detachMu: &sync.Mutex{}, h1State: &conn.H1State{}, detachClosed: true}
				cs.h1State.Detached.Store(true)
				w.conns[fd] = cs
				w.addLiveConn(cs)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				w.stopDetachedProducers()
			}
		})
	}
}

// poolGateWalks884 returns, for the if statement of fn whose condition reads
// h2DrainStart (the pool wait's gate), the names of the walk functions its
// body calls.
func poolGateWalks884(fset *token.FileSet, f *ast.File, fn string) (walks []string, found bool) {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil || fd.Name.Name != fn {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			ifs, ok := n.(*ast.IfStmt)
			if !ok {
				return true
			}
			reads := false
			ast.Inspect(ifs.Cond, func(m ast.Node) bool {
				if se, ok := m.(*ast.SelectorExpr); ok && se.Sel.Name == "h2DrainStart" {
					reads = true
				}
				return true
			})
			if !reads {
				return true
			}
			found = true
			ast.Inspect(ifs.Body, func(m ast.Node) bool {
				if call, ok := m.(*ast.CallExpr); ok {
					if se, ok := call.Fun.(*ast.SelectorExpr); ok && (se.Sel.Name == "stopDetachedProducers" || se.Sel.Name == "stopProducersPaced") {
						walks = append(walks, se.Sel.Name)
					}
				}
				return true
			})
			return false
		})
	}
	return walks, found
}

// TestPoolWaitGateWalksPaced884: the behaviour test cannot tell the paced
// walk from the raw one (both end the producers), so the branch of Worker.run
// that gates the pool wait is pinned to call stopProducersPaced, once, and not
// stopDetachedProducers.
func TestPoolWaitGateWalksPaced884(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "worker.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	walks, found := poolGateWalks884(fset, f, "run")
	if !found {
		t.Fatal("Worker.run has no if reading h2DrainStart: the scan or the loop changed")
	}
	if len(walks) != 1 || walks[0] != "stopProducersPaced" {
		t.Errorf("the pool wait's gate walks the detached conns with %v, want exactly [stopProducersPaced]", walks)
	}
}

// TestPoolGateScanSeesARawWalk884 is the scan's own control.
func TestPoolGateScanSeesARawWalk884(t *testing.T) {
	const src = `package iouring
func (w *Worker) run() {
	if now > w.h2DrainStart+1 {
		w.refuseRecv = true
		w.stopDetachedProducers()
	}
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	walks, found := poolGateWalks884(fset, f, "run")
	if !found || len(walks) != 1 || walks[0] != "stopDetachedProducers" {
		t.Errorf("the scan saw found=%v walks=%v, want the raw walk", found, walks)
	}
}
