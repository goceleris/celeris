//go:build linux

package epoll_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/adaptive"
	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/epoll"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#844 through the adaptive engine, which runs epoll's dispatch goroutines
// unchanged. The fault runs on epoll: on the start engine for epoll, on the
// engine the switch made active for io_uring. Each arm faults ONE boom conn
// (the first worker's) and then judges the engine: the boom conn must be torn
// down, every other conn (a witness per worker, and the boom conns of the
// other workers) must still be answered, fresh conns must be answered, the
// goroutines of the faulted conn must be gone, and Listen must return.
//
// The same file is the epoll and the io_uring arm (the seams differ).

const (
	pkg844    = "epoll"
	budget844 = 2 * time.Second
	settle844 = 5 * time.Second
	fresh844  = 8
	panicMsg  = "async handler panicked"
	canary844 = "c844-header-value-that-must-survive"
)

type a844 struct {
	arm     string // "park", "detach", "headers"
	ae      *adaptive.Engine
	hits    atomic.Int64
	parked  atomic.Bool // park (io_uring): the next RouteAsync(/f) is the park loop's
	started atomic.Bool
	fired   atomic.Int64
}

func (h *a844) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.Path == "/f" {
		h.hits.Add(1)
		switch h.arm {
		case "headers":
			panic(s.Headers)
		case "detach":
			if s.OnDetach == nil {
				return errors.New("no OnDetach on the async stream")
			}
			s.OnDetach() // the armed hook panics inside the window
		case "park":
			if pkg844 == "iouring" {
				h.parked.Store(true) // RouteAsync's next /f call is canRevertToInline's
			} else if h.started.CompareAndSwap(false, true) {
				// epoll: the park-time fault needs a transplant drain, which the
				// adaptive engine starts on a switch. Start one, and return only
				// once the drain is set, so the goroutine's park asks.
				ep := h.ae.ActiveEngine().(*epoll.Engine)
				go h.ae.ForceSwitch()
				for dl := time.Now().Add(settle844); !ep.TransplantActive844() && time.Now().Before(dl); {
					time.Sleep(time.Millisecond)
				}
			}
		}
	}
	if s.ResponseWriter == nil {
		return nil
	}
	id, _ := ctxkit.WorkerIDFrom(ctx)
	body := "w=" + strconv.Itoa(id)
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(body))}}, []byte(body))
}

func (h *a844) RouteAsync(_, path string) bool {
	if path == "/f" && h.arm == "park" && pkg844 == "iouring" && h.parked.CompareAndSwap(true, false) {
		h.fired.Add(1)
		panic("celeris844: RouteAsync panic at the park")
	}
	return path == "/f"
}
func (*a844) HasAsyncRoutes() bool { return true }

var _ stream.AsyncRouteResolver = (*a844)(nil)

// gate: an application slog handler that blocks in the abort's log call and
// formats later, counting the abort records.
type log844 struct {
	panics  atomic.Int64
	gate    bool
	entered chan struct{}
	proceed chan struct{}
	once    sync.Once
	mu      sync.Mutex
	text    string
	done    atomic.Bool
}

func (l *log844) release()                                     { l.once.Do(func() { close(l.proceed) }) }
func (l *log844) Enabled(_ context.Context, v slog.Level) bool { return v >= slog.LevelError }
func (l *log844) WithAttrs([]slog.Attr) slog.Handler           { return l }
func (l *log844) WithGroup(string) slog.Handler                { return l }
func (l *log844) Handle(_ context.Context, r slog.Record) error {
	if r.Message != panicMsg {
		return nil
	}
	l.panics.Add(1)
	if !l.gate {
		return nil
	}
	l.entered <- struct{}{}
	<-l.proceed
	var b strings.Builder
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "panic" {
			b.WriteString(a.Value.String())
		}
		return true
	})
	l.mu.Lock()
	l.text = b.String()
	l.mu.Unlock()
	l.done.Store(true)
	return nil
}

type conn844 struct {
	c  net.Conn
	br *bufio.Reader
}

