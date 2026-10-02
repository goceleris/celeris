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
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#791 through the public API. A handler panic never reaches the
// engines' dispatch-goroutine recover through celeris.Server: the router's
// last-resort recover turns it into a 500 first. A runtime.Goexit does reach
// it, because recover() returns nil for a Goexit: t.FailNow or t.SkipNow in a
// test's handler, or any library that calls runtime.Goexit, on an .Async()
// route. On epoll and io_uring that left the conn owned by a dispatch
// goroutine that no longer existed: the conn was never answered or closed,
// and the server did not stop.
//
// Each test serves /boom (.Async()) and /ok (inline, answers with the
// serving worker's id). Before the fault it opens, on every worker, a
// keep-alive WITNESS connection and a BOOM connection, each proven to be
// served by that worker. Every boom connection then sends /boom. Its conn
// must be answered or torn down, every worker must still answer its witness,
// eight fresh connections must be answered, and the server must stop after
// its context is cancelled, every read within goexitBudget791. The panic arm
// is the control: the router answers it with a 500 on the base as well, so
// the rig can pass, and that is what makes the Goexit arm's failure mean
// something.

const (
	goexitBudget791 = 2 * time.Second
	goexitFresh791  = 8
	goexitStop791   = 10 * time.Second
)

type goexitConn791 struct {
	c  net.Conn
	br *bufio.Reader
}

func (a *goexitConn791) get(path string) (status int, body string, err error) {
	_ = a.c.SetDeadline(time.Now().Add(goexitBudget791))
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

func goexitClass791(err error) string {
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

func runServerAsyncAbort791(t *testing.T, eng celeris.EngineType, goexit bool) {
	if eng == celeris.IOUring {
		if ok, p := keptProbeIOUring(); !ok {
			if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
				t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids skipping", p.IOUringTier, p.KernelVersion)
			}
			t.Skipf("io_uring tier=%s kernel=%s: no usable io_uring", p.IOUringTier, p.KernelVersion)
		}
	}
	mode := "panic"
	if goexit {
		mode = "goexit"
	}
	var hits atomic.Int64
	s := celeris.New(celeris.Config{Engine: eng, Workers: 2})
	s.GET("/boom", func(c *celeris.Context) error {
		hits.Add(1)
		if goexit {
			runtime.Goexit()
		}
		panic("celeris791: handler panic")
	}).Async()
	s.GET("/ok", func(c *celeris.Context) error {
		return c.String(200, "w=%d", c.WorkerID())
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
	addr, err := c714WaitReady(s, done)
	if err != nil {
		t.Fatalf("server did not start: %v", err)
	}
	dial := func() *goexitConn791 {
		c, err := net.DialTimeout("tcp", addr, goexitBudget791)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return &goexitConn791{c: c, br: bufio.NewReader(c)}
	}

	// Before the fault: a witness and a boom conn on every worker that the
	// first 64 dials reach, each proven to be served by it.
	witness := map[string]*goexitConn791{}
	boom := map[string]*goexitConn791{}
	for range 64 {
		a := dial()
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
	ids := make([]string, 0, len(boom))
	for id := range boom {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if len(ids) == 0 {
		t.Fatal("PREMISE: 64 dials put no worker on two conns")
	}

	var boomOut []string
	for _, id := range ids {
		status, _, err := boom[id].get("/boom")
		out := goexitClass791(err)
		if err == nil {
			out = "status=" + strconv.Itoa(status)
		}
		boomOut = append(boomOut, id+":"+out)
	}
	witnessOK := 0
	var witnessOut []string
	for _, id := range ids {
		status, body, err := witness[id].get("/ok")
		if err == nil && status == 200 && body == id {
			witnessOK++
			witnessOut = append(witnessOut, id+":ok")
		} else {
			witnessOut = append(witnessOut, fmt.Sprintf("%s:%s/%d/%q", id, goexitClass791(err), status, body))
		}
	}
	freshOK := 0
	freshOut := map[string]int{}
	for range goexitFresh791 {
		c, err := net.DialTimeout("tcp", addr, goexitBudget791)
		if err != nil {
			freshOut["dial:"+goexitClass791(err)]++
			continue
		}
		a := &goexitConn791{c: c, br: bufio.NewReader(c)}
		status, body, err := a.get("/ok")
		_ = c.Close()
		if err == nil && status == 200 && strings.HasPrefix(body, "w=") {
			freshOK++
			freshOut[body]++
		} else {
			freshOut[goexitClass791(err)]++
		}
	}
	cancel()
	stopped := false
	select {
	case <-done:
		stopped = true
	case <-time.After(goexitStop791):
	}

	t.Logf("celeris791 RESULT engine=server-%s mode=%s workers_judged=%d hits=%d boom=%v witness=%d/%d %v fresh=%d/%d %v stopped=%v",
		eng, mode, len(ids), hits.Load(), boomOut, witnessOK, len(ids), witnessOut, freshOK, goexitFresh791, freshOut, stopped)

	if n := hits.Load(); n != int64(len(ids)) {
		t.Errorf("INJECTION: /boom ran %d times, want %d (one per judged worker)", n, len(ids))
	}
	for _, o := range boomOut {
		switch {
		case strings.HasSuffix(o, ":timeout"):
			t.Errorf("celeris#791: /boom (%s) got %s; want its conn answered or torn down within %v", mode, o, goexitBudget791)
		case !goexit && !strings.HasSuffix(o, ":status=500"):
			t.Errorf("CONTROL: /boom (panic) got %s; want the router's 500", o)
		}
	}
	if witnessOK != len(ids) {
		t.Errorf("celeris#791: after a /boom (%s), %d of %d workers answered their witness conn within %v (%v)",
			mode, witnessOK, len(ids), goexitBudget791, witnessOut)
	}
	if freshOK != goexitFresh791 {
		t.Errorf("celeris#791: after a /boom (%s), %d of %d fresh conns were answered within %v (%v)",
			mode, freshOK, goexitFresh791, goexitBudget791, freshOut)
	}
	if !stopped {
		t.Errorf("celeris#791: the server did not stop within %v of cancel after a /boom (%s)", goexitStop791, mode)
	}
}

func TestServerAsyncRouteGoexitOnEpoll(t *testing.T) {
	runServerAsyncAbort791(t, celeris.Epoll, true)
}

func TestServerAsyncRouteGoexitOnIOUring(t *testing.T) {
	runServerAsyncAbort791(t, celeris.IOUring, true)
}

// The controls: a panic on the same route is the router's to recover (a 500),
// on the base too.
func TestServerAsyncRoutePanicOnEpoll(t *testing.T) {
	runServerAsyncAbort791(t, celeris.Epoll, false)
}

func TestServerAsyncRoutePanicOnIOUring(t *testing.T) {
	runServerAsyncAbort791(t, celeris.IOUring, false)
}
