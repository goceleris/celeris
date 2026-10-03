//go:build linux

package epoll

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#543: a transplant's Carryover.Buffered (the pipelined requests its
// source had already read off the socket) must be replayed by the adopting
// engine exactly as if it had read them itself. Under AsyncHandlers that
// means the per-route decision a fresh conn's first read gets: an async
// route's handler runs on the conn's dispatch goroutine. Before the fix the
// io_uring adopt ran every carried request inline on its worker, and the
// epoll adopt dropped the carried bytes. The in-tree source (epoll's
// tryTransplant) builds no carry under AsyncHandlers, so these tests hand the
// adopting engine the carry through AdoptConn, the TransplantTarget API the
// adaptive switch uses. They build and run against the code before the fix.
// carry543Handler answers every path with its own name, except /block: an
// async route (RouteAsync) whose handler parks until release is closed. It
// counts the /block runs and signals each start.
type carry543Handler struct {
	release   chan struct{}
	started   chan struct{}
	blockRuns atomic.Int32
}

func newCarry543Handler() *carry543Handler {
	return &carry543Handler{release: make(chan struct{}), started: make(chan struct{}, 16)}
}

func (h *carry543Handler) HandleStream(_ context.Context, s *stream.Stream) error {
	var path string
	for _, hd := range s.GetHeaders() {
		if hd[0] == ":path" {
			path = hd[1]
		}
	}
	body := path
	if path == "/block" {
		h.blockRuns.Add(1)
		select {
		case h.started <- struct{}{}:
		default:
		}
		<-h.release
		body = "blocked-done"
	}
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(body))}}, []byte(body))
}

func (h *carry543Handler) RouteAsync(_, path string) bool { return path == "/block" }
func (h *carry543Handler) HasAsyncRoutes() bool           { return true }

var (
	_ stream.Handler            = (*carry543Handler)(nil)
	_ stream.AsyncRouteResolver = (*carry543Handler)(nil)
)

// adoptable543 returns a connected loopback TCP conn's client end and a
// non-blocking duplicate of its server end that no engine and no Go poller
// owns: what a source engine hands to AdoptConn after it has detached it.
func adoptable543(t *testing.T) (net.Conn, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	srv, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	sc, err := srv.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatalf("syscallconn: %v", err)
	}
	fd, dupErr := -1, error(nil)
	if cerr := sc.Control(func(raw uintptr) { fd, dupErr = unix.Dup(int(raw)) }); cerr != nil || dupErr != nil {
		t.Fatalf("dup: %v %v", cerr, dupErr)
	}
	_ = srv.Close()
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatalf("setnonblock: %v", err)
	}
	return client, fd
}

