//go:build linux

package epoll

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
)

// celeris#874 (epoll half, from #819 item 1). The run loop moves the three
// per-iteration batches (reqBatch, bytesReadBatch, bytesWrittenBatch) into the
// shared atomics once per iteration, and has returned by the time shutdown()
// runs. drainSends flushes the responses still queued with onLoopThread=true,
// which adds to bytesWrittenBatch, so every byte the shutdown drain sent was
// counted nowhere: Metrics().BytesWritten read after Shutdown fell short of
// what went out, by up to whole responses.

// drainRig874 is a shutdown-ready loop with one conn holding `queued` bytes
// the socket cannot take at once, and a reader on the peer that counts what
// arrives until the loop closes the conn.
type drainRig874 struct {
	l    *Loop
	fd   int
	peer int
	got  atomic.Int64
	done chan struct{}
}

func newDrainRig874(t *testing.T, queued int) *drainRig874 {
	t.Helper()
	l := shutdownLoop863(t, nil)
	l.async = false
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	r := &drainRig874{l: l, fd: pair[0], peer: pair[1], done: make(chan struct{})}
	t.Cleanup(func() { _ = unix.Close(r.peer) })
	if r.fd >= len(l.conns) {
		_ = unix.Close(r.fd)
		t.Skipf("socketpair fd %d outside the test conn table", r.fd)
	}
	_ = unix.SetsockoptInt(r.fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 32<<10)
	cs := acquireConnState(t.Context(), r.fd, 4096, false)
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.h1State = conn.NewH1State()
	cs.remoteAddr = "127.0.0.1:9"
	cs.writeBuf = append(cs.writeBuf[:0], make([]byte, queued)...)
	l.conns[r.fd] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)
	// The handler's response flushed as far as the socket takes it, then the
	// end of the loop iteration that did it: what run() does before the next
	// epoll_wait.
	if err := l.flushWrites(cs, true); err != nil {
		t.Fatalf("flushWrites: %v", err)
	}
	if !csWritePending(cs) {
		t.Fatalf("celeris874 PREMISE: %d queued bytes all went out; the socket must be too small", queued)
	}
	l.bytesWritten.Add(l.bytesWrittenBatch)
	l.bytesWrittenBatch = 0
	return r
}

// startReader reads the peer until the conn is closed (EOF) and counts bytes.
func (r *drainRig874) startReader() {
	go func() {
		defer close(r.done)
		buf := make([]byte, 64<<10)
		fds := []unix.PollFd{{Fd: int32(r.peer), Events: unix.POLLIN}}
		for {
			n, err := unix.Read(r.peer, buf)
			switch {
			case n > 0:
				r.got.Add(int64(n))
			case n == 0 && err == nil:
				return // EOF: shutdown closed the conn
			case err == unix.EAGAIN:
				_, _ = unix.Poll(fds, 20)
			case err == unix.EINTR:
			default:
				return
			}
		}
	}()
}

func (r *drainRig874) wait(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(20 * time.Second):
		t.Fatal("the reader never saw the conn close")
	}
}

// TestShutdownCountsTheBytesItsSendDrainSends is the issue: every byte the
// shutdown drain sent must be in BytesWritten once shutdown has returned, so
// BytesWritten equals what the client received.
func TestShutdownCountsTheBytesItsSendDrainSends(t *testing.T) {
	const queued = 2 << 20
	r := newDrainRig874(t, queued)
	flushedBefore := r.l.bytesWritten.Load() // what the run loop counted before shutdown
	r.startReader()

	r.l.shutdown()
	r.wait(t)

	got := uint64(r.got.Load())
	counted := r.l.bytesWritten.Load()
	if got != queued {
		t.Fatalf("celeris874 PREMISE: the client received %d bytes, want the %d queued; the drain did not finish", got, queued)
	}
	if got <= flushedBefore {
		t.Fatalf("celeris874 PREMISE: the drain sent no bytes (client got %d, %d counted before shutdown)", got, flushedBefore)
	}
	if counted != got {
		t.Errorf("BytesWritten = %d after shutdown, but the client received %d: the %d bytes the shutdown drain sent "+
			"were added to a batch no iteration will flush (celeris#874)", counted, got, int64(got)-int64(counted))
	}
}

