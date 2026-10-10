//go:build linux

package epoll

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#844: the panic windows on the async dispatch goroutine that
// celeris#840 left open. This file is the same on epoll and io_uring.
//
// Window 1: a panic while the goroutine holds cs.asyncInMu, in the park loop
// of serveAsync. abortAsyncHandler took asyncInMu to end the dispatch, which
// deadlocked the goroutine on itself: it never exited (shutdown's
// asyncWG.Wait never returned), and the next feed of that conn blocked the
// LockOSThread'd worker on the same lock.
//
// Window 2: a panic inside OnDetach between "asyncDetachUnlocked = true" and
// the release of detachMu. The flag says the lock is released, so the
// teardown skipped the release (abort) or took the post-Detach branch (a
// handler that recovered the panic, as the router does), and the lock stayed
// held: the next closeConn blocked the worker on it, and a response written
// through the already-installed guarded writeFn blocked the goroutine on its
// own lock.
//
// Window 3 (TestAbortLogsAPanicValueThatSharesStreamMemory844): the abort
// released detachMu and only then formatted the panic value for the log, so a
// closeConn that met the conn in between ran CloseH1 and the application's
// slog handler formatted memory that had been zeroed.
//
// Each arm below starts an engine with AsyncHandlers, places a keep-alive
// WITNESS conn and a BOOM conn on every worker, injects the fault once per
// worker on the boom conn, and then measures what the fault left: the boom
// conn (torn down?), a touch of it (the worker meeting the conn again), the
// witness on the same worker, fresh conns, ActiveConnections, the goroutines
// still in abortAsyncHandler or runAsyncHandler, the process's goroutine and
// descriptor counts, and whether Listen returns. A control arm per window runs
// the same rig with no fault; it says what a healthy engine reports.

const (
	budget844 = 2 * time.Second
	fresh844  = 8
	settle844 = 5 * time.Second
)

// The arms.
const (
	arm844ParkRoute       = "park-route"       // window 1: RouteAsync panics at the park (application code)
	arm844ParkHook        = "park-hook"        // window 1: the engine's code under asyncInMu panics
	arm844DetachPanic     = "detach-window"    // window 2: the panic propagates to the abort
	arm844DetachRecovered = "detach-recovered" // window 2: the handler recovers it, as the router does
	arm844Headers         = "headers-log"      // window 3: the panic value is the stream's own header slice
	arm844Hold            = "hold"             // item 4: the handler waits to be released, then panics or exits
	arm844BadError        = "bad-error"        // the panic value's Error method itself panics (the abort formats it under detachMu)
)

type h844 struct {
	mode   string
	inject bool

	hits, detaches, recovered atomic.Int64
	// parkArmed: a request ran on a dispatch goroutine, so the next RouteAsync
	// call for /f is the park loop's (canRevertToInline).
	parkArmed  atomic.Bool
	routeFire  atomic.Int64 // RouteAsync panics injected
	hookFire   atomic.Int64 // hook panics injected
	startDrain atomic.Pointer[func()]

	// arm844Hold: the handler announces itself on entered, waits for release,
	// then ends as how says ("panic" or "goexit").
	how     string
	entered chan string
	release chan struct{}
}

func (h *h844) body(ctx context.Context, s *stream.Stream, what string) error {
	if s.ResponseWriter == nil {
		return nil
	}
	id, _ := ctxkit.WorkerIDFrom(ctx)
	b := what + strconv.Itoa(id)
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(b))}}, []byte(b))
}

func (h *h844) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.Path != "/f" {
		return h.body(ctx, s, "w=")
	}
	h.hits.Add(1)
	switch h.mode {
	case arm844ParkRoute:
		h.parkArmed.Store(h.inject)
		return h.body(ctx, s, "w=")
	case arm844ParkHook:
		if f := h.startDrain.Load(); f != nil {
			(*f)()
		}
		return h.body(ctx, s, "w=")
	case arm844DetachPanic:
		if s.OnDetach == nil {
			return errors.New("celeris844: no OnDetach on the async stream")
		}
		s.OnDetach() // panics inside the window when injected, and the panic propagates
		h.detaches.Add(1)
		return h.body(ctx, s, "w=")
	case arm844Hold:
		id, _ := ctxkit.WorkerIDFrom(ctx)
		h.entered <- "w=" + strconv.Itoa(id)
		<-h.release
		if h.how == "goexit" {
			runtime.Goexit()
		}
		panic("celeris844: handler panic after hold")
	case arm844BadError:
		panic(badError844{})
	case arm844Headers:
		panic(s.Headers) // shares the stream's hdrBuf, which CloseH1 zeroes
	case arm844DetachRecovered:
		if s.OnDetach == nil {
			return errors.New("celeris844: no OnDetach on the async stream")
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					h.recovered.Add(1)
				}
			}()
			s.OnDetach()
			h.detaches.Add(1)
		}()
		// What the router does after recovering: answer, through the writeFn
		// OnDetach installed.
		return h.body(ctx, s, "r=")
	}
	return errors.New("celeris844: unknown mode " + h.mode)
}