// readBody543 reads one response off br within d and returns its status and
// body, or an error.
func readBody543(c net.Conn, br *bufio.Reader, d time.Duration) (int, string, error) {
	_ = c.SetReadDeadline(time.Now().Add(d))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// runCarryAsync543 is celeris#543's scenario against one adopting engine
// running with AsyncHandlers. A connection is adopted with a carried request
// for the async route /block, as a transplant's Carryover.Buffered carries
// the pipelined bytes its source had already read. That request must reach
// its handler, on the conn's dispatch goroutine and not on the worker that
// adopted the conn: while the handler is parked, one more conn is adopted per
// worker (AdoptConn hands conns to the workers in turn, so one of them lands
// on the first conn's worker) and every one of them must be served. Then the
// handler is released and the carried request must be answered, once, on a
// conn that keeps serving.
func runCarryAsync543(t *testing.T, name string, workers int, adopt func(int, engine.Carryover) error, h *carry543Handler) {
	t.Helper()
	runCarrySplit543(t, name, workers, adopt, h, "GET /block HTTP/1.1\r\nHost: x\r\n\r\n", "")
}

// runCarrySplit543 is runCarryAsync543 with the /block request cut in two:
// carried holds its start, as a source that had read part of the request
// would carry it, and rest is what the client sends after the adoption. A
// replay that leaves a request half-parsed must hand its continuation to the
// dispatch goroutine: the continuation's parse does not ask the per-route
// async question again.
func runCarrySplit543(t *testing.T, name string, workers int, adopt func(int, engine.Carryover) error, h *carry543Handler, carried, rest string) {
	t.Helper()
	if workers < 1 {
		t.Fatalf("%s reports %d workers", name, workers)
	}
	clientA, fdA := adoptable543(t)
	carry := engine.Carryover{
		RemoteAddr: clientA.LocalAddr().String(),
		Buffered:   []byte(carried),
	}
	if err := adopt(fdA, carry); err != nil {
		_ = unix.Close(fdA)
		t.Fatalf("AdoptConn with a carried request: %v", err)
	}
	if rest != "" {
		if _, err := clientA.Write([]byte(rest)); err != nil {
			t.Fatalf("write the rest of the carried request: %v", err)
		}
	}
	started := false
	select {
	case <-h.started:
		started = true
	case <-time.After(3 * time.Second):
	}

	served := 0
	for i := range workers {
		c, fd := adoptable543(t)
		if err := adopt(fd, engine.Carryover{RemoteAddr: c.LocalAddr().String()}); err != nil {
			_ = unix.Close(fd)
			t.Fatalf("AdoptConn #%d: %v", i, err)
		}
		path := "/fast" + strconv.Itoa(i)
		if _, err := c.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		code, body, err := readBody543(c, bufio.NewReader(c), 2*time.Second)
		if err == nil && code == 200 && body == path {
			served++
		} else {
			t.Logf("%s: adopted conn #%d got no answer to %s while /block ran: code=%d body=%q err=%v", name, i, path, code, body, err)
		}
		_ = c.Close()
	}

	close(h.release)
	brA := bufio.NewReader(clientA)
	code, body, err := readBody543(clientA, brA, 3*time.Second)
	answered := err == nil && code == 200 && body == "blocked-done"
	followUp := false
	if answered {
		if _, werr := clientA.Write([]byte("GET /after HTTP/1.1\r\nHost: x\r\n\r\n")); werr == nil {
			c2, b2, e2 := readBody543(clientA, brA, 3*time.Second)
			followUp = e2 == nil && c2 == 200 && b2 == "/after"
		}
	}
	runs := h.blockRuns.Load()
	t.Logf("celeris543 RESULT engine=%s split=%v workers=%d carried_started=%v others_served=%d/%d carried_answered=%v block_runs=%d follow_up=%v",
		name, rest != "", workers, started, served, workers, answered, runs, followUp)
	if !started {
		t.Errorf("%s: the carried /block request never reached its handler within 3s: the carried bytes were dropped", name)
	}
	if served != workers {
		t.Errorf("%s: %d of %d conns adopted while the carried async request ran got no answer: its handler ran on the worker that adopted the conn, not on a dispatch goroutine", name, workers-served, workers)
	}
	if !answered {
		t.Errorf("%s: the carried /block request was not answered after its handler was released: code=%d body=%q err=%v", name, code, body, err)
	}
	if runs != 1 {
		t.Errorf("%s: /block ran %d times, want 1", name, runs)
	}
	if answered && !followUp {
		t.Errorf("%s: the adopted conn did not serve a request after the carried one", name)
	}
}

// runCarryPipelined543 adopts a conn whose carry holds three pipelined
// requests, a sync route, the async /block and another sync route, with
// /block's handler already released. The three answers must come back whole
// and in order, and the conn must keep serving.
func runCarryPipelined543(t *testing.T, name string, adopt func(int, engine.Carryover) error, h *carry543Handler) {
	t.Helper()
	close(h.release)
	client, fd := adoptable543(t)
	carry := engine.Carryover{
		RemoteAddr: client.LocalAddr().String(),
		Buffered: []byte("GET /first HTTP/1.1\r\nHost: x\r\n\r\n" +
			"GET /block HTTP/1.1\r\nHost: x\r\n\r\n" +
			"GET /third HTTP/1.1\r\nHost: x\r\n\r\n"),
	}
	if err := adopt(fd, carry); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("AdoptConn with three carried requests: %v", err)
	}
	br := bufio.NewReader(client)
	var got []string
	for range 3 {
		code, body, err := readBody543(client, br, 3*time.Second)
		if err != nil || code != 200 {
			got = append(got, "ERR("+strconv.Itoa(code)+")")
			break
		}
		got = append(got, body)
	}
	followUp := false
	if _, err := client.Write([]byte("GET /after HTTP/1.1\r\nHost: x\r\n\r\n")); err == nil {
		code, body, err := readBody543(client, br, 3*time.Second)
		followUp = err == nil && code == 200 && body == "/after"
	}
	want := []string{"/first", "blocked-done", "/third"}
	t.Logf("celeris543 PIPELINED engine=%s answers=%q follow_up=%v block_runs=%d", name, got, followUp, h.blockRuns.Load())
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: carried pipelined requests answered %q, want %q in that order", name, got, want)
	}
	if !followUp {
		t.Errorf("%s: the adopted conn did not serve a request after the carried ones", name)
	}
	if n := h.blockRuns.Load(); n != 1 {
		t.Errorf("%s: /block ran %d times, want 1", name, n)
	}
}

// start543 starts an epoll engine on a free loopback port and waits until it
// has loops to adopt onto.
func start543(t *testing.T, async bool, h stream.Handler) *Engine {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr: addr, Protocol: engine.HTTP1, AsyncHandlers: async,
		Resources: resource.Resources{Workers: 2},
	}, h)
	if err != nil {
		t.Fatalf("epoll.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	for dl := time.Now().Add(20 * time.Second); time.Now().Before(dl); {
		if e.Addr() != nil && e.NumWorkers() > 0 {
			return e
		}
		select {
		case lerr := <-done:
			t.Fatalf("epoll engine did not start: %v", lerr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("epoll engine did not start within 20s")
	return nil
}

func TestEpollAdoptCarriedAsyncRoute543(t *testing.T) {
	h := newCarry543Handler()
	e := start543(t, true, h)
	runCarryAsync543(t, "epoll", e.NumWorkers(), e.AdoptConn, h)
}

// TestEpollAdoptCarriedPartialAsyncRoute543 carries only the start of the
// /block request; the client sends its last header line after the adoption.
func TestEpollAdoptCarriedPartialAsyncRoute543(t *testing.T) {
	h := newCarry543Handler()
	e := start543(t, true, h)
	runCarrySplit543(t, "epoll", e.NumWorkers(), e.AdoptConn, h,
		"GET /block HTTP/1.1\r\nHost: x\r\n", "X-Rest: 1\r\n\r\n")
}

func TestEpollAdoptCarriedPipelinedAsync543(t *testing.T) {
	h := newCarry543Handler()
	e := start543(t, true, h)
	runCarryPipelined543(t, "epoll-async", e.AdoptConn, h)
}

// TestEpollAdoptCarriedPipelinedSync543 is the sync-mode control: the carry
// is replayed inline, as it always was.
func TestEpollAdoptCarriedPipelinedSync543(t *testing.T) {
	h := newCarry543Handler()
	e := start543(t, false, h)
	runCarryPipelined543(t, "epoll-sync", e.AdoptConn, h)
}
