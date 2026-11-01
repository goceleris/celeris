//go:build linux

package celeris_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#1208 (found by the review of #1207): two shapes of a connection at the
// start of Shutdown that the godoc used to get wrong, pinned here per engine.
// The tests characterise what the engines do; a change to one of them fails
// here and has to change the Shutdown godoc with it.
//
//	shape A, a connection that has sent nothing when Shutdown begins
//	  (godoc: "it is answered close when its first request is"; it is not, but
//	  not "never answered" either: see A-accepted-draining):
//	    A-accepted  the engine has accepted it. natives: FIN at once, a request
//	                it sends later is not answered. std: no FIN (net/http closes
//	                a StateNew conn only at 5 s), its first request is dropped.
//	    A-deferred  natives only, TCP_DEFER_ACCEPT (the default): the kernel has
//	                not handed it to the engine. No FIN; its first request gets
//	                a reset.
//	    A-accepted-draining, A-deferred-draining
//	                the same with a response going out on every worker (16
//	                clients that do not read an 8 MiB response). epoll: as above
//	                (A-deferred-draining is io_uring only). io_uring serves on for
//	                the first 250 ms of its send drain: idle and silent conns get
//	                their FIN at about 250 ms, and a first request 50 ms in is
//	                answered, on an accepted conn and on a deferred one.
//	shape B, a request head in the kernel that the server has not read when
//	  Shutdown begins:
//	    B-busy-loop   natives: the loop is held by a handler while the head
//	                  arrives on a conn it was reading. answered, with
//	                  Connection: close.
//	    B-hold-other  natives: the loop is held in another conn's OnConnect
//	                  while 24 conns it was reading send their heads. epoll,
//	                  adaptive (starts on epoll): answered, close; io_uring: the
//	                  held worker has nothing to send and exits at once, those
//	                  heads are dropped.
//	    B-accept-hold OnConnect holds the conn before the engine reads it.
//	                  std (net/http parses the head after Shutdown began):
//	                  GET closed without an answer, with a body reset;
//	                  epoll, adaptive (starts on epoll): answered, close;
//	                  io_uring: closed without an answer.
//	    B-accept-hold-draining
//	                  the same on epoll and io_uring with a response going out on
//	                  every worker: answered, close, on both.
//
// Each cell is a few hundred ms. "The shutdown has begun" is observed, never
// slept for: an idle keep-alive conn's FIN (natives) or the listener's close
// (std).

type outcome1208 struct {
	kind  string // answered, reset, closed-without-answer, no-reply
	close bool
	body  string
	err   error
}

func (o outcome1208) String() string {
	return fmt.Sprintf("%s (close=%v body=%q err=%v)", o.kind, o.close, o.body, o.err)
}

func (o outcome1208) answered() bool { return o.kind == "answered" }

func classify1208(br *bufio.Reader, werr error) outcome1208 {
	o := outcome1208{err: werr}
	resp, err := http.ReadResponse(br, nil)
	switch {
	case err == nil:
		b, _ := io.ReadAll(resp.Body)
		o.kind, o.close, o.body = "answered", resp.Close, string(b)
	case errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(werr, syscall.ECONNRESET) || errors.Is(werr, syscall.EPIPE):
		o.kind, o.err = "reset", err
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		o.kind, o.err = "closed-without-answer", err
	default:
		o.kind, o.err = "no-reply", err
	}
	return o
}

// send1208 writes a request (and a body of n zero bytes) and classifies what
// comes back.
func send1208(c net.Conn, req string, n int) outcome1208 {
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, werr := io.WriteString(c, req)
	if werr == nil && n > 0 {
		_, werr = c.Write(make([]byte, n))
	}
	return classify1208(bufio.NewReader(c), werr)
}

const (
	get1208      = "GET /ping HTTP/1.1\r\nHost: x\r\n\r\n"
	postBody1208 = 64 << 10
)

var post1208 = "POST /up HTTP/1.1\r\nHost: x\r\nContent-Length: " + strconv.Itoa(postBody1208) + "\r\n\r\n"

