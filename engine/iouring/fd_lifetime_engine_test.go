//go:build linux

package iouring

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#657 face 2 end to end: a real io_uring engine, keep-alive clients
// that never stop sending, and a hand-off across StartTransplant. On the base
// every hand-off leaves the connection's next recv armed, and some of those
// recvs take a request the client then waits on forever.

// startFDLEngine runs an io_uring engine on a free loopback port. Workers is
// 2 (the minimum Resources accepts); RLIMIT_MEMLOCK decides how many actually
// start (one at 8 MiB), and the count is logged as workers=N so a run can be
// checked against the shape it was registered for.
func startFDLEngine(t *testing.T, h stream.Handler, mut func(*resource.Config)) (*Engine, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg := resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mut != nil {
		mut(&cfg)
	}
	e, err := New(cfg, h)
	if err != nil {
		skipOrFail656(t, "iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("engine did not stop within 5s")
		}
	})
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
	t.Logf("celeris657 engine workers=%d", e.NumWorkers())
	return e, addr
}

// servingTarget is a stand-in epoll engine: every adopted descriptor is served
// as HTTP/1 keep-alive by a goroutine, so a client whose conn was handed off
// keeps getting answers — unless a recv left on the source took its request.
type servingTarget struct {
	adopted atomic.Int64
	onAdopt func() // runs first, on the source worker's thread
	wg      sync.WaitGroup
	mu      sync.Mutex
	conns   []net.Conn
}

func (s *servingTarget) AdoptConn(fd int, _ engine.Carryover) error {
	if s.onAdopt != nil {
		s.onAdopt()
	}
	f := os.NewFile(uintptr(fd), "adopted")
	c, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		return nil // fd closed above; nothing for the source to reclaim
	}
	s.adopted.Add(1)
	s.mu.Lock()
	s.conns = append(s.conns, c)
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		br := bufio.NewReader(c)
		for {
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, req.Body)
			_ = req.Body.Close()
			if _, err := c.Write([]byte("HTTP/1.1 200 OK\r\ncontent-type: text/plain\r\ncontent-length: 2\r\n\r\nok")); err != nil {
				return
			}
		}
	}()
	return nil
}

func (s *servingTarget) close() {
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// refusingTarget refuses every hand-off, so the source must keep (reclaim)
// each connection it offers.
type refusingTarget struct{ refused atomic.Int64 }

func (r *refusingTarget) AdoptConn(int, engine.Carryover) error {
	r.refused.Add(1)
	return errors.New("refusingTarget: no")
}

// loadResult is what the clients saw.
type loadResult struct {
	ok      int64
	errs    int64
	byClass map[string]int64
}

// runKeepAliveLoad drives n keep-alive clients against addr, each sending one
// request at a time with a 1 s read deadline, until stop is closed. A request
// the server never answers surfaces as that client's read timeout; the client
// then stops, so each lost request is exactly one error.
func runKeepAliveLoad(t *testing.T, addr string, n int, stop <-chan struct{}) func() loadResult {
	t.Helper()
	var ok, errsN atomic.Int64
	var mu sync.Mutex
	byClass := map[string]int64{}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			fail := func(err error) {
				errsN.Add(1)
				cls := "other"
				switch {
				case errors.Is(err, os.ErrDeadlineExceeded):
					cls = "read_timeout"
				case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
					cls = "eof"
				case errors.Is(err, syscall.ECONNRESET):
					cls = "reset"
				}
				mu.Lock()
				byClass[cls]++
				mu.Unlock()
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
					fail(err)
					return
				}
				_ = c.SetReadDeadline(time.Now().Add(time.Second))
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					fail(err)
					return
				}
				_, err = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					fail(err)
					return
				}
				ok.Add(1)
			}
		}()
	}
	return func() loadResult {
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		cp := map[string]int64{}
		for k, v := range byClass {
			cp[k] = v
		}
		return loadResult{ok: ok.Load(), errs: errsN.Load(), byClass: cp}
	}
}

func classes(m map[string]int64) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var b strings.Builder
	for _, k := range ks {
		b.WriteString(k + "=" + strconv.FormatInt(m[k], 10) + " ")
	}
	return strings.TrimSpace(b.String())
}

