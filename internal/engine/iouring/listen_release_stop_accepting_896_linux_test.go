//go:build linux

package iouring

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#896 through the engine, on the route that does not take the drain's
// cancel: a worker that is still waiting for HTTP/2 stream handlers on the
// shared worker pool (h2PoolSettled), or whose send drain has run past
// shutdownSendDrainNanos, closes its listen socket in stopAccepting, at the
// start of that wait, through cancelAccept. The tests below keep an HTTP/2
// handler running, or a response unread, across Listen's context being
// cancelled, and take the oracle's one plain bind the instant Listen returns.
//
// Each runs twice. "natural" is the engine as it is. "sq-full" fills the SQ
// ring with no-ops at the moment cancelAccept runs (cancelAcceptSeam, on the
// worker's thread): the case the SQ ring has no room for the cancel, which
// the kernel does not make rare under load but a test cannot wait for. Both
// count the calls of cancelAccept, so a pass is not a shutdown that never took
// this route.

// holdHandler896 serves /hold from the shared worker pool once release is
// closed (it signals started when the handler runs), /big as a response too
// large for the socket buffers, and anything else at once.
type holdHandler896 struct {
	started chan struct{}
	release chan struct{}
	big     []byte // the /big response; nil unless the test serves it
}

func newHoldHandler896(bigBody bool) *holdHandler896 {
	h := &holdHandler896{started: make(chan struct{}, 4), release: make(chan struct{})}
	if bigBody {
		h.big = bytes.Repeat([]byte{'b'}, 32<<20)
	}
	return h
}

func (h *holdHandler896) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := []byte("ok")
	path := s.Path // an HTTP/1 request; an HTTP/2 stream carries it in :path
	for _, hh := range s.Headers {
		if hh[0] == ":path" {
			path = hh[1]
			break
		}
	}
	switch path {
	case "/hold":
		select {
		case h.started <- struct{}{}:
		default:
		}
		<-h.release
	case "/big":
		body = h.big
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(body))}}, body)
}
func (h *holdHandler896) RouteAsync(_, path string) bool { return path == "/hold" }
func (h *holdHandler896) HasAsyncRoutes() bool           { return true }

// startHold896 starts a standalone io_uring engine on addr with handler h and
// returns once it has published its address. A start that fails only on ring
// ENOMEM is retried, as start896's is.
func startHold896(t *testing.T, addr string, workers int, h stream.Handler) *engineRun896 {
	t.Helper()
	var run *engineRun896
	_, _, err := retryRingENOMEM(func() error {
		e, err := New(resource.Config{
			Addr:          addr,
			Engine:        engine.IOUring,
			Protocol:      engine.Auto,
			AsyncHandlers: true,
			Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
			Resources:     resource.Resources{Workers: workers},
		}, h)
		if err != nil {
			// New probes the tier with a ring of its own, which fails on
			// ENOMEM while the kernel has not yet uncharged the rings of the
			// engines this loop closed a few ms ago (ring_budget_linux_test.go):
			// that is waited out like a Listen that fails the same way.
			if ioUringUsableHere() {
				if _, small := memlockBelowRingBudgetFloor(); !small {
					return fmt.Errorf("New: %v: cannot allocate memory (ring memory not uncharged yet)", err)
				}
			}
			skipOrFail656(t, "New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- e.Listen(ctx) }()
		for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); {
			if e.Addr() != nil {
				run = &engineRun896{e: e, cancel: cancel, done: done}
				return nil
			}
			select {
			case lerr := <-done:
				cancel()
				return lerr
			case <-time.After(time.Millisecond):
			}
		}
		cancel()
		<-done
		t.Fatalf("engine on %s never published an address", addr)
		return nil
	})
	if err != nil {
		if ioUringUnavailable639(err) {
			skipUnlessIOUringUnusable(t, err)
		}
		t.Fatalf("engine start on %s: %v", addr, err)
	}
	return run
}

// h2Request896 opens an HTTP/2 connection to addr (prior knowledge) and sends
// GET path on stream 1, without reading any response.
func h2Request896(t *testing.T, addr, path string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatalf("write preface: %v", err)
	}
	fr := http2.NewFramer(c, c)
	if err := fr.WriteSettings(); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", path}, {":authority", "x"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		t.Fatalf("write headers: %v", err)
	}
	return c
}

// seam896 installs cancelAcceptSeam for one test: it counts the calls and,
// with full set, fills the SQ ring at each. The returned counter is read by
// the test's own goroutine only after the worker's call has been observed
// through it, atomically.
func seam896(t *testing.T, full bool) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	cancelAcceptSeam = func(w *Worker, _ int) {
		if full {
			for {
				sqe := w.ring.GetSQE()
				if sqe == nil {
					break
				}
				*(*uint8)(sqe) = opNOP
			}
		}
		calls.Add(1)
	}
	t.Cleanup(func() { cancelAcceptSeam = nil })
	return &calls
}

// waitCalls896 waits until the seam has been called more than before times.
func waitCalls896(t *testing.T, calls *atomic.Int64, before int64) {
	t.Helper()
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(time.Millisecond) {
		if calls.Load() > before {
			return
		}
	}
	t.Fatal("the shutdown never reached cancelAccept: the test is not exercising the stopAccepting route")
}