func (h *h844) RouteAsync(_, path string) bool {
	if path != "/f" {
		return false
	}
	if h.mode == arm844ParkRoute && h.inject && h.parkArmed.CompareAndSwap(true, false) {
		h.routeFire.Add(1)
		panic("celeris844: RouteAsync panic at the park")
	}
	return true
}
func (*h844) HasAsyncRoutes() bool { return true }

var _ stream.AsyncRouteResolver = (*h844)(nil)

// badError844 is a panic value whose Error method panics: the abort formats the
// value while it still holds detachMu, so a second panic out of the formatting
// would skip the release and then end the process (a panic leaving a deferred
// function in a goroutine nothing recovers).
type badError844 struct{}

func (badError844) Error() string { panic("celeris844: the panic value's Error method panics") }

// refuse844 is a transplant target that takes nothing: a drain started with it
// makes the engine's park loop ask, and every hand-off is refused and reclaimed.
type refuse844 struct{ n atomic.Int64 }

func (r *refuse844) AdoptConn(int, engine.Carryover) error {
	r.n.Add(1)
	return errors.New("celeris844: refused")
}

// log844 counts the abnormal-exit records.
type log844 struct{ panics atomic.Int64 }

func (l *log844) Enabled(_ context.Context, lvl slog.Level) bool { return lvl >= slog.LevelError }
func (l *log844) Handle(_ context.Context, r slog.Record) error {
	if r.Message == abortPanicMsg791 {
		l.panics.Add(1)
	}
	return nil
}
func (l *log844) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *log844) WithGroup(string) slog.Handler      { return l }

// census844 is what the process holds: goroutines by stack, and descriptors.
type census844 struct {
	total, stuckAbort, dispatch, workerBlocked int
	fds                                        int
}