// transplantUnderLoad starts n clients, calls StartTransplant(tgt) after
// warm, keeps the load up for after, stops it and returns what the clients
// saw. The stale CQEs of any stolen request arrive within the load window;
// the final wait lets the last of them land before the counters are read.
func transplantUnderLoad(t *testing.T, e *Engine, addr string, n int, tgt engine.TransplantTarget,
	warm, after time.Duration,
) loadResult {
	t.Helper()
	stop := make(chan struct{})
	wait := runKeepAliveLoad(t, addr, n, stop)
	time.Sleep(warm)
	e.StartTransplant(tgt)
	time.Sleep(after)
	close(stop)
	res := wait()
	e.StopTransplant()
	time.Sleep(100 * time.Millisecond)
	t.Logf("celeris657 load conns=%d ok=%d errs=%d classes=[%s] W1T=%d W1U=%d W1C=%d W2=%d "+
		"held=%d reaps=%d misses=%d rescued=%d doubleclaim=%d detached=%d",
		n, res.ok, res.errs, classes(res.byClass),
		e.metrics.handoffLoss.staleRecvDataTransplanted.Load(),
		e.metrics.handoffLoss.staleRecvDataUnattributed.Load(),
		e.metrics.handoffLoss.staleRecvDataClosed.Load(),
		e.metrics.handoffLoss.handoffInFlight.Load(),
		metricOr(e, "TransplantHeld"), metricOr(e, "TransplantReaps"), metricOr(e, "TransplantReapMisses"),
		metricOr(e, "TransplantHoldRescued"), metricOr(e, "TransplantDoubleClaim"),
		e.metrics.transplantDetached.Load())
	return res
}

// metricOr is metric for a log line: -1 when the tree has no such field.
func metricOr(e *Engine, name string) int64 {
	v := reflect.ValueOf(e.Metrics()).FieldByName(name)
	if !v.IsValid() {
		return -1
	}
	return int64(v.Uint())
}

// TestHandoffHasNothingInFlight: at every hand-off, nothing may still be in
// flight on the connection (TransplantHandoffInFlight does not move), and no
// stale recv data may appear afterwards, with 128 busy keep-alives across a
// StartTransplant. Base: every hand-off is made with the conn's recv armed.
// The sync conns leave through tryTransplant (a held response's SEND
// completion, or a reap); the async ones are promoted to a dispatch goroutine
// that claims its own hand-off at its park, and leave through
// finishAsyncTransplant, which reaps the recv the feed path armed.
func TestHandoffHasNothingInFlight(t *testing.T) {
	for _, tc := range []struct {
		name  string
		async bool
	}{{"sync", false}, {"async", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var h stream.Handler = transplantTestHandler{}
			var mut func(*resource.Config)
			if tc.async {
				h = asyncRouteHandler{}
				mut = func(c *resource.Config) { c.AsyncHandlers = true }
			}
			e, addr := startFDLEngine(t, h, mut)
			var maxSeen atomic.Uint64
			tgt := &servingTarget{}
			tgt.onAdopt = func() {
				if v := e.metrics.handoffLoss.handoffInFlight.Load(); v > maxSeen.Load() {
					maxSeen.Store(v)
				}
			}
			defer tgt.close()
			const conns = 128
			res := transplantUnderLoad(t, e, addr, conns, tgt, 300*time.Millisecond, 1500*time.Millisecond)
			if tc.async {
				if n := e.Metrics().AsyncPromotedConns; n == 0 {
					t.Fatal("no conn was promoted to async dispatch: the async path was not exercised")
				}
			}
			if v := maxSeen.Load(); v != 0 {
				t.Errorf("TransplantHandoffInFlight read %d at a hand-off, want 0 at every one: a conn "+
					"left with an op still able to resolve its fd", v)
			}
			if tr, un := e.metrics.handoffLoss.staleRecvDataTransplanted.Load(),
				e.metrics.handoffLoss.staleRecvDataUnattributed.Load(); tr != 0 || un != 0 {
				t.Errorf("stale recv data after the hand-offs: Transplanted=%d Unattributed=%d, want 0/0", tr, un)
			}
			if res.errs != 0 {
				t.Errorf("clients saw %d errors (%s), want 0", res.errs, classes(res.byClass))
			}
			if got := tgt.adopted.Load(); got != conns {
				t.Errorf("%d of %d busy conns were handed off, want all", got, conns)
			}
		})
	}
}

// asyncRouteHandler is transplantTestHandler with every route async, so each
// conn is promoted to its own dispatch goroutine on its first request.
type asyncRouteHandler struct{ transplantTestHandler }