// TestShutdownFlushesEveryBatchedCounter pins the whole class rather than the
// drain's one instance: whatever the per-iteration batches hold when the loop
// stops reaches the shared counters, requests and received bytes too.
func TestShutdownFlushesEveryBatchedCounter(t *testing.T) {
	l := shutdownLoop863(t, nil)
	l.async = false
	l.reqBatch, l.bytesReadBatch, l.bytesWrittenBatch = 3, 5, 7

	l.shutdown()

	if got := l.reqCount.Load(); got != 3 {
		t.Errorf("reqCount = %d, want 3: reqBatch was dropped at shutdown", got)
	}
	if got := l.bytesRead.Load(); got != 5 {
		t.Errorf("bytesRead = %d, want 5: bytesReadBatch was dropped at shutdown", got)
	}
	if got := l.bytesWritten.Load(); got != 7 {
		t.Errorf("bytesWritten = %d, want 7: bytesWrittenBatch was dropped at shutdown", got)
	}
	if l.reqBatch != 0 || l.bytesReadBatch != 0 || l.bytesWrittenBatch != 0 {
		t.Errorf("batches not reset: req=%d read=%d written=%d", l.reqBatch, l.bytesReadBatch, l.bytesWrittenBatch)
	}
}

// celeris#885 item 37: the run loop flushes the batches inline, once per
// iteration (the per-event path, so not a call), and flushBatchedCounters, which
// shutdown calls, repeats those flushes for what the loop's last work added
// after the last one. A batch added to the Loop and to one of the two is
// silently missing from the other. io_uring's TestShutdownFlushesEveryBatch874
// guards its worker with a reflection walk over the *Batch fields; these two
// tests are the epoll twin, and a parse of the two functions, which the walk
// cannot see: it exercises flushBatchedCounters only, never the run loop.

// batchFields885 returns the names of the Loop's *Batch fields, in
// declaration order, each of which must be a uint64.
func batchFields885(t *testing.T) []string {
	t.Helper()
	var out []string
	rt := reflect.TypeOf(Loop{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !strings.HasSuffix(f.Name, "Batch") {
			continue
		}
		if f.Type.Kind() != reflect.Uint64 {
			t.Errorf("Loop.%s is a %s: the batch guards set and read uint64 batches", f.Name, f.Type)
			continue
		}
		out = append(out, f.Name)
	}
	if len(out) < 3 {
		t.Fatalf("found only %v: the guard no longer sees the three batches it exists for (reqBatch, bytesReadBatch, bytesWrittenBatch)", out)
	}
	return out
}

// batchField885 is l's unexported field name, settable.
func batchField885(l *Loop, name string) reflect.Value {
	f := reflect.ValueOf(l).Elem().FieldByName(name)
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
}

// TestFlushBatchedCountersFlushesEveryBatchField885 gives every *Batch field of
// a bare Loop (found by reflection, so a batch added later is covered, not
// listed) a distinct value, runs flushBatchedCounters, and requires every one
// to be zero afterwards and the three known batches to have reached their
// counters, each to its own.
func TestFlushBatchedCountersFlushesEveryBatchField885(t *testing.T) {
	l := &Loop{reqCount: &atomic.Uint64{}, bytesRead: &atomic.Uint64{}, bytesWritten: &atomic.Uint64{}}
	fields := batchFields885(t)
	for i, name := range fields {
		batchField885(l, name).SetUint(uint64(1) << (i + 1))
	}
	// Distinct values, so a counter fed from the wrong batch is not a pass.
	l.reqBatch, l.bytesReadBatch, l.bytesWrittenBatch = 3, 5, 7
	for _, name := range fields {
		if batchField885(l, name).Uint() == 0 {
			t.Fatalf("setup: %s is zero", name)
		}
	}

	l.flushBatchedCounters()

	for _, c := range []struct {
		name      string
		got, want uint64
	}{
		{"reqCount (reqBatch)", l.reqCount.Load(), 3},
		{"bytesRead (bytesReadBatch)", l.bytesRead.Load(), 5},
		{"bytesWritten (bytesWrittenBatch)", l.bytesWritten.Load(), 7},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d after flushBatchedCounters, want %d", c.name, c.got, c.want)
		}
	}
	for _, name := range fields {
		if left := batchField885(l, name).Uint(); left != 0 {
			t.Errorf("Loop.%s = %d after flushBatchedCounters: a batch the shutdown copy of the run loop's flush does not flush", name, left)
		}
	}
}

