//go:build linux

package epoll

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// The pool wait's gate (celeris#885 item 49) closes an HTTP/1 conn that holds
// nothing: closeIdleH1 looked at the conn's own state (nothing queued, no
// request in progress, or "accepted and has sent nothing": cs.detected false and
// cs.detectN 0, which means the LOOP has read no byte of it) and at the kernel's
// SEND queue, never at the kernel's RECEIVE queue. A request that reached the
// kernel before the gate but that the loop had not read yet (a conn accepted in
// the iteration before the pass, whose read event the next epoll_wait reports;
// the next request of a keep-alive conn, sent while the loop was busy) was cut:
// close(2) with unread bytes sends an RST, and with none left to read the client
// sees an EOF in place of the answer. TestShutdownPoolWaitGateRequestsInProgress
// AndIdleConns885 failed once in CI (a coverage-and-race run) with three resets
// 257 ms into the shutdown (celeris#1104 lane). These tests make the window
// deterministic: a handler running inline on a loop thread does what the loop
// does at the top of an iteration once the pool wait is past poolWaitGrace
// (poolWaitGate), while clients whose request is waiting in the kernel's queue
// are not read yet.

// auto1104 gives the engine protocol detection, as the server under test has
// (a conn's protocol is decided by its first bytes): a conn that has sent none
// is undetected.
func auto1104(c *resource.Config) { c.Protocol = engine.Auto }

type gateUnread1104Handler struct {
	eng      atomic.Pointer[Engine]
	started  chan struct{}
	release  chan struct{}
	held     atomic.Pointer[Loop]
	self     atomic.Pointer[string] // the client address of the /gate conn, as the loop names it
	eligible atomic.Int64           // conns of the held loop the pass looked at, minus the handler's own
	// onGate runs on the held loop's thread, in place of the loop's turn at the
	// top of an iteration: what the test does before the pass.
	onGate func(l *Loop)
	// passes run on the held loop: the gate's pass the test wants.
	pass func(l *Loop)
}

func (h *gateUnread1104Handler) HandleStream(_ context.Context, s *stream.Stream) error {
	var path string
	for _, hd := range s.GetHeaders() {
		if hd[0] == ":path" {
			path = hd[1]
		}
	}
	if path == "/gate" {
		l := h.eng.Load().loops[s.WorkerID]
		h.held.Store(l)
		h.started <- struct{}{}
		<-h.release
		if h.onGate != nil {
			h.onGate(l)
		}
		// The handler's own conn is mid-request: it is not the pass's to look at.
		var self *connState
		for _, cs := range l.liveConns {
			if cs.remoteAddr == h.selfAddr() {
				self = cs
			}
		}
		if self != nil {
			self.dirty = true
		}
		h.eligible.Store(int64(len(l.liveConns)))
		h.pass(l)
		if self != nil {
			self.dirty = false
		}
	}
	if s.ResponseWriter == nil {
		return nil
	}
	body := path + ":" + strconv.Itoa(len(s.GetData()))
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(body))}}, []byte(body))
}

func (h *gateUnread1104Handler) selfAddr() string {
	if p := h.self.Load(); p != nil {
		return *p
	}
	return ""
}

func (*gateUnread1104Handler) RouteAsync(_, _ string) bool { return false }
func (*gateUnread1104Handler) HasAsyncRoutes() bool        { return false }

// gatePass1104 is the gate as the loop runs it once the pool wait is past
// poolWaitGrace: poolWaitGate, with the wait begun long enough ago.
func gatePass1104(l *Loop) {
	l.h2DrainStart = time.Now().Add(-2 * poolWaitGrace)
	l.poolWaitGate()
}