var engines1208 = []struct {
	name   string
	eng    celeris.EngineType
	native bool
}{
	{"std", celeris.Std, false},
	{"epoll", celeris.Epoll, true},
	{"io_uring", celeris.IOUring, true},
	{"adaptive", celeris.Adaptive, true},
}

func routes1208(s *celeris.Server) {
	s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
	s.POST("/up", func(c *celeris.Context) error { return c.String(http.StatusOK, "got %d", len(c.Body())) })
}

type rig1208 struct {
	t        *testing.T
	s        *celeris.Server
	addr     string
	done     chan error
	shutDone chan error
}

func startRig1208(t *testing.T, cfg celeris.Config, routes func(*celeris.Server)) *rig1208 {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg.Workers, cfg.ShutdownTimeout = 2, 15*time.Second
	s, addr, done := startPickedCtx1104(ctx, t, cfg, routes,
		func(_ *celeris.Server, addr string, done <-chan error) error { return waitDrainOrderReady(addr, done) })
	return &rig1208{t: t, s: s, addr: addr, done: done, shutDone: make(chan error, 1)}
}

func (r *rig1208) dial() net.Conn {
	r.t.Helper()
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = c.Close() })
	return c
}

// waitConns returns once the engine counts n more conns than base.
func (r *rig1208) waitConns(base int64, n int) {
	r.t.Helper()
	for dl := time.Now().Add(10 * time.Second); r.s.EngineInfo().Metrics.ActiveConnections < base+int64(n); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(dl) {
			r.t.Fatalf("the engine counts %d conns, want %d", r.s.EngineInfo().Metrics.ActiveConnections, base+int64(n))
		}
	}
}

func (r *rig1208) shutdown() {
	go func() {
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		r.shutDone <- r.s.Shutdown(sctx)
	}()
}

// finish closes the clients and waits for Shutdown and Start to return.
func (r *rig1208) finish(conns ...net.Conn) {
	r.t.Helper()
	for _, c := range conns {
		_ = c.Close()
	}
	select {
	case err := <-r.shutDone:
		if err != nil {
			r.t.Errorf("Shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		r.t.Error("Shutdown did not return")
	}
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		r.t.Error("the server did not stop")
	}
}

// waitListenerClosed returns once a new connect to addr is refused: std closes
// its listener first thing in Shutdown.
func waitListenerClosed1208(t *testing.T, addr string) {
	t.Helper()
	for dl := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return
		}
		_ = c.Close()
		if time.Now().After(dl) {
			t.Fatal("the listener is still open 5 s into the shutdown")
		}
	}
}

// waitFIN1208 reads c until it fails and returns how: nil error means EOF.
func waitFIN1208(c net.Conn, within time.Duration) error {
	_ = c.SetReadDeadline(time.Now().Add(within))
	_, err := c.Read(make([]byte, 1))
	if err == io.EOF {
		return nil
	}
	return err
}