func (asyncRouteHandler) RouteAsync(_, _ string) bool { return true }
func (asyncRouteHandler) HasAsyncRoutes() bool        { return true }

// TestStaleRecvDataCounted is the celeris#657 join as a test: every request a
// client lost is a stale recv CQE with data that the witness counted as
// Transplanted or Unattributed — and with the fix there are none of either.
func TestStaleRecvDataCounted(t *testing.T) {
	e, addr := startFDLEngine(t, transplantTestHandler{}, nil)
	tgt := &servingTarget{}
	defer tgt.close()
	res := transplantUnderLoad(t, e, addr, 128, tgt, 300*time.Millisecond, 1500*time.Millisecond)
	w1 := int64(e.metrics.handoffLoss.staleRecvDataTransplanted.Load() +
		e.metrics.handoffLoss.staleRecvDataUnattributed.Load())
	if res.errs != w1 {
		t.Errorf("join broken: clients lost %d requests (%s) but the witness counted %d stale "+
			"data CQEs (Transplanted+Unattributed)", res.errs, classes(res.byClass), w1)
	}
	if res.errs != 0 || w1 != 0 {
		t.Errorf("lost requests = %d, stale data = %d, want 0 and 0", res.errs, w1)
	}
}

// TestHeldRecvIsReArmedWhenTheHandOffDoesNotHappen: a conn held for a
// hand-off that does not happen must be served on as if nothing had been
// attempted — (a) the target refuses every conn, (b) the drain stops while
// held responses are in flight, five times over. Clients see no error, no
// stale data appears, and no hold is left for the timeout sweep to rescue.
func TestHeldRecvIsReArmedWhenTheHandOffDoesNotHappen(t *testing.T) {
	check := func(t *testing.T, e *Engine, res loadResult) {
		t.Helper()
		if res.errs != 0 {
			t.Errorf("clients saw %d errors (%s), want 0", res.errs, classes(res.byClass))
		}
		if tr, un := e.metrics.handoffLoss.staleRecvDataTransplanted.Load(),
			e.metrics.handoffLoss.staleRecvDataUnattributed.Load(); tr != 0 || un != 0 {
			t.Errorf("stale recv data: Transplanted=%d Unattributed=%d, want 0/0", tr, un)
		}
		if n := e.metrics.handoffLoss.handoffInFlight.Load(); n != 0 {
			t.Errorf("TransplantHandoffInFlight = %d, want 0", n)
		}
		if n := metric(t, e, "TransplantHoldRescued"); n != 0 {
			t.Errorf("TransplantHoldRescued = %d, want 0: a held conn was stranded until the "+
				"timeout sweep found it", n)
		}
		if res.ok == 0 {
			t.Error("clients completed no requests")
		}
	}

	t.Run("target_refuses", func(t *testing.T) {
		e, addr := startFDLEngine(t, transplantTestHandler{}, nil)
		tgt := &refusingTarget{}
		res := transplantUnderLoad(t, e, addr, 64, tgt, 200*time.Millisecond, 1200*time.Millisecond)
		if tgt.refused.Load() == 0 {
			t.Fatal("no hand-off was attempted; the test exercised nothing")
		}
		check(t, e, res)
	})

	t.Run("drain_stops_while_held", func(t *testing.T) {
		e, addr := startFDLEngine(t, transplantTestHandler{}, nil)
		tgt := &refusingTarget{}
		stop := make(chan struct{})
		wait := runKeepAliveLoad(t, addr, 64, stop)
		time.Sleep(200 * time.Millisecond)
		for i := 0; i < 5; i++ {
			e.StartTransplant(tgt)
			time.Sleep(100 * time.Millisecond)
			e.StopTransplant()
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(1200 * time.Millisecond) // a stranded conn's 1 s read deadline expires in here
		close(stop)
		res := wait()
		t.Logf("celeris657 flaps=5 ok=%d errs=%d classes=[%s] refused=%d", res.ok, res.errs,
			classes(res.byClass), tgt.refused.Load())
		if tgt.refused.Load() == 0 {
			t.Fatal("no hand-off was attempted; the test exercised nothing")
		}
		check(t, e, res)
	})
}

// testHoldReleasedInTheWorkerLoop is TestHoldReleasedWhenDrainStops through
// the worker's own event loop (the inlined udSend dispatch site), made
// deterministic by a response large enough to keep its SEND in flight until
// the client reads it: the request arrives with a drain set (so the response
// is held), the drain stops, and only then does the SEND complete.
//
// The target refuses every conn. The warm-up response's SEND completion can
// reach the worker after the client has read it, and so after StartTransplant:
// the idle conn is then reaped and offered to the target before /big is sent.
// Refused, it is reclaimed onto io_uring and /big is served there as intended;
// an accepting target would own (and here close) the conn instead.
func testHoldReleasedInTheWorkerLoop(t *testing.T) {
	big := make([]byte, 3<<20)
	e, addr := startFDLEngine(t, bigBodyHandler{big: big}, nil)
	d := net.Dialer{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		_ = rc.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
		})
		return serr
	}}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	get := func(path string, timeout time.Duration) (int, error) {
		if _, err := c.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			return 0, err
		}
		_ = c.SetReadDeadline(time.Now().Add(timeout))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			return 0, err
		}
		n, err := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return int(n), err
	}
	if _, err := get("/", 2*time.Second); err != nil {
		t.Fatalf("warm-up request: %v", err)
	}
	tgt := &fdlTarget{refuse: true}
	e.StartTransplant(tgt)
	if _, err := c.Write([]byte("GET /big HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write /big: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // served and held; its SEND waits on this client
	if n := metric(t, e, "TransplantHeld"); n == 0 {
		t.Fatal("the /big response was not held: the test exercised nothing")
	}
	e.StopTransplant()
	state := func() string {
		m := e.Metrics()
		return "adopted=" + strconv.FormatInt(tgt.adopted.Load(), 10) +
			" detached=" + strconv.FormatUint(m.TransplantDetached, 10) +
			" refused=" + strconv.FormatUint(m.TransplantHandoffRefused, 10) +
			" held=" + strconv.FormatUint(metric(t, e, "TransplantHeld"), 10) +
			" closes=" + strconv.FormatUint(m.CloseCount, 10)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read /big: %v (%s)", err, state())
	}
	n, err := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil || int(n) != len(big) {
		t.Fatalf("read /big body: %d of %d bytes, err %v (%s)", n, len(big), err, state())
	}
	if _, err := get("/", 2*time.Second); err != nil {
		t.Fatalf("the request after a held response whose drain stopped got %v: the conn was "+
			"left with no recv armed", err)
	}
	if tgt.adopted.Load() != 0 {
		t.Fatalf("the refusing target adopted %d conns", tgt.adopted.Load())
	}
	if n := metric(t, e, "TransplantHoldRescued"); n != 0 {
		t.Fatalf("TransplantHoldRescued = %d, want 0: the release at the SEND completion did not run", n)
	}
}

type bigBodyHandler struct{ big []byte }

func (h bigBodyHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := []byte("ok")
	if s.Path == "/big" {
		body = h.big
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "application/octet-stream"}, {"content-length", strconv.Itoa(len(body))}}, body)
}