// holdLoop1104 starts an engine whose handler holds one loop thread (a GET
// /gate request) and returns once the loop is inside the handler.
func holdLoop1104(t *testing.T, h *gateUnread1104Handler) (string, net.Conn) {
	t.Helper()
	e, addr := startE2E885(t, false, h, auto1104)
	h.eng.Store(e)
	b, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	self := b.LocalAddr().String()
	h.self.Store(&self)
	if _, err := b.Write([]byte("GET /gate HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the /gate handler did not start")
	}
	return addr, b
}

// get1104 writes a GET and reads its response, with a deadline.
func get1104(c net.Conn, br *bufio.Reader, path string) (*http.Response, error) {
	_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		return nil, err
	}
	return readResp1104(c, br)
}

func readResp1104(c net.Conn, br *bufio.Reader) (*http.Response, error) {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp, nil
}

func isTimeout1104(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// TestPoolWaitGateDoesNotCloseAConnWithAnUnreadRequest1104: 32 conns are
// accepted by the held loop in the iteration before the pass, each with its
// first request unread in the kernel; the pass must read them and answer with
// Connection: close, not close them. (On main: 11 to 21 of 32 closed.)
func TestPoolWaitGateDoesNotCloseAConnWithAnUnreadRequest1104(t *testing.T) {
	var accepted atomic.Int64
	h := &gateUnread1104Handler{started: make(chan struct{}, 1), release: make(chan struct{})}
	h.onGate = func(l *Loop) {
		// The loop's accept of the iteration before the pass.
		before := len(l.liveConns)
		l.acceptAll(context.Background(), time.Now().UnixNano())
		accepted.Store(int64(len(l.liveConns) - before))
	}
	h.pass = gatePass1104
	addr, _ := holdLoop1104(t, h)
	// The loop thread is inside the handler: these conns are queued on the
	// listeners with their first request unread. Those on the other loop are
	// served as usual; those on the held loop wait.
	const n = 32
	type client struct {
		c  net.Conn
		br *bufio.Reader
	}
	var clients []client
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		defer func() { _ = c.Close() }()
		if _, err := c.Write([]byte("GET /x HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		clients = append(clients, client{c, bufio.NewReader(c)})
	}
	time.Sleep(200 * time.Millisecond)
	close(h.release)

	// Every request is answered: by the other loop as usual, by the held loop
	// with Connection: close (the gate's rule for a request it answers). Never a
	// close in place of the answer.
	closedInstead, closeAfter := 0, 0
	var first error
	for _, cl := range clients {
		resp, err := readResp1104(cl.c, cl.br)
		if err != nil {
			closedInstead++
			if first == nil {
				first = err
			}
			continue
		}
		if resp.StatusCode != 200 {
			t.Errorf("status %d", resp.StatusCode)
		}
		if resp.Close {
			closeAfter++
		}
	}
	t.Logf("held loop accepted %d conns in the pass's iteration; %d of %d clients got no answer (first: %v; reset=%v); %d answers carry Connection: close", accepted.Load(), closedInstead, n, first, errors.Is(first, syscall.ECONNRESET), closeAfter)
	if closedInstead != 0 {
		t.Errorf("the gate's pass closed %d of %d conns whose first request was still unread in the kernel (%v): a conn with unread bytes is not idle, and close(2) resets it", closedInstead, n, first)
	}
	if int64(closeAfter) != accepted.Load() {
		t.Errorf("%d answers carry Connection: close, want %d (the conns the pass met with a request unread: answered after the gate)", closeAfter, accepted.Load())
	}
}

// TestPoolWaitGateDoesNotCloseAKeepAliveConnWithAnUnreadRequest1104: the same
// window on a conn the loop has read from: a keep-alive client sends its next
// request while the loop is busy; the pass finds the conn idle by the loop's
// own state, with the request unread in the kernel.
func TestPoolWaitGateDoesNotCloseAKeepAliveConnWithAnUnreadRequest1104(t *testing.T) {
	h := &gateUnread1104Handler{started: make(chan struct{}, 1), release: make(chan struct{})}
	h.pass = gatePass1104
	// Idle keep-alive conns first (served and detected), then the loop is held.
	e, addr := startE2E885(t, false, h, auto1104)
	h.eng.Store(e)
	const n = 32
	type client struct {
		c  net.Conn
		br *bufio.Reader
	}
	var clients []client
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		defer func() { _ = c.Close() }()
		cl := client{c, bufio.NewReader(c)}
		if _, err := get1104(cl.c, cl.br, "/x"); err != nil {
			t.Fatalf("first request %d: %v", i, err)
		}
		clients = append(clients, cl)
	}
	b, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	self := b.LocalAddr().String()
	h.self.Store(&self)
	if _, err := b.Write([]byte("GET /gate HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the /gate handler did not start")
	}
	for i, cl := range clients {
		_ = cl.c.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if _, err := cl.c.Write([]byte("GET /y HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("second request %d: %v", i, err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	close(h.release)

	closedInstead, closeAfter := 0, 0
	var first error
	for _, cl := range clients {
		resp, err := readResp1104(cl.c, cl.br)
		if err != nil {
			closedInstead++
			if first == nil {
				first = err
			}
			continue
		}
		if resp.Close {
			closeAfter++
		}
	}
	held := int(h.eligible.Load()) - 1 // the handler's own conn
	t.Logf("held loop had %d idle keep-alive conns at the pass; %d of %d clients got no answer to the request in the kernel's queue (first: %v; reset=%v); %d answers carry Connection: close", held, closedInstead, n, first, errors.Is(first, syscall.ECONNRESET), closeAfter)
	if closedInstead != 0 {
		t.Errorf("the gate's pass closed %d of %d keep-alive conns whose next request was unread in the kernel (%v)", closedInstead, n, first)
	}
	if closeAfter != held {
		t.Errorf("%d answers carry Connection: close, want %d (every conn of the held loop)", closeAfter, held)
	}
}

// TestPoolWaitGateClosesIdleConnsWithAFIN1104 is the control of the two tests
// above: an idle keep-alive conn that holds nothing in the kernel is closed by
// the pass, with a FIN (EOF, not a reset), and the conns of the other loop are
// left alone. The unread-bytes check must not keep an idle conn open.
func TestPoolWaitGateClosesIdleConnsWithAFIN1104(t *testing.T) {
	h := &gateUnread1104Handler{started: make(chan struct{}, 1), release: make(chan struct{})}
	h.pass = gatePass1104
	e, addr := startE2E885(t, false, h, auto1104)
	h.eng.Store(e)
	const n = 32
	type client struct {
		c  net.Conn
		br *bufio.Reader
	}
	var clients []client
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		defer func() { _ = c.Close() }()
		cl := client{c, bufio.NewReader(c)}
		if _, err := get1104(cl.c, cl.br, "/x"); err != nil {
			t.Fatalf("first request %d: %v", i, err)
		}
		clients = append(clients, cl)
	}
	b, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	self := b.LocalAddr().String()
	h.self.Store(&self)
	if _, err := b.Write([]byte("GET /gate HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the /gate handler did not start")
	}
	close(h.release)
	time.Sleep(100 * time.Millisecond)

	fin, other := 0, 0
	for i, cl := range clients {
		_ = cl.c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		_, err := cl.br.ReadByte()
		switch {
		case err == io.EOF:
			fin++
		case isTimeout1104(err):
			other++
		default:
			t.Errorf("client %d read %v, want a FIN or nothing", i, err)
		}
	}
	held := int(h.eligible.Load()) - 1
	t.Logf("held loop: %d idle conns; %d closed with a FIN, %d left alone (the other loop's)", held, fin, other)
	if fin != held {
		t.Errorf("%d conns got a FIN, want %d (every idle conn of the held loop): the gate must still close what holds nothing", fin, held)
	}
	if fin+other != n {
		t.Errorf("%d + %d != %d", fin, other, n)
	}
}

// startKillable1104 is startE2E885 with the cancel function of the engine's
// context handed back, for a test that begins the shutdown itself.
func startKillable1104(t *testing.T, h stream.Handler) (*Engine, string, context.CancelFunc, chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
	}, h)
	if err != nil {
		t.Fatalf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(30 * time.Second):
		}
	})
	for dl := time.Now().Add(15 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}
	return e, e.Addr().String(), cancel, errCh
}