func isTimeout1208(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// TestShutdownSilentConn_1208_ShapeA pins what Shutdown does to a connection
// that has sent nothing, per engine.
func TestShutdownSilentConn_1208_ShapeA(t *testing.T) {
	for _, ec := range engines1208 {
		t.Run(ec.name, func(t *testing.T) {
			t.Parallel()
			t.Run("A-accepted", func(t *testing.T) { t.Parallel(); silentAccepted1208(t, ec.eng, ec.native) })
			if ec.native {
				t.Run("A-deferred", func(t *testing.T) { t.Parallel(); silentDeferred1208(t, ec.eng) })
			}
			if ec.native && ec.eng != celeris.Adaptive { // adaptive starts on epoll: the same cells
				t.Run("A-accepted-draining", func(t *testing.T) { t.Parallel(); silentAcceptedDraining1208(t, ec.eng) })
			}
			if ec.eng == celeris.IOUring {
				// Not epoll: its loops close their listeners at their own pace
				// while they send, so a first request that arrives in the first
				// ms is answered, not reset.
				t.Run("A-deferred-draining", func(t *testing.T) { t.Parallel(); silentDeferredDraining1208(t, ec.eng) })
			}
		})
	}
}

func silentAccepted1208(t *testing.T, eng celeris.EngineType, native bool) {
	// DisableDeferAccept: the engine accepts a silent conn at its connect (by
	// default the kernel holds it back for about a second: A-deferred).
	r := startRig1208(t, celeris.Config{Engine: eng, DisableDeferAccept: true}, routes1208)
	base := settledEngineMetrics1104Server(r.s)
	never, getter, poster := r.dial(), r.dial(), r.dial()
	r.waitConns(base, 3)
	r.shutdown()
	if native {
		// Each conn the engine closes at the start of the shutdown gets its FIN
		// at once: write the request after it, and it is not answered.
		for name, c := range map[string]net.Conn{"never": never, "getter": getter, "poster": poster} {
			if err := waitFIN1208(c, 2*time.Second); err != nil {
				t.Errorf("%s: a conn that sent nothing had no FIN 2 s into the shutdown: %v", name, err)
			}
		}
	} else {
		waitListenerClosed1208(t, r.addr)
		// net/http closes a conn still in StateNew only when it is 5 s old, or
		// when its first request is read: the silent conn holds Shutdown.
		if err := waitFIN1208(never, 300*time.Millisecond); !isTimeout1208(err) {
			t.Errorf("std: the silent conn read %v 300 ms into the shutdown, want it held open", err)
		}
		select {
		case err := <-r.shutDone:
			t.Errorf("std: Shutdown returned (%v) with a silent conn open", err)
			r.shutDone <- err
		default:
		}
	}
	if o := send1208(getter, get1208, 0); o.answered() || (!native && o.kind != "closed-without-answer") {
		t.Errorf("a GET on a conn that sent nothing when the shutdown began: %v, want it dropped (closed without an answer)", o)
	}
	if o := send1208(poster, post1208, postBody1208); o.answered() {
		t.Errorf("a POST on a conn that sent nothing when the shutdown began: %v, want it dropped", o)
	}
	r.finish(never, getter, poster)
}

func silentDeferred1208(t *testing.T, eng celeris.EngineType) {
	r := startRig1208(t, celeris.Config{Engine: eng}, routes1208)
	// An accepted idle conn: its FIN says the shutdown has begun.
	k := r.dial()
	if o := send1208(k, get1208, 0); !o.answered() || o.close {
		t.Fatalf("the keep-alive conn before the shutdown: %v", o)
	}
	d := r.dial() // TCP_DEFER_ACCEPT keeps it out of the engine for ~1 s
	dialed := time.Now()
	r.shutdown()
	if err := waitFIN1208(k, 2*time.Second); err != nil {
		t.Fatalf("the idle keep-alive conn had no FIN 2 s into the shutdown: %v", err)
	}
	err := waitFIN1208(d, 300*time.Millisecond)
	inWindow := time.Since(dialed) < 900*time.Millisecond
	if inWindow && !isTimeout1208(err) {
		t.Errorf("the conn the kernel has not handed over read %v after the shutdown began, want no FIN", err)
	}
	o := send1208(d, get1208, 0)
	if o.answered() || (inWindow && o.kind != "reset") {
		t.Errorf("a GET on a conn the kernel had not handed over when the shutdown began: %v, want a reset", o)
	}
	r.finish(k, d)
}

// TestShutdownKernelHead_1208_ShapeB pins what Shutdown does to a request head
// that is in the kernel, unread by the server, when it begins, per engine.
func TestShutdownKernelHead_1208_ShapeB(t *testing.T) {
	for _, ec := range engines1208 {
		t.Run(ec.name, func(t *testing.T) {
			t.Parallel()
			for _, post := range []bool{false, true} {
				name := "B-accept-hold/GET"
				if post {
					name = "B-accept-hold/POST"
				}
				t.Run(name, func(t *testing.T) {
					kernelHeadAcceptHold1208(t, ec.eng, ec.native, post, false, wantAcceptHold1208(ec.eng, post))
				})
			}
			if ec.native && ec.eng != celeris.Adaptive {
				t.Run("B-accept-hold-draining", func(t *testing.T) { t.Parallel(); kernelHeadAcceptHold1208(t, ec.eng, true, false, true, "answered") })
			}
			if ec.native {
				t.Run("B-busy-loop", func(t *testing.T) { t.Parallel(); kernelHeadHeldLoop1208(t, ec.eng, "busy-loop", "answered") })
				t.Run("B-hold-other", func(t *testing.T) {
					t.Parallel()
					kernelHeadHeldLoop1208(t, ec.eng, "hold-other", wantHoldOther1208(ec.eng))
				})
			}
		})
	}
}

// wantAcceptHold1208 is what a conn whose accept the server has not finished,
// with its head in the kernel, gets when Shutdown begins (measured):
//
//   - std: net/http parses the head after Shutdown began and drops it; the body
//     of a POST left unread makes the close a reset;
//   - epoll (and adaptive, which starts on epoll): the gate asks the kernel for
//     the receive queue (TIOCINQ) and reads the head on: answered, with close;
//   - io_uring: closed without an answer, GET or POST (a FIN, not an RST). The
//     worker has no send pending and no parsed request in progress when it first
//     looks at the shutdown, so it exits at once (worker.go, the send-drain
//     branch), and h1RequestInProgress does not look at the kernel, as epoll's
//     h1InProgress does: the recv it had prepared after OnConnect is reaped
//     without being served. A worker with a send pending, or a request in
//     progress, stays in its drain and answers it (B-hold-other-draining).
func wantAcceptHold1208(eng celeris.EngineType, post bool) string {
	switch eng {
	case celeris.Epoll, celeris.Adaptive:
		return "answered"
	case celeris.IOUring:
		return "closed-without-answer"
	}
	if post {
		return "reset"
	}
	return "closed-without-answer"
}

// kernelHeadAcceptHold1208: OnConnect runs before the engine reads the conn
// (in net/http's accept loop on std, on the event-loop thread on the native
// engines). Held there, the head the client writes stays in the kernel until
// after Shutdown began.
func kernelHeadAcceptHold1208(t *testing.T, eng celeris.EngineType, native, post, draining bool, want string) {
	var armed atomic.Bool
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	rel := func() { once.Do(func() { close(release) }) }
	t.Cleanup(rel)
	r := startRig1208(t, celeris.Config{Engine: eng, DisableDeferAccept: true, OnConnect: func(string) {
		if armed.CompareAndSwap(true, false) {
			close(held)
			<-release
		}
	}}, routesBig1208)
	stop := func() {}
	if draining {
		stop = r.stallers(t, 16)
	}
	// Natives: idle keep-alive conns, served before the shutdown; the ones on the
	// loop OnConnect does not hold get their FIN when the shutdown begins.
	var wg sync.WaitGroup
	var begunOnce sync.Once
	begun := make(chan struct{})
	if native {
		for range 12 {
			c := r.dial()
			if o := send1208(c, get1208, 0); !o.answered() || o.close {
				t.Fatalf("a keep-alive conn before the shutdown: %v", o)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if waitFIN1208(c, 10*time.Second) == nil {
					begunOnce.Do(func() { close(begun) })
				}
			}()
		}
	}
	settledEngineMetrics1104Server(r.s)
	armed.Store(true)
	c := r.dial()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("OnConnect was not reached")
	}
	req, n := get1208, 0
	if post {
		req, n = post1208, postBody1208
	}
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	if n > 0 {
		if _, err := c.Write(make([]byte, n)); err != nil {
			t.Fatal(err)
		}
	}
	r.shutdown()
	if native {
		select {
		case <-begun:
		case <-time.After(3 * time.Second): // every idle conn was on the held loop (p = 2^-12)
		}
	} else {
		waitListenerClosed1208(t, r.addr)
	}
	rel()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	o := classify1208(bufio.NewReader(c), nil)
	if o.kind != want || (want == "answered" && (!o.close || o.body != map[bool]string{false: "ok", true: "got " + strconv.Itoa(postBody1208)}[post])) {
		t.Errorf("%v, a head in the kernel when the shutdown began (post=%v): %v, want %s", eng, post, o, want)
	}
	stop() // Shutdown waits for the stalled responses
	r.finish(c)
	wg.Wait()
}