func take844() census844 {
	buf := make([]byte, 16<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var c census844
	for _, g := range strings.Split(string(buf), "\n\n") {
		c.total++
		if strings.Contains(g, ".abortAsyncHandler(") && strings.Contains(g, "sync.(*Mutex).Lock") {
			c.stuckAbort++
		}
		if strings.Contains(g, ".runAsyncHandler(") {
			c.dispatch++
		}
		if strings.Contains(g, "sync.(*Mutex).Lock") && strings.Contains(g, "/engine/"+pkgDir844+".") &&
			!strings.Contains(g, ".abortAsyncHandler(") {
			c.workerBlocked++
		}
	}
	if ents, err := os.ReadDir("/proc/self/fd"); err == nil {
		c.fds = len(ents)
	}
	return c
}

// since returns c less what the process held before this test's first dial:
// the goroutines of a test that failed earlier in the same process are still
// there (a wedged worker never exits), and are not this test's to count.
func (c census844) since(b census844) census844 {
	c.stuckAbort -= b.stuckAbort
	c.dispatch -= b.dispatch
	c.workerBlocked -= b.workerBlocked
	return c
}

type rig844 struct {
	t       *testing.T
	e       *Engine
	addr    string
	workers int
	cancel  context.CancelFunc
	done    chan error
	witness map[string]*abortConn791
	boom    map[string]*abortConn791
	ids     []string
	base    census844
	logs    *log844
}

// start844 starts an engine with AsyncHandlers on two workers and waits for it
// to listen.
func start844(t *testing.T, h stream.Handler, logger *slog.Logger) *rig844 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	r := &rig844{t: t, addr: addr, logs: &log844{}}
	if logger == nil {
		logger = slog.New(r.logs)
	}
	e, err := New(resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		Resources:     resource.Resources{Workers: 2},
		AsyncHandlers: true,
		Logger:        logger,
	}, h)
	if err != nil {
		unavailable844(t, engineName844+" engine unavailable: %v", err)
	}
	r.e = e
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan error, 1)
	go func() { r.done <- e.Listen(ctx) }()
	t.Cleanup(cancel)
	for deadline := time.Now().Add(8 * time.Second); ; {
		if c, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond); derr == nil {
			_ = c.Close()
			if e.NumWorkers() > 0 {
				break
			}
		}
		select {
		case err := <-r.done:
			unavailable844(t, engineName844+" engine failed to start: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("engine did not start listening within 8s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.workers = e.NumWorkers()
	return r
}

// place puts a witness and a boom conn on every worker, each proven to be
// served by it.
func (r *rig844) place() {
	t := r.t
	t.Helper()
	r.base = take844()
	r.witness, r.boom = map[string]*abortConn791{}, map[string]*abortConn791{}
	for range 64 {
		if len(r.witness) == r.workers && len(r.boom) == r.workers {
			break
		}
		a := abortDial791(t, r.addr)
		status, body, err := a.get("/ok")
		if err != nil || status != 200 || !strings.HasPrefix(body, "w=") {
			t.Fatalf("PREMISE: /ok before the fault: status %d body %q err %v", status, body, err)
		}
		switch {
		case r.witness[body] == nil:
			r.witness[body] = a
		case r.boom[body] == nil:
			r.boom[body] = a
		default:
			_ = a.c.Close()
		}
	}
	if len(r.witness) != r.workers || len(r.boom) != r.workers {
		t.Fatalf("PREMISE: 64 dials put a witness and a boom conn on %d and %d of %d workers",
			len(r.witness), len(r.boom), r.workers)
	}
	for id := range r.boom {
		r.ids = append(r.ids, id)
	}
	slices.Sort(r.ids)
}

// fault844 is what one boom conn did: the first answer, and what a second
// request on the same conn (a touch: the worker meeting the conn again) got.
type fault844 struct {
	id, first, touch string
}

// closedWithin reads until the conn ends or the budget runs out and reports
// how it ended: "closed", "timeout" or the error. Answers read on the way are
// returned in got.
func (a *abortConn791) closedWithin(d time.Duration) (got []string, end string) {
	_ = a.c.SetReadDeadline(time.Now().Add(d))
	for {
		line, err := a.br.ReadString('\n')
		if err != nil {
			return got, abortClass791(err)
		}
		if strings.HasPrefix(line, "HTTP/1.1 ") {
			got = append(got, strings.TrimSpace(line))
		}
	}
}

type judge844 struct {
	arm                string
	want               string // "closed" or "alive"
	faults             []fault844
	touchOut           []string
	witnessOK, freshOK int
	witnessOut         []string
	freshOut           map[string]int
	active             int64
	stopped            bool
	after, final       census844
}

// judge measures what the fault left, per the header comment.
func (r *rig844) judge(arm, want string, faults []fault844) *judge844 {
	j := &judge844{arm: arm, want: want, faults: faults, freshOut: map[string]int{}}
	// The touch: a second request on each boom conn. A closed conn answers
	// with nothing; a live one with a 200. On the base it also makes the
	// worker take the conn's asyncInMu in the feed path.
	for _, f := range faults {
		a := r.boom[f.id]
		if want == "alive" {
			status, _, err := a.get("/ok")
			out := abortClass791(err)
			if err == nil {
				out = "status=" + strconv.Itoa(status)
			}
			j.touchOut = append(j.touchOut, f.id+":"+out)
			continue
		}
		if want == "open" {
			// A detached conn is not an HTTP conn any more: all that can be
			// asked of it is that nobody closed it.
			got, end := a.closedWithin(300 * time.Millisecond)
			j.touchOut = append(j.touchOut, fmt.Sprintf("%s:%v/%s", f.id, got, end))
			continue
		}
		_, _ = fmt.Fprintf(a.c, "GET /ok HTTP/1.1\r\nHost: x\r\n\r\n")
		got, end := a.closedWithin(budget844)
		j.touchOut = append(j.touchOut, fmt.Sprintf("%s:%v/%s", f.id, got, end))
	}
	for _, id := range r.ids {
		status, body, err := r.witness[id].get("/ok")
		if err == nil && status == 200 && body == id {
			j.witnessOK++
			j.witnessOut = append(j.witnessOut, id+":ok")
		} else {
			j.witnessOut = append(j.witnessOut, fmt.Sprintf("%s:%s/%d/%q", id, abortClass791(err), status, body))
		}
	}
	for range fresh844 {
		c, err := net.DialTimeout("tcp", r.addr, budget844)
		if err != nil {
			j.freshOut["dial:"+abortClass791(err)]++
			continue
		}
		a := &abortConn791{c: c, br: bufio.NewReader(c)}
		status, body, err := a.get("/ok")
		_ = c.Close()
		if err == nil && status == 200 && strings.HasPrefix(body, "w=") {
			j.freshOK++
			j.freshOut[body]++
		} else {
			j.freshOut[abortClass791(err)]++
		}
	}
	wantActive := int64(r.workers)
	if want != "closed" {
		wantActive = int64(2 * r.workers)
	}
	for deadline := time.Now().Add(settle844); ; time.Sleep(20 * time.Millisecond) {
		j.active = r.e.Metrics().ActiveConnections
		j.after = take844().since(r.base)
		if (j.active == wantActive && j.after.stuckAbort == 0 && (want != "closed" || j.after.dispatch == 0)) ||
			time.Now().After(deadline) {
			break
		}
	}
	r.cancel()
	select {
	case <-r.done:
		j.stopped = true
	case <-time.After(5 * time.Second):
	}
	// Everything the test dialled goes, then what the process holds is
	// compared with the census before the first dial.
	for _, a := range r.boom {
		_ = a.c.Close()
	}
	for _, a := range r.witness {
		_ = a.c.Close()
	}
	for deadline := time.Now().Add(settle844); ; time.Sleep(20 * time.Millisecond) {
		j.final = take844().since(r.base)
		if j.final.dispatch == 0 || time.Now().After(deadline) {
			break
		}
	}
	return j
}

func (j *judge844) report(t *testing.T, r *rig844, h *h844) {
	t.Helper()
	t.Logf("celeris844 RESULT engine=%s tier=%s arm=%s inject=%v want=%s workers=%d hits=%d detaches=%d recovered=%d "+
		"route_fire=%d hook_fire=%d log_panics=%d first=%v touch=%v witness=%d/%d %v fresh=%d/%d %v active=%d "+
		"stuck_in_abort=%d dispatch_goroutines=%d blocked_engine_goroutines=%d goroutines=%d->%d->%d fds=%d->%d->%d stopped=%v",
		engineName844, tier844(r.e), j.arm, h.inject, j.want, r.workers, h.hits.Load(), h.detaches.Load(), h.recovered.Load(),
		h.routeFire.Load(), h.hookFire.Load(), r.logs.panics.Load(), j.faults, j.touchOut, j.witnessOK, r.workers, j.witnessOut,
		j.freshOK, fresh844, j.freshOut, j.active, j.after.stuckAbort, j.after.dispatch, j.after.workerBlocked,
		r.base.total, j.after.total, j.final.total, r.base.fds, j.after.fds, j.final.fds, j.stopped)
}

// expect844 turns the measurements into failures.
func (j *judge844) expect(t *testing.T, r *rig844, h *h844) {
	t.Helper()
	for _, f := range j.faults {
		if f.first == "timeout" && j.want == "closed" {
			t.Errorf("celeris#844: the %s fault on worker %s got no answer within %v", j.arm, f.id, budget844)
		}
	}
	if j.want == "closed" {
		for _, o := range j.touchOut {
			if !strings.HasSuffix(o, "/closed") {
				t.Errorf("celeris#844: after the %s fault, a boom conn was not torn down within %v: %s (the conn is "+
					"still owned by a dispatch goroutine that never exited, or a worker is blocked on its lock)",
					j.arm, budget844, o)
			}
		}
		if j.after.dispatch != 0 {
			t.Errorf("celeris#844: after the %s fault, %d dispatch goroutines were still alive after %v with every boom conn closed",
				j.arm, j.after.dispatch, settle844)
		}
	} else {
		for _, o := range j.touchOut {
			if j.want == "alive" && !strings.HasSuffix(o, ":status=200") {
				t.Errorf("CONTROL: with no fault injected a boom conn answered %s, want status=200", o)
			}
			if j.want == "open" && !strings.HasSuffix(o, "/timeout") {
				t.Errorf("CONTROL: with no fault injected a detached boom conn was closed: %s", o)
			}
		}
		if n := r.logs.panics.Load(); n != 0 {
			t.Errorf("CONTROL: %d recovered-panic records logged with no fault injected", n)
		}
	}
	if j.after.stuckAbort != 0 {
		t.Errorf("celeris#844: %d goroutines are blocked in abortAsyncHandler on a lock", j.after.stuckAbort)
	}
	if j.witnessOK != r.workers {
		t.Errorf("celeris#844: after the %s fault on every worker, %d of %d workers answered their witness conn within %v (%v): "+
			"the worker stopped serving", j.arm, j.witnessOK, r.workers, budget844, j.witnessOut)
	}
	if j.freshOK != fresh844 {
		t.Errorf("celeris#844: after the %s fault, %d of %d fresh conns were answered within %v (%v)",
			j.arm, j.freshOK, fresh844, budget844, j.freshOut)
	}
	wantActive := int64(r.workers)
	if j.want != "closed" {
		wantActive = int64(2 * r.workers)
	}
	if j.active != wantActive {
		t.Errorf("celeris#844: after the %s fault, ActiveConnections settled at %d within %v, want %d", j.arm, j.active, settle844, wantActive)
	}
	if !j.stopped {
		t.Errorf("celeris#844: Listen did not return within 5s of cancel after the %s fault", j.arm)
	}
	if j.final.dispatch != 0 {
		t.Errorf("celeris#844: %d dispatch goroutines outlived the engine and every conn", j.final.dispatch)
	}
}

// runArm844 injects the arm's fault once per worker, sequentially.
func runArm844(t *testing.T, arm string, inject bool, want string) {
	h := &h844{mode: arm, inject: inject}
	r := start844(t, h, nil)
	switch arm {
	case arm844ParkRoute:
		if ok, why := parkRouteReachable844(r.e); !ok {
			t.Skipf("celeris844 ABSENT engine=%s arm=%s: %s", engineName844, arm, why)
		}
	case arm844ParkHook:
		tgt := &refuse844{}
		start := func() { r.e.StartTransplant(tgt) }
		h.startDrain.Store(&start)
		t.Cleanup(r.e.StopTransplant)
		if inject {
			f := func() {
				if h.hookFire.Add(1) <= int64(r.workers) {
					panic("celeris844: engine fault under asyncInMu")
				}
			}
			parkWindowHook.Store(&f)
			t.Cleanup(func() { parkWindowHook.Store(nil) })
		}
	case arm844DetachPanic, arm844DetachRecovered:
		if inject {
			f := func() {
				if h.hookFire.Add(1) <= int64(r.workers) {
					panic("celeris844: engine fault in OnDetach")
				}
			}
			detachWindowHook.Store(&f)
			t.Cleanup(func() { detachWindowHook.Store(nil) })
		}
	}
	r.place()
	var faults []fault844
	for i, id := range r.ids {
		f := fault844{id: id}
		status, _, err := r.boom[id].get("/f")
		f.first = abortClass791(err)
		if err == nil {
			f.first = "status=" + strconv.Itoa(status)
		}
		faults = append(faults, f)
		// Wait for the fault to have run (the abort logs before it takes any
		// lock, on the base too) before the next one, so the faults never
		// overlap. The recovered arm logs nothing: its handler counts.
		if want != "closed" {
			continue
		}
		for deadline := time.Now().Add(budget844); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if arm == arm844DetachRecovered && h.recovered.Load() >= int64(i+1) {
				break
			}
			if arm != arm844DetachRecovered && r.logs.panics.Load() >= int64(i+1) {
				break
			}
		}
	}
	j := r.judge(arm, want, faults)
	j.report(t, r, h)
	if inject {
		if n := h.hits.Load(); n != int64(r.workers) {
			t.Errorf("INJECTION: /f ran %d times, want %d (one per worker)", n, r.workers)
		}
		switch arm {
		case arm844ParkRoute:
			if n := h.routeFire.Load(); n != int64(r.workers) {
				t.Errorf("INJECTION: RouteAsync panicked %d times at the park, want %d", n, r.workers)
			}
		case arm844BadError:
		default:
			if n := h.hookFire.Load(); n < int64(r.workers) {
				t.Errorf("INJECTION: the hook fired %d times, want at least %d", n, r.workers)
			}
		}
		if n := r.logs.panics.Load(); n != int64(r.workers) && arm != arm844DetachRecovered {
			t.Errorf("INJECTION: the engine logged %d recovered panics, want %d", n, r.workers)
		}
		if arm == arm844DetachRecovered {
			if n := h.recovered.Load(); n != int64(r.workers) {
				t.Errorf("INJECTION: the handler recovered %d panics, want %d", n, r.workers)
			}
		}
	}
	j.expect(t, r, h)
}