// TestListenerCloseResetsTheAcceptQueue1104 pins the other source of a reset in
// the shutdown: stopAccepting closes the listener at the first iteration that
// sees the cancelled context, and the kernel resets every conn still in that
// listener's accept queue (stopAccepting's doc). A conn the loop had not
// accepted yet, whether it holds a request or has sent nothing and was handed
// over by TCP_DEFER_ACCEPT's timeout, reads ECONNRESET at once; this has
// nothing to do with the gate. It is the answer to the silent conn that read a
// reset in the CI failure: no byte of it was ever received, so the gate's close
// of it would have been a FIN.
func TestListenerCloseResetsTheAcceptQueue1104(t *testing.T) {
	h := &gateUnread1104Handler{started: make(chan struct{}, 1), release: make(chan struct{})}
	h.pass = func(*Loop) {}
	e, addr, cancel, _ := startKillable1104(t, h)
	h.eng.Store(e)
	b, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if _, err := b.Write([]byte("GET /gate HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the /gate handler did not start")
	}
	// Silent conns: TCP_DEFER_ACCEPT keeps them out of the accept queue for
	// about a second, then the kernel hands them over; the held loop does not
	// accept them. Conns with a request are in the queue at once.
	const n = 32
	var silent, asking []net.Conn
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		silent = append(silent, c)
	}
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if _, err := c.Write([]byte("GET /x HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		asking = append(asking, c)
	}
	time.Sleep(2500 * time.Millisecond)
	// Conns on the free loop were accepted and answered by now; the held loop's
	// are in its listener's queue. The shutdown begins, then the loop is let go.
	cancel()
	close(h.release)

	resets := func(cs []net.Conn) (reset, fin, none int) {
		for _, c := range cs {
			_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			_, err := c.Read(make([]byte, 1))
			switch {
			case errors.Is(err, syscall.ECONNRESET):
				reset++
			case err == io.EOF:
				fin++
			case isTimeout1104(err):
				none++
			}
		}
		return
	}
	sr, sf, sn := resets(silent)
	ar, af, an := resets(asking)
	t.Logf("silent conns: %d reset, %d FIN, %d nothing; conns with a request: %d reset, %d FIN, %d nothing", sr, sf, sn, ar, af, an)
	if sr == 0 {
		t.Errorf("no silent conn read ECONNRESET: the listener's close left the accept queue alone, and the premise of this test (and of its explanation of the CI reset) is gone")
	}
}