// TestWorkerParksWithNothingPending (A5): a worker that parks must first
// submit what its last iteration queued. The last connection's close queues
// the cancel of its header timer in the same iteration that finds the worker
// idle; on the base it parks with that SQE unsubmitted, and it stays so until
// something wakes the worker.
func TestWorkerParksWithNothingPending(t *testing.T) {
	e, addr := startFDLEngine(t, transplantTestHandler{}, func(c *resource.Config) {
		c.ReadHeaderTimeout = 10 * time.Second // a kernel timer in flight per conn
	})
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	br := bufio.NewReader(c)
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	_ = c.Close()

	e.mu.Lock()
	workers := append([]*Worker(nil), e.workers...)
	e.mu.Unlock()
	for deadline := time.Now().Add(5 * time.Second); ; {
		parked := 0
		for _, w := range workers {
			if w.suspended.Load() {
				parked++
			}
		}
		if parked == len(workers) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d workers parked within 5s", parked, len(workers))
		}
		time.Sleep(10 * time.Millisecond)
	}
	// suspended is stored after the park decision, so everything the worker
	// wrote before it — including the SQ ring's pending count — is visible
	// here, and a parked worker writes nothing.
	for i, w := range workers {
		if p := w.ring.Pending(); p != 0 {
			t.Errorf("worker %d parked with %d SQE(s) queued and never submitted", i, p)
		}
	}
}