// Window 1.

func TestAbortParkRoutePanicHoldingAsyncInMu844(t *testing.T) {
	runArm844(t, arm844ParkRoute, true, "closed")
}
func TestAbortParkRouteControl844(t *testing.T) {
	runArm844(t, arm844ParkRoute, false, "alive")
}
func TestAbortParkEnginePanicHoldingAsyncInMu844(t *testing.T) {
	runArm844(t, arm844ParkHook, true, "closed")
}
func TestAbortParkEngineControl844(t *testing.T) {
	runArm844(t, arm844ParkHook, false, "alive")
}

// Window 2.

func TestAbortPanicInOnDetachWindow844(t *testing.T) {
	runArm844(t, arm844DetachPanic, true, "closed")
}
func TestAbortPanicInOnDetachWindowRecoveredByTheHandler844(t *testing.T) {
	runArm844(t, arm844DetachRecovered, true, "closed")
}

// A panic value whose Error method panics: the abort recovers the formatting,
// still releases the lock and tears the conn down.
func TestAbortPanicValueWhoseErrorPanics844(t *testing.T) {
	runArm844(t, arm844BadError, true, "closed")
}

func TestAbortOnDetachControl844(t *testing.T) {
	runArm844(t, arm844DetachPanic, false, "open")
}

// Window 3.