func dial844(t *testing.T, addr string) *conn844 {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, budget844)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &conn844{c: c, br: bufio.NewReader(c)}
}

func (a *conn844) get(path string) (int, string, error) {
	_ = a.c.SetDeadline(time.Now().Add(budget844))
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

func class844(err error) string {
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

type census844 struct{ total, stuck, dispatch, fds int }

func take844() census844 {
	buf := make([]byte, 16<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var c census844
	for _, g := range strings.Split(string(buf), "\n\n") {
		c.total++
		if strings.Contains(g, ".abortAsyncHandler(") && strings.Contains(g, "sync.(*Mutex).Lock") {
			c.stuck++
		}
		if strings.Contains(g, ".runAsyncHandler(") {
			c.dispatch++
		}
	}
	if ents, err := os.ReadDir("/proc/self/fd"); err == nil {
		c.fds = len(ents)
	}
	return c
}

// since returns c less what the process held before this test's first dial
// (the goroutines of a test that failed earlier in the process are not this
// test's to count).
func (c census844) since(b census844) census844 {
	c.stuck -= b.stuck
	c.dispatch -= b.dispatch
	return c
}

type rig844 struct {
	t       *testing.T
	ae      *adaptive.Engine
	addr    string
	workers int
	cancel  context.CancelFunc
	done    chan error
	witness map[string]*conn844
	boom    map[string]*conn844
	ids     []string
	base    census844
	logs    *log844
}

func start844(t *testing.T, h *a844, gate bool) *rig844 {
	t.Helper()
	r := &rig844{t: t, logs: &log844{gate: gate, entered: make(chan struct{}, 8), proceed: make(chan struct{})}}
	t.Cleanup(r.logs.release)
	cfg := resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, AsyncHandlers: true,
		Resources: resource.Resources{Workers: 2}, Logger: slog.New(r.logs)}
	ae, err := adaptive.New(cfg, h, nil)
	if err != nil {
		t.Skipf("celeris844 ABSENT engine=adaptive+%s: adaptive.New unsupported here: %v", pkg844, err)
	}
	ae.FreezeSwitching()
	h.ae, r.ae = ae, ae
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	r.done = make(chan error, 1)
	go func() { r.done <- ae.Listen(ctx) }()
	for dl := time.Now().Add(5 * time.Second); ae.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if ae.Addr() == nil {
		t.Fatal("adaptive engine never bound")
	}
	r.addr = ae.Addr().String()
	if pkg844 == "iouring" {
		ae.ForceSwitch()
		if ae.ActiveEngine().Type() != engine.IOUring {
			t.Skipf("celeris844 ABSENT engine=adaptive+iouring: the forced switch left %v active (io_uring unusable here)",
				ae.ActiveEngine().Type())
		}
	} else if ae.ActiveEngine().Type() != engine.Epoll {
		t.Skipf("celeris844 ABSENT engine=adaptive+epoll: the start engine is %v (CELERIS_ADAPTIVE_START)", ae.ActiveEngine().Type())
	}
	r.workers = ae.ActiveEngine().Metrics().Workers
	return r
}

func (r *rig844) place() {
	t := r.t
	t.Helper()
	r.base = take844()
	r.witness, r.boom = map[string]*conn844{}, map[string]*conn844{}
	for range 64 {
		if len(r.witness) == r.workers && len(r.boom) == r.workers {
			break
		}
		a := dial844(t, r.addr)
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
		t.Fatalf("PREMISE: 64 dials put a witness and a boom conn on %d and %d of %d workers", len(r.witness), len(r.boom), r.workers)
	}
	for id := range r.boom {
		r.ids = append(r.ids, id)
	}
	slices.Sort(r.ids)
}

// closedWithin reads until the conn ends or the budget runs out.
func (a *conn844) closedWithin(d time.Duration) (got []string, end string) {
	_ = a.c.SetReadDeadline(time.Now().Add(d))
	for {
		line, err := a.br.ReadString('\n')
		if err != nil {
			return got, class844(err)
		}
		if strings.HasPrefix(line, "HTTP/1.1 ") {
			got = append(got, strings.TrimSpace(line))
		}
	}
}

// judge faults nothing: it measures what the fault left.
func (r *rig844) judge(h *a844, first, touch string, inject bool) {
	t := r.t
	t.Helper()
	victim := r.ids[0]
	witnessOK, freshOK := 0, 0
	var witnessOut []string
	freshOut := map[string]int{}
	// A switch (the epoll park arm) moves the witnesses to the other engine,
	// whose worker ids differ; every other arm checks the witness is still
	// served by its own worker.
	strict := !(h.arm == "park" && pkg844 == "epoll")
	for _, id := range r.ids {
		status, body, err := r.witness[id].get("/ok")
		if err == nil && status == 200 && ((strict && body == id) || (!strict && strings.HasPrefix(body, "w="))) {
			witnessOK++
			witnessOut = append(witnessOut, id+":ok")
		} else {
			witnessOut = append(witnessOut, fmt.Sprintf("%s:%s/%d/%q", id, class844(err), status, body))
		}
	}
	for range fresh844 {
		c, err := net.DialTimeout("tcp", r.addr, budget844)
		if err != nil {
			freshOut["dial:"+class844(err)]++
			continue
		}
		a := &conn844{c: c, br: bufio.NewReader(c)}
		status, body, err := a.get("/ok")
		_ = c.Close()
		if err == nil && status == 200 && strings.HasPrefix(body, "w=") {
			freshOK++
			freshOut[body]++
		} else {
			freshOut[class844(err)]++
		}
	}
	// Everything but the victim is alive: workers witnesses + (workers-1) boom conns.
	wantActive := int64(2*r.workers - 1)
	var active int64
	var after census844
	for dl := time.Now().Add(settle844); ; time.Sleep(20 * time.Millisecond) {
		active = r.ae.Metrics().ActiveConnections
		after = take844().since(r.base)
		if (active == wantActive && after.stuck == 0 && after.dispatch == 0) || time.Now().After(dl) {
			break
		}
	}
	r.cancel()
	stopped := false
	select {
	case <-r.done:
		stopped = true
	case <-time.After(15 * time.Second):
	}
	t.Logf("celeris844 RESULT engine=adaptive+%s tier=%s arm=%s inject=%v victim=%s hits=%d fired=%d log_panics=%d first=%s touch=%s "+
		"witness=%d/%d %v fresh=%d/%d %v active=%d/%d stuck_in_abort=%d dispatch_goroutines=%d goroutines=%d->%d fds=%d->%d stopped=%v",
		pkg844, tierOf(r.ae), h.arm, inject, victim, h.hits.Load(), h.fired.Load(), r.logs.panics.Load(), first, touch,
		witnessOK, r.workers, witnessOut, freshOK, fresh844, freshOut, active, wantActive, after.stuck, after.dispatch,
		r.base.total, after.total, r.base.fds, after.fds, stopped)
	if !strings.HasSuffix(touch, "/closed") {
		t.Errorf("celeris#844: after the %s fault the victim conn was not torn down within %v: %s", h.arm, budget844, touch)
	}
	if witnessOK != r.workers {
		t.Errorf("celeris#844: %d of %d witness conns answered after the %s fault (%v)", witnessOK, r.workers, h.arm, witnessOut)
	}
	if freshOK != fresh844 {
		t.Errorf("celeris#844: %d of %d fresh conns answered after the %s fault (%v)", freshOK, fresh844, h.arm, freshOut)
	}
	if after.stuck != 0 {
		t.Errorf("celeris#844: %d goroutines blocked in abortAsyncHandler on a lock", after.stuck)
	}
	if active != wantActive {
		t.Errorf("celeris#844: ActiveConnections settled at %d, want %d", active, wantActive)
	}
	if !stopped {
		t.Errorf("celeris#844: Listen did not return within 15s of cancel after the %s fault", h.arm)
	}
}

func tierOf(ae *adaptive.Engine) string {
	switch e := ae.ActiveEngine().(type) {
	case interface{ TierName844() string }:
		return e.TierName844()
	}
	return "?"
}

func runAdaptive844(t *testing.T, arm string, inject bool) {
	h := &a844{arm: arm}
	r := start844(t, h, false)
	switch arm {
	case "detach":
		if inject {
			var n atomic.Int64
			epoll.SetDetachWindowHook844(func() {
				if n.Add(1) == 1 {
					h.fired.Add(1)
					panic("celeris844: engine fault in OnDetach")
				}
			})
			t.Cleanup(func() { epoll.SetDetachWindowHook844(nil) })
		}
	case "park":
		if pkg844 == "epoll" && inject {
			var n atomic.Int64
			epoll.SetParkWindowHook844(func() {
				if n.Add(1) == 1 {
					h.fired.Add(1)
					panic("celeris844: engine fault under asyncInMu")
				}
			})
			t.Cleanup(func() { epoll.SetParkWindowHook844(nil) })
		}
	}
	r.place()
	if arm == "park" && pkg844 == "iouring" {
		if tier := tierOf(r.ae); tier != "base" {
			t.Skipf("celeris844 ABSENT engine=adaptive+iouring arm=park: tier %s: canRevertToInline never calls RouteAsync with a buffer ring", tier)
		}
	}
	victim := r.ids[0]
	status, _, err := r.boom[victim].get("/f")
	first := class844(err)
	if err == nil {
		first = "status=" + strconv.Itoa(status)
	}
	for dl := time.Now().Add(budget844); r.logs.panics.Load() < 1 && time.Now().Before(dl); time.Sleep(5 * time.Millisecond) {
	}
	_, _ = fmt.Fprintf(r.boom[victim].c, "GET /ok HTTP/1.1\r\nHost: x\r\n\r\n")
	got, end := r.boom[victim].closedWithin(budget844)
	touch := fmt.Sprintf("%s:%v/%s", victim, got, end)
	if inject && h.fired.Load() == 0 {
		t.Errorf("INJECTION: the fault never fired (hits=%d)", h.hits.Load())
	}
	r.judge(h, first, touch, inject)
}

func TestAdaptiveAbortParkWindow844(t *testing.T)   { runAdaptive844(t, "park", true) }
func TestAdaptiveAbortDetachWindow844(t *testing.T) { runAdaptive844(t, "detach", true) }

// Window 3 through the adaptive engine: the peer's FIN tears the conn down
// while the abort is inside the application's slog handler.
func TestAdaptiveAbortLogsAPanicValueThatSharesStreamMemory844(t *testing.T) {
	h := &a844{arm: "headers"}
	r := start844(t, h, true)
	r.place()
	victim := r.ids[0]
	a := r.boom[victim]
	if _, err := fmt.Fprintf(a.c, "GET /f HTTP/1.1\r\nHost: x\r\nX-Canary: %s\r\n\r\n", canary844); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-r.logs.entered:
	case <-time.After(budget844):
		t.Fatalf("PREMISE: the abort never reached its log call within %v (hits=%d)", budget844, h.hits.Load())
	}
	_ = a.c.Close()
	torn := false
	var active int64
	want := int64(2*r.workers - 1)
	for dl := time.Now().Add(settle844); time.Now().Before(dl); time.Sleep(10 * time.Millisecond) {
		if active = r.ae.Metrics().ActiveConnections; active == want {
			torn = true
			break
		}
	}
	if !torn {
		t.Fatalf("PREMISE: while the abort was logging, the peer's FIN did not tear the conn down within %v (ActiveConnections=%d, want %d)",
			settle844, active, want)
	}
	r.logs.release()
	for dl := time.Now().Add(budget844); !r.logs.done.Load() && time.Now().Before(dl); time.Sleep(5 * time.Millisecond) {
	}
	r.logs.mu.Lock()
	text := r.logs.text
	r.logs.mu.Unlock()
	t.Logf("celeris844 RESULT engine=adaptive+%s arm=headers-log torn_down_during_log=%v formatted=%q", pkg844, torn, text)
	if !strings.Contains(text, canary844) {
		t.Errorf("celeris#844 item 3: the slog handler formatted the panic value after the conn was torn down and got %q", text)
	}
}
