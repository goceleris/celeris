//go:build linux

package iouring

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#884 item 49: Worker.run's ring waits treated every error as the end
// of the worker (w.shutdown(); return). On kernels 5.19 to 6.1 the wait of
// io_uring_enter answers EBUSY when completions overflowed behind a full CQ
// ring cannot be flushed (io_cqring_wait: "if we can't even flush overflow,
// don't wait for more"), so a worker whose CQ ring overflowed between two loop
// passes stopped serving and closed every connection it owned. Neither this
// package's CI kernel nor the laptop's 7.0 Docker VM answers it, so the test
// injects the answer through ringWaitSeam, as shutdown_owed_ebusy_873 does for
// the shutdown drain's wait.
//
// What the oracle has to survive: the engine has two workers in one
// SO_REUSEPORT group, so a dead worker's listener leaves the group and new
// connections reach the other one, and "the server still answers" would pass
// on the defect. So the connections are opened first, the injection armed
// after, and every one of those connections must keep answering.

// ebusyInject884 answers the loop's waits with err for the next n waits of
// each worker, once armed, and counts what it did, per worker and per mode.
type ebusyInject884 struct {
	armed atomic.Bool
	only  atomic.Int32 // -1: any mode; else the one ringWaitMode to answer
	err   error
	n     int64
	// firstOnly: only the first worker to meet the armed seam is answered,
	// and once (the EIO arm: one worker stops, the other must not).
	firstOnly bool
	chosen    atomic.Pointer[Worker]

	mu sync.Mutex
	ws map[*Worker]*ebusyWorker884
}

type ebusyWorker884 struct {
	left     atomic.Int64
	seen     atomic.Int64
	injected [4]atomic.Int64
}

func (j *ebusyInject884) state(w *Worker) *ebusyWorker884 {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.ws == nil {
		j.ws = map[*Worker]*ebusyWorker884{}
	}
	st := j.ws[w]
	if st == nil {
		st = &ebusyWorker884{}
		st.left.Store(j.n)
		j.ws[w] = st
	}
	return st
}

func (j *ebusyInject884) seam(w *Worker, mode ringWaitMode, err error) error {
	if !j.armed.Load() {
		return err
	}
	st := j.state(w)
	st.seen.Add(1)
	if o := j.only.Load(); o >= 0 && ringWaitMode(o) != mode {
		return err
	}
	if j.firstOnly {
		if !j.chosen.CompareAndSwap(nil, w) && j.chosen.Load() != w {
			return err
		}
	}
	if st.left.Add(-1) < 0 {
		return err
	}
	st.injected[mode].Add(1)
	return j.err
}

func (j *ebusyInject884) total(mode int) (n int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, st := range j.ws {
		if mode < 0 {
			for i := range st.injected {
				n += st.injected[i].Load()
			}
		} else {
			n += st.injected[mode].Load()
		}
	}
	return n
}

func (j *ebusyInject884) summary() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := ""
	for w, st := range j.ws {
		s += fmt.Sprintf(" [worker %d seen=%d injected sqpoll/ready/sends/timed=%d/%d/%d/%d]", w.id, st.seen.Load(),
			st.injected[waitModeSQPoll].Load(), st.injected[waitModeSubmitReady].Load(),
			st.injected[waitModeSubmitSends].Load(), st.injected[waitModeTimed].Load())
	}
	return s
}

func ebusyErr884() error {
	return fmt.Errorf("io_uring_enter submit+wait: %w", unix.EBUSY)
}

// setRingWaitSeam884 installs the seam before the engine starts and removes it
// after the engine has stopped: the cleanup is registered first, so it runs
// last, and the workers read the variable without a lock.
func setRingWaitSeam884(t *testing.T, fn func(*Worker, ringWaitMode, error) error) {
	t.Helper()
	old := ringWaitSeam
	ringWaitSeam = fn
	t.Cleanup(func() { ringWaitSeam = old })
}