type iter896 func(t *testing.T, calls *atomic.Int64) (addr string, returned time.Time)

// loop896 runs iter n times and counts the iterations whose plain bind, made
// the instant Listen returned, was refused.
func loop896(t *testing.T, label string, n int, full bool, iter iter896) {
	t.Helper()
	calls := seam896(t, full)
	busy, never := 0, 0
	var slowest time.Duration
	for i := range n {
		addr, returned := iter(t, calls)
		if perr := plainBind896(addr); perr != nil {
			if !errors.Is(perr, unix.EADDRINUSE) {
				t.Fatalf("iteration %d: plain bind: %v, want nil or EADDRINUSE", i, perr)
			}
			busy++
			d := freeAfter896(addr, returned, time.Second)
			if d < 0 {
				never++
			}
			slowest = max(slowest, d)
		}
		// The kernel gives a closed engine's ring memory back 12 to 23 ms
		// later and an engine start holds about 1.6 MiB of the runner's 8 MiB
		// (ring_budget_linux_test.go): a loop this quick would have five or
		// six starts inside that window and run out, so it paces itself.
		time.Sleep(8 * time.Millisecond)
	}
	t.Logf("%s: %d shutdowns (cancelAccept called %d times), port still taken when Listen returned in %d (%.0f%%); "+
		"slowest to come free %v (never %d)", label, n, calls.Load(), busy, 100*float64(busy)/float64(n), slowest, never)
	if busy != 0 {
		t.Fatalf("%s: Listen returned with its port still taken in %d of %d shutdowns", label, busy, n)
	}
}

// h2Iteration896 is one engine with an HTTP/2 stream handler running when its
// context is cancelled; the handler is released once the shutdown has reached
// cancelAccept, and Listen returns after it.
func h2Iteration896(t *testing.T, calls *atomic.Int64) (string, time.Time) {
	addr := freeAddr639(t)
	h := newHoldHandler896(false)
	run := startHold896(t, addr, 2, h)
	c := h2Request896(t, addr, "/hold")
	defer func() { _ = c.Close() }()
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the /hold handler never ran")
	}
	before := calls.Load()
	run.cancel()
	waitCalls896(t, calls, before)
	close(h.release)
	returned, err := run.stopped()
	if err != nil {
		t.Fatalf("Listen returned %v after a cancel", err)
	}
	return addr, returned
}

// stopped waits for Listen, already cancelled, and returns the instant it did.
func (r *engineRun896) stopped() (time.Time, error) {
	err := <-r.done
	return time.Now(), err
}

// sendDrainIteration896 is one engine whose worker has a response in flight
// that its peer does not read when the context is cancelled, with the budget
// of an Engine.Shutdown that outlasts shutdownSendDrainNanos (without one the
// drain gives up at that mark and shuts down, never stopping the accept
// first). Past the mark the worker stops accepting; the peer then goes away
// and Listen returns.
func sendDrainIteration896(t *testing.T, calls *atomic.Int64) (string, time.Time) {
	addr := freeAddr639(t)
	h := newHoldHandler896(true)
	run := startHold896(t, addr, 2, h)
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("GET /big HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // the response is on its way, and does not fit the socket buffers
	budget, cancelBudget := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelBudget()
	_ = run.e.Shutdown(budget)
	before := calls.Load()
	run.cancel()
	waitCalls896(t, calls, before)
	_ = c.Close()
	returned, err := run.stopped()
	if err != nil {
		t.Fatalf("Listen returned %v after a cancel", err)
	}
	return addr, returned
}

// iterations896For is full under CELERIS_896_FULL=1, the runs that measure the
// failure rate over 200 shutdowns (100 for the send drain, which takes 0.3 s
// each), and ci otherwise. CI runs the same loops with fewer: the SQ-full arms
// fail 94% to 100% of their shutdowns without the fix, so 40 cannot all pass by
// chance, and the package's -race run has a 420 s timeout that 200 shutdowns of
// each of these would use up (they took 118 s of it).
func iterations896For(full, ci int) int {
	if os.Getenv("CELERIS_896_FULL") == "1" {
		return full
	}
	return ci
}

func TestListenReleasesThePortWithH2HandlersRunningNatural896(t *testing.T) {
	loop896(t, "h2 handler running, natural", iterations896For(iterations896, 40), false, h2Iteration896)
}

func TestListenReleasesThePortWithH2HandlersRunningSQFull896(t *testing.T) {
	loop896(t, "h2 handler running, SQ ring full", iterations896For(iterations896, 40), true, h2Iteration896)
}

func TestListenReleasesThePortAfterALongSendDrainNatural896(t *testing.T) {
	loop896(t, "send drain past 250 ms, natural", iterations896For(iterations896/2, 8), false, sendDrainIteration896)
}

func TestListenReleasesThePortAfterALongSendDrainSQFull896(t *testing.T) {
	loop896(t, "send drain past 250 ms, SQ ring full", iterations896For(iterations896/2, 16), true, sendDrainIteration896)
}