// wantHoldOther1208 is what B-hold-other gets (measured): epoll answers, io_uring
// drops. The loop is held in another conn's OnConnect, so on io_uring the worker
// has no send pending and no request parsed when it first looks at the shutdown,
// and exits (see wantAcceptHold1208).
func wantHoldOther1208(eng celeris.EngineType) string {
	if eng == celeris.IOUring {
		return "dropped"
	}
	return "answered"
}

// kernelHeadHeldLoop1208: epoll, io_uring and adaptive. One loop is held while
// 24 conns the engine is already reading send their heads: those on the held
// loop have their head in the kernel (epoll) or unreaped (io_uring) when Shutdown
// begins, those on the other loop are served at once and then get their FIN
// (which says the shutdown has begun). Every one of them is answered; one the
// shutdown found unread is answered with Connection: close.
//
//   - "busy-loop": a handler holds the loop. The blocked handler's response is a
//     send pending, which keeps an io_uring worker in its drain whatever the
//     conns hold.
//   - "hold-other": the OnConnect of another, silent conn holds the loop. No send
//     is pending and no request is in progress, so the io_uring worker exits at
//     once on its first look at the shutdown, with the heads still in the kernel
//     (celeris#1208; #1104's contract is "a request that reached the kernel
//     before the gate is answered").
//
// The conns are spread by the kernel over the loops (p = 2^-23 that none is on
// the held one): that is a failure, not a skip, which CI's tally would fail
// anyway.
func kernelHeadHeldLoop1208(t *testing.T, eng celeris.EngineType, how, want string) {
	const n = 24
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	rel := func() { once.Do(func() { close(release) }) }
	t.Cleanup(rel)
	var armed atomic.Bool
	cfg := celeris.Config{Engine: eng, DisableDeferAccept: true}
	if how == "hold-other" {
		cfg.OnConnect = func(string) {
			if armed.CompareAndSwap(true, false) {
				started <- struct{}{}
				<-release
			}
		}
	}
	r := startRig1208(t, cfg, func(s *celeris.Server) {
		routes1208(s)
		s.GET("/block", func(c *celeris.Context) error {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return c.String(http.StatusOK, "blocked")
		})
	})
	base := settledEngineMetrics1104Server(r.s)
	conns := make([]net.Conn, n)
	brs := make([]*bufio.Reader, n)
	for i := range conns {
		conns[i] = r.dial()
		brs[i] = bufio.NewReader(conns[i])
	}
	r.waitConns(base, n)
	if how == "hold-other" {
		// The loop reads these conns already: each has been served once.
		for i, c := range conns {
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			_, _ = io.WriteString(c, get1208)
			if o := classify1208(brs[i], nil); !o.answered() || o.close {
				t.Fatalf("conn %d before the shutdown: %v", i, o)
			}
		}
		settledEngineMetrics1104Server(r.s)
	}
	var holder net.Conn
	switch how {
	case "busy-loop":
		holder = r.dial()
		if _, err := io.WriteString(holder, "GET /block HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
	case "hold-other":
		armed.Store(true)
		holder = r.dial() // silent: held in OnConnect, on one of the loops
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop was not held")
	}
	for i, c := range conns {
		req, body := get1208, 0
		if i%2 == 1 {
			req, body = post1208, postBody1208
		}
		_, _ = io.WriteString(c, req)
		if body > 0 {
			_, _ = c.Write(make([]byte, body))
		}
	}
	var (
		wg         sync.WaitGroup
		servedOnce sync.Once
		served     = make(chan struct{}) // a conn of the free loop was served
		begunOnce  sync.Once
		begun      = make(chan struct{}) // ... and then got its FIN: the shutdown has begun
		res        = make([]outcome1208, n)
	)
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
			br := brs[i]
			res[i] = classify1208(br, nil)
			if res[i].answered() && !res[i].close {
				servedOnce.Do(func() { close(served) })
				if _, err := br.ReadByte(); err == io.EOF {
					begunOnce.Do(func() { close(begun) })
				}
			}
		}()
	}
	select {
	case <-served:
	case <-time.After(3 * time.Second): // every conn landed on the held loop (p = 2^-23)
	}
	r.shutdown()
	select {
	case <-begun:
	case <-time.After(3 * time.Second):
	}
	rel()
	wg.Wait()
	// A conn answered with close is either one the shutdown found unread or one
	// of the loop that was not held whose request was still arriving when it
	// began (a loaded machine): both are answered. Dropped conns are the held
	// loop's, with the heads unread, and are what "dropped" counts.
	unread, dropped := 0, 0
	for i, o := range res {
		if o.answered() && !o.close {
			continue // served by the loop that was not held, before the shutdown
		}
		unread++
		if !o.answered() {
			dropped++
		}
		if want == "answered" {
			if !o.answered() {
				t.Errorf("%v/%s conn %d (post=%v): a head in the kernel when the shutdown began: %v, want it answered", eng, how, i, i%2 == 1, o)
			} else {
				wantBody := "ok"
				if i%2 == 1 {
					wantBody = "got " + strconv.Itoa(postBody1208)
				}
				if o.body != wantBody {
					t.Errorf("conn %d: body %q, want %q", i, o.body, wantBody)
				}
			}
		}
	}
	if unread == 0 {
		t.Errorf("%v/%s: every conn was on the loop that was not held: nothing was unread (p = 2^-23)", eng, how)
	}
	if want == "dropped" && dropped == 0 {
		t.Errorf("%v/%s: no conn was dropped: %d answered with close, want the held loop's heads dropped", eng, how, unread)
	}
	if how == "busy-loop" {
		_ = holder.SetReadDeadline(time.Now().Add(5 * time.Second))
		bo := classify1208(bufio.NewReader(holder), nil)
		if !bo.answered() || !bo.close || bo.body != "blocked" {
			t.Errorf("the request in the handler when the shutdown began: %v, want answered with Connection: close", bo)
		}
	}
	r.finish(append(conns, holder)...)
}

