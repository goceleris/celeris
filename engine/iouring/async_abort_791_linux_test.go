//go:build linux

package iouring

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#791: a dispatch goroutine whose handler panics, or calls
// runtime.Goexit, must not take its worker down with it.
//
// runAsyncHandler holds cs.detachMu across ProcessH1. Its deferred recover()
// handed the conn to the worker for teardown without releasing that lock, and
// the worker's closeConn then waited on it forever: the LockOSThread'd worker
// stopped serving every connection of its ring, and Listen never returned
// after its context was cancelled. A Goexit was not recovered at all, so the
// lock leaked with the goroutine and the conn stayed owned by nobody.
//
// Each test starts an io_uring engine with AsyncHandlers and one async route,
// /boom. Before the fault it opens, on every worker, a keep-alive WITNESS
// connection and a BOOM connection, each proven to be served by that worker
// (the handler answers /ok with its worker id). Then every boom connection
// sends /boom. The worker must then still answer its witness, eight fresh
// connections must be answered, and Listen must return after cancel, every
// read within abortBudget791, and ActiveConnections must settle at the
// witness count (every /boom conn closed and accounted). The error arm is the
// control: /boom returns an error, which reaches the same asyncClosed
// teardown with the lock released, so it passes on the base as well; that
// the rig can pass is what makes the other arms' failures mean something.
//
// The two Detach arms pin the teardown's guard. A handler that calls
// Context.Detach (the websocket and sse middleware do) runs the engine's
// OnDetach, which releases cs.detachMu on the dispatch goroutine's behalf
// (celeris#273). If such a handler then panics or calls runtime.Goexit, the
// teardown must not release the lock a second time: that Unlock is a fatal
// "unlock of unlocked mutex", which ends the whole process (cf. celeris#309).
// Detach-then-panic passes on the base, whose recover never unlocked;
// Detach-then-Goexit fails there as the plain Goexit does, the conn never
// closed.

// abortBudget791 bounds every read and the stop. A worker that serves answers
// in well under a millisecond; the rest absorbs a loaded -race run.
const abortBudget791 = 2 * time.Second

// abortFresh791 is how many fresh connections must be answered after the fault.
const abortFresh791 = 8

// abortSettle791 bounds the wait for ActiveConnections to settle at the
// witness count once the fault's conns and the test's own closed conns have
// been torn down.
const abortSettle791 = 5 * time.Second

// The ways /boom ends its handler.
const (
	abortPanic791  = "panic"
	abortGoexit791 = "goexit"
	abortError791  = "error" // the control
	// Context.Detach's engine half (stream.OnDetach), then a panic or a
	// Goexit.
	abortDetachPanic791  = "detach-panic"
	abortDetachGoexit791 = "detach-goexit"
)

// The engine's log messages for the two abnormal exits (abortAsyncHandler).
const (
	abortPanicMsg791  = "async handler panicked"
	abortGoexitMsg791 = "async handler exited without returning (runtime.Goexit)"
)

type abortHandler791 struct {
	mode     string
	hits     *atomic.Int64 // /boom invocations: the fault was injected this many times
	detaches *atomic.Int64 // OnDetach calls by /boom (the Detach arms)
}

func (h abortHandler791) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.Path == "/boom" {
		h.hits.Add(1)
		switch h.mode {
		case abortPanic791:
			panic("celeris791: handler panic")
		case abortGoexit791:
			runtime.Goexit()
		case abortDetachPanic791, abortDetachGoexit791:
			// What Context.Detach does on an engine stream: OnDetach, which
			// on a dispatch goroutine releases cs.detachMu on its behalf.
			// A stream without OnDetach is counted as no detach, which the
			// test reports as a failed injection.
			if s.OnDetach == nil {
				return errors.New("celeris791: no OnDetach on the async stream")
			}
			s.OnDetach()
			h.detaches.Add(1)
			if h.mode == abortDetachGoexit791 {
				runtime.Goexit()
			}
			panic("celeris791: handler panic after Detach")
		default:
			return errors.New("celeris791: handler error")
		}
	}
	if s.ResponseWriter == nil {
		return nil
	}
	id, _ := ctxkit.WorkerIDFrom(ctx)
	body := "w=" + strconv.Itoa(id)
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(body))}},
		[]byte(body))
}
func (abortHandler791) RouteAsync(_, path string) bool { return path == "/boom" }
func (abortHandler791) HasAsyncRoutes() bool           { return true }

var _ stream.AsyncRouteResolver = abortHandler791{}

// abortLog791 counts the engine's abnormal-exit log records, the witness that
// the fault ran on a dispatch goroutine and reached its deferred teardown.
type abortLog791 struct{ panics, goexits atomic.Int64 }

func (l *abortLog791) Enabled(_ context.Context, lvl slog.Level) bool { return lvl >= slog.LevelError }
func (l *abortLog791) Handle(_ context.Context, r slog.Record) error {
	switch r.Message {
	case abortPanicMsg791:
		l.panics.Add(1)
	case abortGoexitMsg791:
		l.goexits.Add(1)
	}
	return nil
}
func (l *abortLog791) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *abortLog791) WithGroup(string) slog.Handler      { return l }

