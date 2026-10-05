//go:build linux

package adaptive

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#791 through the adaptive engine, which runs epoll's and io_uring's
// dispatch goroutines unchanged and so inherited the defect from both: a
// handler panic on a dispatch goroutine left cs.detachMu locked, and the
// sub-engine's closeConn then parked the loop or worker that owned the conn,
// with every other connection on it.
//
// Two shapes. On the start engine (epoll), as the engine-level tests do it.
// And on a connection the switch moved: keep-alive connections promoted to
// their dispatch goroutines on epoll, handed to io_uring by ForceSwitch
// (#383), then /boom on one of them on its new worker. That is the adaptive
// engine's own path to the defect: the adopted conn's dispatch goroutine is
// io_uring's, started on a connState io_uring built at the hand-off.
//
// In both, every worker that takes a /boom must still answer a keep-alive
// witness connection it serves, fresh connections must be answered, and
// Listen must return after cancel, every read within abortBudget791.

const (
	abortBudget791    = 2 * time.Second
	abortFresh791     = 8
	abortPanicMsg791  = "async handler panicked"
	abortAdoptConns   = 8
	abortAdoptBound   = 3 * time.Second
	abortAdaptiveStop = 10 * time.Second
)

// abortHandler791: /boom (async) panics, /aok (async) and /ok (inline) answer
// with the serving worker's id.
type abortHandler791 struct{ hits *atomic.Int64 }