var bigBody1208 = make([]byte, 8<<20)

func routesBig1208(s *celeris.Server) {
	routes1208(s)
	s.GET("/big", func(c *celeris.Context) error { return c.Blob(http.StatusOK, "application/octet-stream", bigBody1208) })
}

// stallers dials n clients that ask for /big (8 MiB) and never read, with a
// 4 KiB receive buffer, so that every conn's response is more than the kernel
// holds: every worker has a send pending (a server under load when Shutdown
// comes). It returns once each server-side socket has bytes in its send queue
// (/proc/net/tcp, where it can be read; else after 300 ms), and returns the
// func that closes them. Each worker holds one with p = 1 - 2^-(n-1).
func (r *rig1208) stallers(t *testing.T, n int) func() {
	t.Helper()
	d := net.Dialer{Control: func(_, _ string, rc syscall.RawConn) error {
		return rc.Control(func(fd uintptr) { _ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096) })
	}}
	cs := make([]net.Conn, n)
	for i := range cs {
		c, err := d.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		cs[i] = c
		if _, err := io.WriteString(c, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
	}
	_, sport, _ := net.SplitHostPort(r.addr)
	for dl := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		queues, err := procTCPQueues1166()
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			break
		}
		pending := 0
		for _, c := range cs {
			cport := strconv.Itoa(c.LocalAddr().(*net.TCPAddr).Port)
			if q, ok := queues[[2]string{sport, cport}]; !ok || q.tx == 0 {
				pending++
			}
		}
		if pending == 0 {
			break
		}
		if time.Now().After(dl) {
			t.Fatalf("%d of %d stalled responses have nothing in the server's send queue 10 s on", pending, n)
		}
	}
	stop := func() {
		for _, c := range cs {
			_ = c.Close()
		}
	}
	t.Cleanup(stop)
	return stop
}