type unreadClient1104 struct {
	c  net.Conn
	br *bufio.Reader
}

// holdWith1104 starts the engine (mut adjusts its config), dials pre clients
// (served once when served is true, else silent), holds one loop with a GET
// /gate, lets write send each client's request while the loop is held, runs the
// pass and returns what each client read. held is the number of conns the pass
// looked at on the held loop.
func holdWith1104(t *testing.T, mut func(*resource.Config), served bool, write func(i int, c net.Conn)) (resps []*http.Response, bodies []string, errs []error, held int) {
	t.Helper()
	h := &gateUnread1104Handler{started: make(chan struct{}, 1), release: make(chan struct{})}
	h.pass = gatePass1104
	e, addr := startE2E885(t, false, h, mut)
	h.eng.Store(e)
	const n = 32
	var clients []unreadClient1104
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		cl := unreadClient1104{c, bufio.NewReader(c)}
		if served {
			if _, err := get1104(cl.c, cl.br, "/x"); err != nil {
				t.Fatalf("first request %d: %v", i, err)
			}
		}
		clients = append(clients, cl)
	}
	// With TCP_DEFER_ACCEPT off the loop has the silent conns by now.
	time.Sleep(300 * time.Millisecond)
	b, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	self := b.LocalAddr().String()
	h.self.Store(&self)
	if _, err := b.Write([]byte("GET /gate HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the /gate handler did not start")
	}
	for i, cl := range clients {
		write(i, cl.c)
	}
	time.Sleep(300 * time.Millisecond)
	close(h.release)
	for _, cl := range clients {
		resp, err := http.ReadResponse(cl.br, nil)
		var body []byte
		if err == nil {
			_ = cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
			body, _ = io.ReadAll(resp.Body)
		}
		_ = cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
		resps, bodies, errs = append(resps, resp), append(bodies, string(body)), append(errs, err)
	}
	return resps, bodies, errs, int(h.eligible.Load()) - 1
}