func (h abortHandler791) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.Path == "/boom" {
		h.hits.Add(1)
		panic("celeris791: handler panic")
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
func (abortHandler791) RouteAsync(_, path string) bool { return path == "/boom" || path == "/aok" }
func (abortHandler791) HasAsyncRoutes() bool           { return true }

var _ stream.AsyncRouteResolver = abortHandler791{}

// abortLog791 counts the sub-engines' recovered-panic log records.
type abortLog791 struct{ panics atomic.Int64 }

func (l *abortLog791) Enabled(_ context.Context, lvl slog.Level) bool { return lvl >= slog.LevelError }
func (l *abortLog791) Handle(_ context.Context, r slog.Record) error {
	if r.Message == abortPanicMsg791 {
		l.panics.Add(1)
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

func TestAdaptiveAsyncHandlerPanicLeavesTheStartEngineServing(t *testing.T) {
	abortAdaptive791(t, false)
}

func TestAdaptiveAsyncHandlerPanicOnAnAdoptedConnLeavesItsWorkerServing(t *testing.T) {
	abortAdaptive791(t, true)
}

func abortAdaptive791(t *testing.T, adopted bool) {
	var hits atomic.Int64
	logs := &abortLog791{}
	// Two workers per sub-engine: with abortAdoptConns conns on at most two
	// io_uring workers, one of them holds at least four (the premise below
	// needs two on one worker; at one conn per worker it would not hold).
	cfg := resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, AsyncHandlers: true,
		Resources: resource.Resources{Workers: 2}, Logger: slog.New(logs)}
	e, err := New(cfg, abortHandler791{hits: &hits}, nil)
	if err != nil {
		s0SkipUnlessRequired(t, "adaptive.New unsupported here: %v", err)
	}
	e.ctrl.cooldown = 0
	e.FreezeSwitching()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	for dl := time.Now().Add(3 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("adaptive engine never bound")
	}
	addr := e.Addr().String()
	start := e.ActiveEngine().Type()

	// byWorker groups keep-alive conns by the worker that answers them.
	byWorker := map[string][]*abortConn791{}
	shape := "start"
	if !adopted {
		for range 64 {
			full := len(byWorker) > 0
			for _, cs := range byWorker {
				full = full && len(cs) >= 2
			}
			if full && len(byWorker) == e.ActiveEngine().Metrics().Workers {
				break
			}
			a := abortDial791(t, addr)
			status, body, err := a.get("/ok")
			if err != nil || status != 200 {
				t.Fatalf("PREMISE: /ok before the fault: status %d body %q err %v", status, body, err)
			}
			if len(byWorker[body]) < 2 {
				byWorker[body] = append(byWorker[body], a)
			} else {
				_ = a.c.Close()
			}
		}
	} else {
		shape = "adopted"
		conns := make([]*abortConn791, 0, abortAdoptConns)
		for range abortAdoptConns {
			a := abortDial791(t, addr)
			if status, body, err := a.get("/aok"); err != nil || status != 200 {
				t.Fatalf("PREMISE: /aok on %s: status %d body %q err %v", start, status, body, err)
			}
			conns = append(conns, a)
		}
		promoted := subEngine(e, false).Metrics().AsyncPromotedConns
		if promoted < abortAdoptConns {
			t.Fatalf("PREMISE: %d of %d conns promoted to their dispatch goroutine on %s", promoted, abortAdoptConns, start)
		}
		e.ForceSwitch()
		moved := false
		for dl := time.Now().Add(abortAdoptBound); time.Now().Before(dl); time.Sleep(25 * time.Millisecond) {
			if subActive(e, false) == 0 && subActive(e, true) >= abortAdoptConns {
				moved = true
				break
			}
		}
		if !moved {
			t.Fatalf("PREMISE: %v after ForceSwitch the conns had not moved: epoll=%d io_uring=%d (want 0 and >= %d)",
				abortAdoptBound, subActive(e, false), subActive(e, true), abortAdoptConns)
		}
		for _, a := range conns {
			status, body, err := a.get("/ok")
			if err != nil || status != 200 {
				t.Fatalf("PREMISE: /ok on an adopted conn: status %d body %q err %v", status, body, err)
			}
			byWorker[body] = append(byWorker[body], a)
		}
	}
	// Every worker holding at least two of the conns takes a /boom on one
	// and is judged by another.
	var ids []string
	for id, cs := range byWorker {
		if len(cs) >= 2 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	if len(ids) == 0 {
		t.Fatalf("PREMISE: no worker holds two of the conns (%d workers seen)", len(byWorker))
	}
	faultEngine := e.ActiveEngine().Type()

	var boomOut []string
	for _, id := range ids {
		status, _, err := byWorker[id][0].get("/boom")
		out := abortClass791(err)
		if err == nil {
			out = "status=" + strconv.Itoa(status)
		}
		boomOut = append(boomOut, id+":"+out)
	}
	witnessOK := 0
	var witnessOut []string
	for _, id := range ids {
		status, body, err := byWorker[id][1].get("/ok")
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
		if err == nil && status == 200 {
			freshOK++
			freshOut[body]++
		} else {
			freshOut[abortClass791(err)]++
		}
	}
	cancel()
	stopped := false
	select {
	case <-done:
		stopped = true
	case <-time.After(abortAdaptiveStop):
	}

	t.Logf("celeris791 RESULT engine=adaptive shape=%s start=%s fault_on=%s workers_judged=%d hits=%d log_panics=%d "+
		"boom=%v witness=%d/%d %v fresh=%d/%d %v stopped=%v",
		shape, start, faultEngine, len(ids), hits.Load(), logs.panics.Load(), boomOut,
		witnessOK, len(ids), witnessOut, freshOK, abortFresh791, freshOut, stopped)

	if adopted && faultEngine != engine.IOUring {
		t.Errorf("PREMISE: the fault ran on %s, want io_uring after the switch", faultEngine)
	}
	if n := hits.Load(); n != int64(len(ids)) {
		t.Errorf("INJECTION: /boom ran %d times, want %d", n, len(ids))
	}
	if n := logs.panics.Load(); n != int64(len(ids)) {
		t.Errorf("INJECTION: the engine logged %d recovered panics, want %d", n, len(ids))
	}
	for _, o := range boomOut {
		if strings.HasSuffix(o, ":timeout") || strings.HasSuffix(o, ":status=200") {
			t.Errorf("celeris#791: /boom got %s; want its conn torn down within %v", o, abortBudget791)
		}
	}
	if witnessOK != len(ids) {
		t.Errorf("celeris#791: after a /boom on %s, %d of %d workers answered their witness conn within %v (%v)",
			faultEngine, witnessOK, len(ids), abortBudget791, witnessOut)
	}
	if freshOK != abortFresh791 {
		t.Errorf("celeris#791: after a /boom on %s, %d of %d fresh conns were answered within %v (%v)",
			faultEngine, freshOK, abortFresh791, abortBudget791, freshOut)
	}
	if !stopped {
		t.Errorf("celeris#791: Listen did not return within %v of cancel after a /boom on %s", abortAdaptiveStop, faultEngine)
	}
}