const canary844 = "c844-header-value-that-must-survive"

// gate844 is an application slog handler that blocks inside the abort's log
// call until the test lets it go, and only then formats the record: the
// worst case for a panic value that aliases memory the engine recycles. (An
// asynchronous handler formats later whatever the order.)
type gate844 struct {
	entered chan struct{}
	proceed chan struct{}
	once    sync.Once
	mu      sync.Mutex
	text    string
	done    atomic.Bool
}

func (g *gate844) release()                                     { g.once.Do(func() { close(g.proceed) }) }
func (g *gate844) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelError }
func (g *gate844) WithAttrs([]slog.Attr) slog.Handler           { return g }
func (g *gate844) WithGroup(string) slog.Handler                { return g }
func (g *gate844) Handle(_ context.Context, r slog.Record) error {
	if r.Message != abortPanicMsg791 {
		return nil
	}
	g.entered <- struct{}{}
	<-g.proceed
	var b strings.Builder
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "panic" {
			b.WriteString(a.Value.String())
		}
		return true
	})
	g.mu.Lock()
	g.text = b.String()
	g.mu.Unlock()
	g.done.Store(true)
	return nil
}

// TestAbortLogsAPanicValueThatSharesStreamMemory844: a stream.Handler used
// directly panics with s.Headers. The abort releases detachMu and calls the
// application's slog handler, which blocks; the peer's FIN reaches the engine
// meanwhile and closeConn, finding the lock free, runs CloseH1, which zeroes
// the stream's header storage. The handler then formats the record. The
// canary request header must still be in what it formats.
func TestAbortLogsAPanicValueThatSharesStreamMemory844(t *testing.T) {
	g := &gate844{entered: make(chan struct{}, 8), proceed: make(chan struct{})}
	t.Cleanup(g.release)
	h := &h844{mode: arm844Headers}
	r := start844(t, h, slog.New(g))
	r.place()
	id := r.ids[0]
	a := r.boom[id]
	if _, err := fmt.Fprintf(a.c, "GET /f HTTP/1.1\r\nHost: x\r\nX-Canary: %s\r\n\r\n", canary844); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-g.entered:
	case <-time.After(budget844):
		t.Fatalf("PREMISE: the abort never reached its log call within %v (hits=%d)", budget844, h.hits.Load())
	}
	// The abort is inside the log call. The peer goes away now.
	_ = a.c.Close()
	torn := false
	var active int64
	for deadline := time.Now().Add(settle844); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		active = r.e.Metrics().ActiveConnections
		if active == int64(2*r.workers-1) {
			torn = true
			break
		}
	}
	if !torn {
		t.Fatalf("PREMISE: while the abort was logging, the peer's FIN did not tear the conn down within %v "+
			"(ActiveConnections=%d, want %d): the interleaving did not occur", settle844, active, 2*r.workers-1)
	}
	g.release()
	for deadline := time.Now().Add(budget844); !g.done.Load() && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
	}
	g.mu.Lock()
	text := g.text
	g.mu.Unlock()
	t.Logf("celeris844 RESULT engine=%s tier=%s arm=headers-log hits=%d torn_down_during_log=%v formatted=%q",
		engineName844, tier844(r.e), h.hits.Load(), torn, text)
	if !g.done.Load() {
		t.Fatalf("the log call never returned after it was released")
	}
	if !strings.Contains(text, canary844) {
		t.Errorf("celeris#844 item 3: the slog handler formatted the panic value after the conn was torn down and got %q: "+
			"the request header %q was zeroed under it (CloseH1 released the stream while the abort still logged)", text, canary844)
	}
}