// silentAcceptedDraining1208 (A-accepted-draining): the same silent, idle and
// first-request-50-ms-in conns as A-accepted, with every worker having a
// response going out. epoll closes them at once. An io_uring worker serves on for
// the first 250 ms of its send drain (celeris#595, #806): its idle and silent
// conns get their FIN at 250 ms, and a first request before then is answered.
func silentAcceptedDraining1208(t *testing.T, eng celeris.EngineType) {
	r := startRig1208(t, celeris.Config{Engine: eng, DisableDeferAccept: true}, routesBig1208)
	stop := r.stallers(t, 16)
	base := settledEngineMetrics1104Server(r.s)
	silent, idle, getter := r.dial(), r.dial(), r.dial()
	if o := send1208(idle, get1208, 0); !o.answered() || o.close {
		t.Fatalf("the keep-alive conn before the shutdown: %v", o)
	}
	r.waitConns(base, 3)
	begin := time.Now()
	r.shutdown()
	type fin struct {
		err error
		at  time.Duration
	}
	finOf := func(c net.Conn) chan fin {
		ch := make(chan fin, 1)
		go func() { err := waitFIN1208(c, 3*time.Second); ch <- fin{err, time.Since(begin)} }()
		return ch
	}
	silentFin, idleFin := finOf(silent), finOf(idle)
	if eng == celeris.IOUring {
		time.Sleep(50 * time.Millisecond)
	} else if err := waitFIN1208(getter, 3*time.Second); err != nil {
		// epoll closes at its own pace while it sends: the request goes in after the FIN.
		t.Fatalf("%v: the silent conn had no FIN 3 s into the shutdown: %v", eng, err)
	}
	o := send1208(getter, get1208, 0)
	sf, idf := <-silentFin, <-idleFin
	for name, f := range map[string]fin{"silent": sf, "idle": idf} {
		if f.err != nil {
			t.Errorf("%v: the %s conn had no FIN 3 s into the shutdown: %v", eng, name, f.err)
			continue
		}
		if eng == celeris.IOUring && f.at < 150*time.Millisecond {
			t.Errorf("io_uring: the %s conn had its FIN at %v, want it at about 250 ms (the send drain's floor)", name, f.at)
		}
		if eng != celeris.IOUring && f.at > 240*time.Millisecond {
			t.Errorf("%v: the %s conn had its FIN at %v, want it at once", eng, name, f.at)
		}
	}
	switch {
	case eng == celeris.IOUring && (!o.answered() || o.body != "ok"):
		t.Errorf("io_uring: a first request 50 ms into the shutdown, with a send pending: %v, want it answered", o)
	case eng != celeris.IOUring && o.answered():
		t.Errorf("%v: a first request 50 ms into the shutdown: %v, want it dropped", eng, o)
	}
	stop() // Shutdown waits for the stalled responses
	r.finish(silent, idle, getter)
}

// silentDeferredDraining1208 (A-deferred-draining, io_uring): a conn
// TCP_DEFER_ACCEPT holds back sends its first request 50 ms into a shutdown while
// every worker has a response going out: the worker is still accepting (the first
// 250 ms of its send drain) and answers it, where an idle worker, or epoll,
// resets it (A-deferred).
func silentDeferredDraining1208(t *testing.T, eng celeris.EngineType) {
	r := startRig1208(t, celeris.Config{Engine: eng}, routesBig1208)
	stop := r.stallers(t, 16)
	d := r.dial()
	r.shutdown()
	time.Sleep(50 * time.Millisecond)
	o := send1208(d, get1208, 0)
	if !o.answered() || o.body != "ok" {
		t.Errorf("%v: a first request of a held-back conn 50 ms into the shutdown, with a send pending: %v, want it answered", eng, o)
	}
	stop() // Shutdown waits for the stalled responses
	r.finish(d)
}