// warmLoad884 dials n keep-alive connections, each answered once, so every
// worker that will own one does; the returned stop ends the load the
// connections then carry and reports what the clients saw.
func warmLoad884(t *testing.T, addr string, n int) (stop func() loadResult) {
	t.Helper()
	ch := make(chan struct{})
	wait := runKeepAliveLoad(t, addr, n, ch)
	return func() loadResult {
		close(ch)
		return wait()
	}
}

// TestRunSurvivesARingWaitEBUSY884: whichever of the loop's waits meets the
// kernel's EBUSY, the worker reaps the CQ ring and waits again; the
// connections it owns keep answering and no worker stops.
func TestRunSurvivesARingWaitEBUSY884(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode int32 // -1 any
	}{
		{"any-wait", -1},
		{"submit-and-wait-timeout", int32(waitModeTimed)},
		{"submit-and-wait-sends-pending", int32(waitModeSubmitSends)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inj := &ebusyInject884{err: ebusyErr884(), n: 40}
			inj.only.Store(tc.mode)
			setRingWaitSeam884(t, inj.seam)
			e, addr := startFDLEngine(t, transplantTestHandler{}, nil)

			stop := warmLoad884(t, addr, 32)
			time.Sleep(200 * time.Millisecond) // every connection answers, un-injected
			inj.armed.Store(true)
			deadline := time.Now().Add(3 * time.Second)
			for inj.total(int(tc.mode)) < inj.n && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(200 * time.Millisecond) // and serves on after the last one
			res := stop()

			t.Logf("celeris884 mode=%s clients ok=%d errs=%d (%s) injected=%d%s", tc.name, res.ok, res.errs,
				classes(res.byClass), inj.total(-1), inj.summary())
			if inj.total(int(tc.mode)) == 0 {
				t.Fatalf("the injected EBUSY never reached a %s wait: the test would prove nothing", tc.name)
			}
			if res.errs != 0 {
				t.Errorf("%d client requests failed (%s) after the loop's wait answered EBUSY: the worker stopped serving",
					res.errs, classes(res.byClass))
			}
			var retried uint64
			for _, w := range e.workers {
				retried += w.ringWaitRetries.Load()
			}
			if want := uint64(inj.total(-1)); retried != want {
				t.Errorf("the workers counted %d retried waits, the injection answered %d", retried, want)
			}
			for i, w := range e.workers {
				if w.exited.Load() {
					t.Errorf("worker %d (index %d) stopped after EBUSY waits", w.id, i)
				}
			}
			c, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				t.Fatalf("dial after the injection: %v", err)
			}
			_ = c.Close()
		})
	}
}