// Item 4: the engine's context is cancelled while the handlers run. Shutdown
// phase 1 takes each async conn's detachMu and so waits for the handler; the
// handler then panics or calls runtime.Goexit, and the abort must release the
// lock for shutdown to go on (celeris#840 handles this; nothing pinned it).
// The wait for "shutdown is blocked on the lock" is a condition on the
// stacks, not a sleep.

func shutdownBlocked844() (blocked int, stacks []string) {
	buf := make([]byte, 16<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "/engine/"+pkgDir844+".") && strings.Contains(g, ".shutdown(") &&
			strings.Contains(g, "sync.(*Mutex).Lock") {
			blocked++
			stacks = append(stacks, g)
		}
	}
	return blocked, stacks
}

func runCancelWhileHandlerRuns844(t *testing.T, how string) {
	h := &h844{mode: arm844Hold, how: how, entered: make(chan string, 16), release: make(chan struct{})}
	r := start844(t, h, nil)
	var once sync.Once
	rel := func() { once.Do(func() { close(h.release) }) }
	t.Cleanup(rel)
	r.place()
	blocked0, _ := shutdownBlocked844()
	for _, id := range r.ids {
		if _, err := fmt.Fprintf(r.boom[id].c, "GET /f HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			t.Fatalf("write /f: %v", err)
		}
	}
	for range r.ids {
		select {
		case <-h.entered:
		case <-time.After(budget844):
			t.Fatalf("PREMISE: only some of the %d /f handlers started within %v", len(r.ids), budget844)
		}
	}
	r.cancel()
	blocked, stacks := 0, []string(nil)
	for deadline := time.Now().Add(budget844); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if blocked, stacks = shutdownBlocked844(); blocked-blocked0 >= r.workers {
			break
		}
	}
	early := false
	select {
	case <-r.done:
		early = true
	default:
	}
	rel()
	stopped := early
	if !early {
		select {
		case <-r.done:
			stopped = true
		case <-time.After(settle844):
		}
	}
	after := take844().since(r.base)
	blocked -= blocked0
	t.Logf("celeris844 RESULT engine=%s tier=%s arm=cancel-%s workers=%d shutdown_blocked_on_detachMu=%d stopped_before_release=%v "+
		"stopped=%v log_panics=%d stuck_in_abort=%d dispatch_goroutines=%d goroutines=%d->%d",
		engineName844, tier844(r.e), how, r.workers, blocked, early, stopped, r.logs.panics.Load(),
		after.stuckAbort, after.dispatch, r.base.total, after.total)
	if early || blocked < r.workers {
		for _, g := range stacks {
			t.Logf("celeris844 shutdown stack\n%s", g)
		}
		t.Fatalf("PREMISE: with every handler running, shutdown was blocked on a lock in %d of %d workers (Listen returned early: %v)",
			blocked, r.workers, early)
	}
	if !stopped {
		t.Errorf("celeris#844: Listen did not return within %v after the handlers (%s) ended: shutdown stays blocked "+
			"on a detachMu the abort did not release", settle844, how)
	}
	if after.stuckAbort != 0 {
		t.Errorf("celeris#844: %d goroutines are blocked in abortAsyncHandler on a lock", after.stuckAbort)
	}
}

func TestShutdownWaitingOnAHandlerThatPanics844(t *testing.T) {
	runCancelWhileHandlerRuns844(t, "panic")
}
func TestShutdownWaitingOnAHandlerThatGoexits844(t *testing.T) {
	runCancelWhileHandlerRuns844(t, "goexit")
}