// TestPoolWaitGateDoesNotCloseASilentConnWithAUnreadFirstRequest1104 is the
// first-request test without the emulated accept: the conns were accepted well
// before the pass (TCP_DEFER_ACCEPT off, so the loop has them with no byte), and
// their first request reaches the kernel while the loop is busy.
func TestPoolWaitGateDoesNotCloseASilentConnWithAUnreadFirstRequest1104(t *testing.T) {
	resps, _, errs, held := holdWith1104(t, func(c *resource.Config) { auto1104(c); c.DisableDeferAccept = true }, false, func(_ int, c net.Conn) {
		_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte("GET /x HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Errorf("write: %v", err)
		}
	})
	closedInstead, closeAfter := 0, 0
	for i := range resps {
		if errs[i] != nil {
			closedInstead++
			continue
		}
		if resps[i].Close {
			closeAfter++
		}
	}
	t.Logf("held loop had %d silent conns at the pass; %d of %d got no answer; %d answers carry Connection: close", held, closedInstead, len(resps), closeAfter)
	if closedInstead != 0 {
		t.Errorf("the pass closed %d silent conns whose first request was unread in the kernel (%v)", closedInstead, errs)
	}
	if closeAfter != held {
		t.Errorf("%d answers carry Connection: close, want %d (every conn of the held loop)", closeAfter, held)
	}
}

// TestPoolWaitGateReadsAnUnreadRequestPastTheReadBudget1104: the request the
// pass finds in the kernel is a POST whose body is many times the read budget
// (the shape of the CI failure's upload): the inline read yields at the budget
// (yieldRead re-arms the conn and returns), and the conn must be read on, not
// refused as an idle HTTP/1 conn, until the whole body is in and answered.
func TestPoolWaitGateReadsAnUnreadRequestPastTheReadBudget1104(t *testing.T) {
	withBudget885(t, 1024)
	const size = 24 << 10
	resps, bodies, errs, held := holdWith1104(t, auto1104, true, func(_ int, c net.Conn) {
		_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write(append([]byte("POST /up HTTP/1.1\r\nHost: x\r\nContent-Length: "+strconv.Itoa(size)+"\r\n\r\n"), make([]byte, size)...)); err != nil {
			t.Errorf("write: %v", err)
		}
	})
	closeAfter := 0
	for i := range resps {
		if errs[i] != nil {
			t.Errorf("client %d: %v: a request in the kernel at the pass was not answered", i, errs[i])
			continue
		}
		if want := "/up:" + strconv.Itoa(size); bodies[i] != want {
			t.Errorf("client %d: body %q, want %q", i, bodies[i], want)
		}
		if resps[i].Close {
			closeAfter++
		}
	}
	t.Logf("held loop had %d conns at the pass; %d answers carry Connection: close", held, closeAfter)
	if closeAfter != held {
		t.Errorf("%d answers carry Connection: close, want %d", closeAfter, held)
	}
}
