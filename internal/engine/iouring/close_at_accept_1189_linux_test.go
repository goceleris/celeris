//go:build linux

package iouring

import (
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// A connection whose header timer finds its budget already spent when it is armed
// at the accept is closed there, and nothing may then be armed on it
// (celeris#1189, review of #1193). The budget is taken from the CQE batch's clock,
// so it is spent at the accept when the batch ran for longer than ReadHeaderTimeout
// before it reached the accept: an OnConnect hook, or a slow inline handler of an
// earlier CQE of the batch. Here OnConnect takes 300 ms against a ReadHeaderTimeout
// of 200 ms. A recv armed on the closed connection is a kernel op on a released
// descriptor number and on a connState that is queued for release (celeris#685,
// celeris#256), and nothing accounts for it: the connState is held by the 5 s
// pendingRelease backstop, which then releases it with a "kernel ops unaccounted"
// warning. The check is the sweep's own, so it does not wait for the backstop: no
// connState of pendingRelease may have an op in flight and no closedOps entry that
// will account for its completion.
func TestConnClosedByTheArmAtAcceptGetsNoRecv1189(t *testing.T) {
	var (
		mu      sync.Mutex
		addr    string
		looked  int
		orphans int
	)
	hook := func(w *Worker) {
		mu.Lock()
		defer mu.Unlock()
		// The hook is the package's, not the engine's: workers of an earlier test may
		// still be winding down.
		if addr == "" || w.cfg.Addr != addr {
			return
		}
		for i := range w.pendingRelease {
			cs := w.pendingRelease[i].cs
			looked++
			if cs.kernelInflight > 0 && w.closedOps[encodeConnOpKey(cs.fd, cs.generation)] == nil {
				orphans++
			}
		}
	}
	testHookCheckTimeouts.Store(&hook)
	t.Cleanup(func() { testHookCheckTimeouts.Store(nil) })
	logs := &syncBuf806{}
	var (
		first        atomic.Bool
		connReturned atomic.Pointer[time.Time] // when the slow OnConnect returned
		closedAt     atomic.Pointer[time.Time] // when the first connection's OnDisconnect ran
	)
	r := startEngine806(t, loadHandler806{}, func(c *resource.Config) {
		c.Protocol = engine.Auto
		c.ReadHeaderTimeout = 200 * time.Millisecond
		c.DisableDeferAccept = true
		c.Logger = slog.New(slog.NewTextHandler(io.Writer(logs), &slog.HandlerOptions{Level: slog.LevelWarn}))
		c.OnConnect = func(string) {
			if first.CompareAndSwap(false, true) {
				time.Sleep(300 * time.Millisecond)
				now := time.Now()
				connReturned.Store(&now)
			}
		}
		c.OnDisconnect = func(string) {
			now := time.Now()
			closedAt.CompareAndSwap(nil, &now)
		}
	})
	mu.Lock()
	addr = r.addr
	mu.Unlock()

	c, err := net.DialTimeout("tcp", r.addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the connection whose budget was spent at the accept: read = %v, want it closed (EOF)", err)
	}

	// The scenario happened: the arm closed the connection at the accept, as soon as
	// OnConnect returned, not at a timer or a sweep after it. Without this the test
	// passes when the OnConnect did not outlast the budget (nothing is closed at the
	// accept), or when a timer or the sweep closed the connection, which have nothing
	// to leave behind. The close is within microseconds of the return when it is the
	// arm's (measured 6 to 75 microseconds, under -race too); a timer
	// would be ReadHeaderTimeout (200 ms) after the accept, which is 200 ms after the return, and the sweep's 100 ms or more.
	ret, closed := connReturned.Load(), closedAt.Load()
	if ret == nil || closed == nil {
		t.Fatalf("OnConnect returned: %v, OnDisconnect ran: %v: the scenario did not occur", ret != nil, closed != nil)
	}
	if gap := closed.Sub(*ret); gap < 0 || gap > 50*time.Millisecond {
		t.Fatalf("the connection was closed %v after its slow OnConnect returned, want within 50ms: it was not the arm at the accept that closed it", gap)
	}
	t.Logf("closed %v after OnConnect returned", closed.Sub(*ret))

	// A healthy connection is served.
	c2, err := net.DialTimeout("tcp", r.addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	_ = c2.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c2, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 12)
	if _, err := io.ReadFull(c2, buf); err != nil || !strings.HasPrefix(string(buf), "HTTP/1.1 200") {
		t.Fatalf("the healthy connection was not served: %q, %v", buf, err)
	}

	// The sweeps come within about 70, 250 and 430 ms of the activity of an idle
	// worker (its idle backoff starts again), so a second is several looks; the 5 s
	// backstop is not waited for.
	time.Sleep(time.Second)
	mu.Lock()
	defer mu.Unlock()
	t.Logf("sweeps looked at a connState of pendingRelease %d times", looked)
	if orphans > 0 {
		t.Errorf("a connState queued for release had a kernel op in flight with no closedOps entry in %d of %d looks (a recv armed on a closed connection)", orphans, looked)
	}
	if l := logs.String(); strings.Contains(l, "unaccounted") {
		t.Errorf("warning logged: %s", l)
	}
}