// TestRunStopsOnARingWaitFailure884: the retry is for the CQ ring being full,
// not for the ring failing. An EIO from a wait still ends the worker that met
// it (and only that one), which closes what it owns.
func TestRunStopsOnARingWaitFailure884(t *testing.T) {
	inj := &ebusyInject884{err: fmt.Errorf("io_uring_enter submit+wait: %w", unix.EIO), n: 1, firstOnly: true}
	inj.only.Store(-1)
	setRingWaitSeam884(t, inj.seam)
	e, addr := startFDLEngine(t, transplantTestHandler{}, nil)

	stop := warmLoad884(t, addr, 32)
	time.Sleep(200 * time.Millisecond)
	inj.armed.Store(true)
	deadline := time.Now().Add(3 * time.Second)
	var dead *Worker
	for time.Now().Before(deadline) && dead == nil {
		for _, w := range e.workers {
			if w.exited.Load() {
				dead = w
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	res := stop()
	t.Logf("celeris884 EIO clients ok=%d errs=%d (%s) injected=%d%s", res.ok, res.errs, classes(res.byClass), inj.total(-1), inj.summary())
	if inj.total(-1) != 1 {
		t.Fatalf("injected %d EIO answers, want exactly 1", inj.total(-1))
	}
	if dead == nil {
		t.Fatal("a worker whose ring wait failed with EIO went on: the loop retries errors that are not the full CQ ring's")
	}
	for _, w := range e.workers {
		if n := w.ringWaitRetries.Load(); n != 0 {
			t.Errorf("worker %d counted %d retried waits for an EIO, which is not retryable", w.id, n)
		}
	}
	if dead != inj.chosen.Load() {
		t.Errorf("worker %d stopped, but the EIO was answered to worker %d", dead.id, inj.chosen.Load().id)
	}
	for _, w := range e.workers {
		if w != dead && w.exited.Load() {
			t.Errorf("worker %d stopped though only worker %d met the failure", w.id, dead.id)
		}
	}
}

// ringWaitSites884 returns the position of every ring wait in fn that its
// loop does not route through ringWaitContinues: a call of SubmitAndWait,
// SubmitAndWaitTimeout or WaitCQETimeout on a ring that is not assigned to
// err and followed, as the next statement of its block, by an if whose
// condition calls ringWaitContinues. It also returns how many waits it saw.
//
// Mode 1 of the loop (CQEs ready with SQEs pending) cannot be driven from the
// engine: the loop submits what it queued before it looks at the ring again,
// so under load a run of 250 000 passes never met it. The structure pins that
// wait, and the SQPOLL one (no tier sets SQPOLL: SQPollIdle is 0 in all three),
// to the same helper the engine arms exercise.
func ringWaitSites884(fset *token.FileSet, f *ast.File) (bad []string, seen int) {
	isWait := func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		switch sel.Sel.Name {
		case "SubmitAndWait", "SubmitAndWaitTimeout", "WaitCQETimeout":
			return true
		}
		return false
	}
	callsHelper := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(m ast.Node) bool {
			if call, ok := m.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ringWaitContinues" {
					found = true
				}
			}
			return !found
		})
		return found
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Name.Name != "run" {
			continue
		}
		covered := map[ast.Node]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			blk, ok := n.(*ast.BlockStmt)
			if !ok {
				return true
			}
			for i, st := range blk.List {
				as, ok := st.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 || !isWait(as.Rhs[0]) {
					continue
				}
				if id, ok := as.Lhs[0].(*ast.Ident); !ok || id.Name != "err" || i+1 >= len(blk.List) {
					continue
				}
				if ifs, ok := blk.List[i+1].(*ast.IfStmt); ok && callsHelper(ifs.Cond) {
					covered[as.Rhs[0]] = true
				}
			}
			return true
		})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if isWait(n) {
				seen++
				if !covered[n] {
					bad = append(bad, fset.Position(n.Pos()).String())
				}
			}
			return true
		})
	}
	return bad, seen
}

// TestEveryRingWaitOfRunGoesThroughTheHelper884: the four waits of Worker.run
// (SQPOLL's WaitCQETimeout, SubmitAndWait twice, SubmitAndWaitTimeout) each
// answer an error through ringWaitContinues, none by shutting down itself.
func TestEveryRingWaitOfRunGoesThroughTheHelper884(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "worker.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	bad, seen := ringWaitSites884(fset, f)
	if seen != 4 {
		t.Errorf("Worker.run has %d ring waits, want 4: the scan or the loop changed", seen)
	}
	for _, b := range bad {
		t.Errorf("%s: a ring wait of Worker.run that does not answer its error through ringWaitContinues (celeris#884)", b)
	}
}

// TestRingWaitScanSeesAnUnroutedWait884 is the scan's own control: of the
// snippet's waits only the one that shuts down itself, and the one whose
// result is dropped, are reported.
func TestRingWaitScanSeesAnUnroutedWait884(t *testing.T) {
	const src = `package iouring
func (w *Worker) run() {
	err := w.ring.SubmitAndWait()
	if (err != nil || ringWaitSeam != nil) && !w.ringWaitContinues(1, err) { return }
	err = w.ring.SubmitAndWaitTimeout(1)
	if err != nil { w.shutdown(); return }
	_ = w.ring.WaitCQETimeout(1)
	if err := w.ring.SubmitAndWait(); err != nil { return }
	err = w.ring.WaitCQETimeout(1)
	if !w.ringWaitContinues(1, err) { return }
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	bad, seen := ringWaitSites884(fset, f)
	if seen != 5 || len(bad) != 3 {
		t.Errorf("the scan saw %d waits and %d unrouted, want 5 and 3: %v", seen, len(bad), bad)
	}
}