type abortConn791 struct {
	c  net.Conn
	br *bufio.Reader
}

func abortDial791(t *testing.T, addr string) *abortConn791 {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, abortBudget791)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &abortConn791{c: c, br: bufio.NewReader(c)}
}

// get sends one request and reports the status and body, or how it failed.
func (a *abortConn791) get(path string) (status int, body string, err error) {
	_ = a.c.SetDeadline(time.Now().Add(abortBudget791))
	if _, err := fmt.Fprintf(a.c, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path); err != nil {
		return 0, "", err
	}
	resp, err := http.ReadResponse(a.br, nil)
	if err != nil {
		return 0, "", err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b), err
}

// abortClass791 names a failed read: timeout (nothing came back within the
// budget), closed (EOF or reset: the conn was torn down), or the error.
func abortClass791(err error) string {
	var ne net.Error
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET):
		return "closed"
	}
	return err.Error()
}

// abortBlocked791 returns the goroutines parked in sync.(*Mutex).Lock with an
// engine/iouring frame: on the base, the worker in closeConn.
func abortBlocked791() []string {
	buf := make([]byte, 8<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "sync.(*Mutex).Lock") && strings.Contains(g, "/engine/iouring.") {
			out = append(out, g)
		}
	}
	return out
}

func runAbort791(t *testing.T, mode string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	var hits, detaches atomic.Int64
	logs := &abortLog791{}
	e, err := New(resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		Resources:     resource.Resources{Workers: 2},
		AsyncHandlers: true,
		Logger:        slog.New(logs),
	}, abortHandler791{mode: mode, hits: &hits, detaches: &detaches})
	if err != nil {
		skipOrFail656(t, "iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(cancel)
	for deadline := time.Now().Add(8 * time.Second); ; {
		if c, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond); derr == nil {
			_ = c.Close()
			if e.NumWorkers() > 0 {
				break
			}
		}
		select {
		case err := <-done:
			skipOrFail656(t, "iouring engine failed to start: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("engine did not start listening within 8s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	workers := e.NumWorkers()

	// Before the fault: a witness and a boom conn on every worker, each
	// proven to be served by it.
	witness := map[string]*abortConn791{}
	boom := map[string]*abortConn791{}
	for range 64 {
		if len(witness) == workers && len(boom) == workers {
			break
		}
		a := abortDial791(t, addr)
		status, body, err := a.get("/ok")
		if err != nil || status != 200 || !strings.HasPrefix(body, "w=") {
			t.Fatalf("PREMISE: /ok before the fault: status %d body %q err %v", status, body, err)
		}
		switch {
		case witness[body] == nil:
			witness[body] = a
		case boom[body] == nil:
			boom[body] = a
		default:
			_ = a.c.Close()
		}
	}
	if len(witness) != workers || len(boom) != workers {
		t.Fatalf("PREMISE: 64 dials put a witness and a boom conn on %d and %d of %d workers",
			len(witness), len(boom), workers)
	}
	ids := make([]string, 0, workers)
	for id := range boom {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	// The fault, once per worker.
	var boomOut []string
	for _, id := range ids {
		status, _, err := boom[id].get("/boom")
		out := abortClass791(err)
		if err == nil {
			out = "status=" + strconv.Itoa(status)
		}
		boomOut = append(boomOut, id+":"+out)
	}

	// After it: every worker answers its witness, fresh conns are answered.
	witnessOK := 0
	var witnessOut []string
	for _, id := range ids {
		status, body, err := witness[id].get("/ok")
		if err == nil && status == 200 && body == id {
			witnessOK++
			witnessOut = append(witnessOut, id+":ok")
		} else {
			witnessOut = append(witnessOut, fmt.Sprintf("%s:%s/%d/%q", id, abortClass791(err), status, body))
		}
	}
	freshOK := 0
	freshOut := map[string]int{}
	for range abortFresh791 {
		c, err := net.DialTimeout("tcp", addr, abortBudget791)
		if err != nil {
			freshOut["dial:"+abortClass791(err)]++
			continue
		}
		a := &abortConn791{c: c, br: bufio.NewReader(c)}
		status, body, err := a.get("/ok")
		_ = c.Close()
		if err == nil && status == 200 && strings.HasPrefix(body, "w=") {
			freshOK++
			freshOut[body]++
		} else {
			freshOut[abortClass791(err)]++
		}
	}
	// Every conn but the witnesses is gone: the boom conns closed by the
	// engine, the fresh and surplus ones by the test. ActiveConnections is
	// this engine's own count, so no other test's state reaches it.
	var active int64
	for deadline := time.Now().Add(abortSettle791); ; time.Sleep(20 * time.Millisecond) {
		active = e.Metrics().ActiveConnections
		if active == int64(workers) || time.Now().After(deadline) {
			break
		}
	}
	var blocked []string
	if witnessOK != workers || freshOK != abortFresh791 {
		blocked = abortBlocked791()
	}

	cancel()
	stopped := false
	select {
	case <-done:
		stopped = true
	case <-time.After(5 * time.Second):
	}

	t.Logf("celeris791 RESULT engine=io_uring mode=%s workers=%d hits=%d detaches=%d log_panics=%d log_goexits=%d "+
		"boom=%v witness=%d/%d %v fresh=%d/%d %v active=%d/%d stopped=%v blocked=%d",
		mode, workers, hits.Load(), detaches.Load(), logs.panics.Load(), logs.goexits.Load(), boomOut,
		witnessOK, workers, witnessOut, freshOK, abortFresh791, freshOut, active, workers, stopped, len(blocked))
	for _, g := range blocked {
		t.Logf("celeris791 BLOCKED\n%s", g)
	}

	// The fault was injected: once per worker, on the dispatch goroutine.
	if n := hits.Load(); n != int64(workers) {
		t.Errorf("INJECTION: /boom ran %d times, want %d (one per worker)", n, workers)
	}
	switch mode {
	case abortDetachPanic791, abortDetachGoexit791:
		if n := detaches.Load(); n != int64(workers) {
			t.Errorf("INJECTION: /boom ran OnDetach %d times, want %d (one per worker)", n, workers)
		}
	default:
		if n := detaches.Load(); n != 0 {
			t.Errorf("INJECTION: /boom (%s) ran OnDetach %d times, want 0", mode, n)
		}
	}
	switch mode {
	case abortPanic791, abortDetachPanic791:
		if n := logs.panics.Load(); n != int64(workers) {
			t.Errorf("INJECTION: the engine logged %d recovered panics, want %d", n, workers)
		}
		if n := logs.goexits.Load(); n != 0 {
			t.Errorf("INJECTION: the engine logged %d handler Goexits for a panic, want 0", n)
		}
	case abortGoexit791, abortDetachGoexit791:
		if n := logs.goexits.Load(); n != int64(workers) {
			t.Errorf("the engine logged %d handler Goexits, want %d: a Goexit took no teardown", n, workers)
		}
		if n := logs.panics.Load(); n != 0 {
			t.Errorf("INJECTION: the engine logged %d recovered panics for a Goexit, want 0", n)
		}
	default:
		if p, g := logs.panics.Load(), logs.goexits.Load(); p != 0 || g != 0 {
			t.Errorf("CONTROL: a handler error took the abnormal-exit teardown (%d panics, %d Goexits logged)", p, g)
		}
	}
	for _, o := range boomOut {
		if strings.HasSuffix(o, ":timeout") || strings.HasSuffix(o, ":status=200") {
			t.Errorf("celeris#791: /boom (%s) got %s; want its conn torn down within %v", mode, o, abortBudget791)
		}
	}
	if witnessOK != workers {
		t.Errorf("celeris#791: after a /boom (%s) on every worker, %d of %d workers answered their witness conn "+
			"within %v (%v): the worker stopped serving", mode, witnessOK, workers, abortBudget791, witnessOut)
	}
	if freshOK != abortFresh791 {
		t.Errorf("celeris#791: after a /boom (%s) on every worker, %d of %d fresh conns were answered within %v (%v)",
			mode, freshOK, abortFresh791, abortBudget791, freshOut)
	}
	if active != int64(workers) {
		t.Errorf("celeris#791: after a /boom (%s) on every worker, ActiveConnections settled at %d within %v, "+
			"want %d (the witness conns): a /boom conn was never closed", mode, active, abortSettle791, workers)
	}
	if !stopped {
		t.Errorf("celeris#791: Listen did not return within 5s of cancel after a /boom (%s)", mode)
	}
}

func TestIouringAsyncHandlerPanicLeavesItsWorkerServing(t *testing.T) {
	runAbort791(t, abortPanic791)
}

func TestIouringAsyncHandlerGoexitLeavesItsWorkerServing(t *testing.T) {
	runAbort791(t, abortGoexit791)
}

// TestIouringAsyncHandlerErrorLeavesItsWorkerServing is the control: the same
// teardown, entered by a normal return. It passes on the base too.
func TestIouringAsyncHandlerErrorLeavesItsWorkerServing(t *testing.T) {
	runAbort791(t, abortError791)
}

// The Detach arms: the teardown must leave alone the lock that OnDetach has
// already released (see the top of the file). With the guard gone the
// process dies of "sync: unlock of unlocked mutex" and the test prints no
// result line at all.
func TestIouringAsyncHandlerDetachThenPanicLeavesItsWorkerServing(t *testing.T) {
	runAbort791(t, abortDetachPanic791)
}

func TestIouringAsyncHandlerDetachThenGoexitLeavesItsWorkerServing(t *testing.T) {
	runAbort791(t, abortDetachGoexit791)
}