// batchFlush885 is what a method does with one *Batch field: the counter it adds
// the field to (l.counter.Add(l.xBatch)), "" if none, and whether it zeroes it
// (l.xBatch = 0).
type batchFlush885 struct {
	counter string
	zeroed  bool
}

// flushedBatches885 parses loop.go and returns, for the body of the method
// named fn on *Loop, what it does with each *Batch field it names.
func flushedBatches885(t *testing.T, fn string) map[string]batchFlush885 {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "loop.go", nil, 0)
	if err != nil {
		t.Fatalf("parse loop.go: %v", err)
	}
	batchOf := func(e ast.Expr) (string, bool) {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || !strings.HasSuffix(sel.Sel.Name, "Batch") {
			return "", false
		}
		return sel.Sel.Name, true
	}
	var body *ast.BlockStmt
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn && fd.Recv != nil && len(fd.Recv.List) == 1 {
			if star, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok {
				if id, ok := star.X.(*ast.Ident); ok && id.Name == "Loop" {
					body = fd.Body
				}
			}
		}
	}
	if body == nil {
		t.Fatalf("loop.go has no method (*Loop).%s: the guard needs updating", fn)
	}
	out := map[string]batchFlush885{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt: // l.xBatch = 0
			if lit, ok := n.Rhs[0].(*ast.BasicLit); ok && len(n.Lhs) == 1 && n.Tok == token.ASSIGN && lit.Value == "0" {
				if name, ok := batchOf(n.Lhs[0]); ok {
					f := out[name]
					f.zeroed = true
					out[name] = f
				}
			}
		case *ast.CallExpr: // l.counter.Add(l.xBatch)
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Add" && len(n.Args) == 1 {
				if name, ok := batchOf(n.Args[0]); ok {
					if counter, ok := sel.X.(*ast.SelectorExpr); ok {
						f := out[name]
						f.counter = counter.Sel.Name
						out[name] = f
					}
				}
			}
		}
		return true
	})
	return out
}

// TestRunLoopAndFlushBatchedCountersFlushTheSameBatches885: the run loop's
// inline flush and flushBatchedCounters add the same batches to the same
// counters and zero them, and both cover every *Batch field of the Loop. A
// fourth batch flushed by one of them only, or one fed to another counter by
// the shutdown copy, fails here; the reflection test above sees only the copy.
func TestRunLoopAndFlushBatchedCountersFlushTheSameBatches885(t *testing.T) {
	inRun := flushedBatches885(t, "run")
	inFlush := flushedBatches885(t, "flushBatchedCounters")
	fields := batchFields885(t)
	t.Logf("celeris885 batches: Loop fields %v; run flushes %v; flushBatchedCounters flushes %v", fields, inRun, inFlush)
	if !maps.Equal(inRun, inFlush) {
		t.Errorf("(*Loop).run flushes %v but flushBatchedCounters flushes %v (batch -> counter, zeroed): a batch added to one is missing from the other", inRun, inFlush)
	}
	got := slices.Sorted(maps.Keys(inRun))
	want := slices.Clone(fields)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("(*Loop).run names the batches %v but the Loop has the batch fields %v: a batch that no flush covers", got, want)
	}
	for name, f := range inRun {
		if f.counter == "" || !f.zeroed {
			t.Errorf("(*Loop).run does not both add %s to a counter and zero it: %+v", name, f)
		}
	}
}
