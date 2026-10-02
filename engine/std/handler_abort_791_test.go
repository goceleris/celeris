package std

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#791's std arm. The defect was epoll's and io_uring's: a handler
// panic, or runtime.Goexit, on a dispatch goroutine left that conn's
// detachMu locked and parked the loop or worker that owned the conn. The std
// engine holds no engine lock around a handler, and net/http ends a
// connection whose handler panicked or exited (its per-connection deferred
// close), so these pass on the base as well. They pin that the std engine
// keeps serving across both, as the other engines now do: a keep-alive
// witness opened before the faults is answered after them, fresh connections
// are answered, and Listen returns after cancel, every read within
// abortBudget791.

const (
	abortBudget791 = 2 * time.Second
	abortBooms791  = 4
	abortFresh791  = 8
)

type abortHandler791 struct {
	goexit bool
	hits   *atomic.Int64
}

func (h abortHandler791) HandleStream(_ context.Context, s *stream.Stream) error {
	// The std bridge carries the path as the :path pseudo-header (s.Path is
	// set by the H1 engines only).
	path := s.Path
	for _, hd := range s.GetHeaders() {
		if hd[0] == ":path" {
			path = hd[1]
		}
	}
	if path == "/boom" {
		h.hits.Add(1)
		if h.goexit {
			runtime.Goexit()
		}
		panic("celeris791: handler panic")
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}

type abortConn791 struct {
	c  net.Conn
	br *bufio.Reader
}

func (a *abortConn791) get(path string) (int, string, error) {
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

func abortDial791(t *testing.T, addr string) *abortConn791 {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, abortBudget791)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &abortConn791{c: c, br: bufio.NewReader(c)}
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

func runAbort791(t *testing.T, goexit bool) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var hits atomic.Int64
	e, err := New(resource.Config{Listener: ln, Engine: engine.Std, Protocol: engine.HTTP1},
		abortHandler791{goexit: goexit, hits: &hits})
	if err != nil {
		_ = ln.Close()
		t.Fatalf("New: %v", err)
	}
	// net/http logs the recovered panic through the server's ErrorLog; keep
	// the stack out of the test output.
	if e.server != nil {
		e.server.ErrorLog = log.New(io.Discard, "", 0)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	for dl := time.Now().Add(5 * time.Second); e.Addr() == nil; {
		if time.Now().After(dl) {
			t.Fatal("listener not ready within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	addr := e.Addr().String()

	witness := abortDial791(t, addr)
	if status, body, err := witness.get("/ok"); err != nil || status != 200 || body != "ok" {
		t.Fatalf("PREMISE: /ok before the fault: status %d body %q err %v", status, body, err)
	}
	boomOut := map[string]int{}
	for range abortBooms791 {
		status, _, err := abortDial791(t, addr).get("/boom")
		out := abortClass791(err)
		if err == nil {
			out = fmt.Sprintf("status=%d", status)
		}
		boomOut[out]++
	}
	status, body, werr := witness.get("/ok")
	witnessOK := werr == nil && status == 200 && body == "ok"
	freshOK := 0
	for range abortFresh791 {
		if status, body, err := abortDial791(t, addr).get("/ok"); err == nil && status == 200 && body == "ok" {
			freshOK++
		}
	}
	cancel()
	stopped := false
	select {
	case <-done:
		stopped = true
	case <-time.After(5 * time.Second):
	}
	t.Logf("celeris791 RESULT engine=std goexit=%v hits=%d boom=%v witness=%v(%s) fresh=%d/%d stopped=%v",
		goexit, hits.Load(), boomOut, witnessOK, abortClass791(werr), freshOK, abortFresh791, stopped)

	if n := hits.Load(); n != abortBooms791 {
		t.Errorf("INJECTION: /boom ran %d times, want %d", n, abortBooms791)
	}
	if boomOut["timeout"] != 0 || boomOut["status=200"] != 0 {
		t.Errorf("/boom outcomes %v: want every conn torn down within %v", boomOut, abortBudget791)
	}
	if !witnessOK {
		t.Errorf("the keep-alive witness was not answered after the faults: status %d body %q err %v", status, body, werr)
	}
	if freshOK != abortFresh791 {
		t.Errorf("%d of %d fresh conns answered after the faults", freshOK, abortFresh791)
	}
	if !stopped {
		t.Error("Listen did not return within 5s of cancel")
	}
}

func TestStdHandlerPanicLeavesTheEngineServing(t *testing.T)  { runAbort791(t, false) }
func TestStdHandlerGoexitLeavesTheEngineServing(t *testing.T) { runAbort791(t, true) }
