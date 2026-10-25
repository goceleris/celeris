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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#1068 (and the TestStreamedResponseIsDelivered/*/async-route/close
// item of #890): an async-route response that ends its connection while its
// remainder is still queued was cut short, at the same byte count each time
// (the bytes the socket buffers took at the first flush: 2634119 of a
// 3 MiB body, 2681685 of an 8 MiB one), and the client saw EOF.
//
// serveAsync, after a request that ends the conn (Connection: close, an error,
// a refused write), declared the conn closed (asyncClosed) and only then took
// asyncInMu to mark its goroutine gone (endDispatch). A drainDetachQueue that
// read the conn between the two found a conn declared closed whose goroutine
// was still counted as running, took that for a close requested from outside
// while a handler runs, and called closeConn: the handler's detachMu was free
// (the goroutine had released it after its flush), so closeConn went through
// and closed the descriptor with the rest of the response unsent. The entry
// that made the loop look was the one the goroutine enqueued for the remainder
// of its partial flush, before it declared the close.
//
// The test holds the goroutine in that gap, with the response's remainder
// queued, and waits for the loop to look. With the gap closed the loop cannot
// look until the goroutine has finished (the wait ends on its timeout), and
// sends the remainder before it closes.

func TestAsyncRouteCloseSendsTheQueuedRemainder1068(t *testing.T) {
	for _, tc := range []struct {
		name string
		// reqs requests are sent in one packet, header the lines after Host.
		reqs   int
		header string
		body   int
		// closesAtBoundary: the conn ends after whole responses only (the
		// back-pressure refusal of celeris#761 drops the responses past the
		// cap), so any number of them is right, but each one that is read
		// must be whole, and then the stream ends (TestBackloggedPeerIsClosed
		// takes an EOF in the next response's head as that end too).
		closesAtBoundary bool
	}{
		// TestStreamedResponseIsDelivered/*/async-route/close: one response
		// larger than the socket buffers, then Connection: close.
		{"connection-close", 1, "Connection: close\r\n", 8 << 20, false},
		// TestBackloggedPeerIsClosed/*/async-*: four pipelined 3 MiB responses
		// for a client that reads nothing; the cap refuses one past 4 MiB.
		{"refused-write", 4, "", 3 << 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				looked   = make(chan struct{})
				lookOnce sync.Once
				held     = make(chan struct{})
				heldOnce sync.Once
			)
			var queued atomic.Int64
			exitHook := func(cs *connState) {
				queued.Store(int64(queued1068(cs)))
				select {
				case <-looked:
				case <-time.After(500 * time.Millisecond):
				}
				heldOnce.Do(func() { close(held) })
			}
			drainHook := func(*connState) { lookOnce.Do(func() { close(looked) }) }
			testHookAsyncExitClosed.Store(&exitHook)
			testHookDrainAsyncClosed.Store(&drainHook)
			t.Cleanup(func() {
				testHookAsyncExitClosed.Store(nil)
				testHookDrainAsyncClosed.Store(nil)
			})

			_, addr := startE2E885(t, true, newPathHandler885(tc.body), func(*resource.Config) {})
			c, err := net.DialTimeout("tcp", addr, 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if _, err := io.WriteString(c, strings.Repeat("GET /big HTTP/1.1\r\nHost: x\r\n"+tc.header+"\r\n", tc.reqs)); err != nil {
				t.Fatal(err)
			}
			// The client reads nothing until the goroutine has been held and
			// let go, so the response cannot fit the socket buffers and its
			// remainder is queued at every point of the hold.
			select {
			case <-held:
			case <-time.After(10 * time.Second):
				t.Fatal("the dispatch goroutine never reached the exit of a request that ends its conn")
			}
			// The arm proves nothing unless the remainder was queued while the
			// goroutine was held (an 8 MiB response must outgrow the socket
			// buffers of the host it runs on).
			if q := queued.Load(); q <= 0 {
				t.Fatalf("%d bytes were queued while the goroutine was held (-1: not readable): the arm would prove nothing", q)
			}
			_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
			br := bufio.NewReaderSize(c, 64<<10)
			whole := 0
			for {
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					if whole > 0 && tc.closesAtBoundary && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
						break
					}
					t.Fatalf("response %d: %v (looked=%v)", whole+1, err, isClosed1068(looked))
				}
				n, err := io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if err != nil || n != int64(tc.body) {
					t.Fatalf("response %d: received %d of %d body bytes (%v): the loop closed the conn with the rest of the response unsent while the goroutine was still on its way out (looked=%v)",
						whole+1, n, tc.body, err, isClosed1068(looked))
				}
				whole++
				if !tc.closesAtBoundary || whole == tc.reqs {
					break
				}
			}
			if tc.closesAtBoundary && whole == tc.reqs {
				t.Fatalf("all %d responses were sent to a client that read nothing: the cap did not refuse one", tc.reqs)
			}
		})
	}
}

func isClosed1068(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// queued1068 is the bytes cs has queued for its socket, read under detachMu
// with TryLock only: the caller may hold asyncInMu, and the loop may hold
// detachMu for a flush. -1 if the lock stayed busy for 100 ms.
func queued1068(cs *connState) int {
	for dl := time.Now().Add(100 * time.Millisecond); time.Now().Before(dl); time.Sleep(time.Millisecond) {
		if cs.detachMu.TryLock() {
			n := csPendingBytes(cs)
			cs.detachMu.Unlock()
			return n
		}
	}
	return -1
}

// abortHandler1068: /big waits for proceed, then answers with a body larger
// than the socket buffers; /die panics, past every recovery (this handler is
// the stream.Handler itself).
type abortHandler1068 struct {
	big     []byte
	entered chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (h *abortHandler1068) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	if s.Path == "/die" {
		panic("celeris1068: handler panic")
	}
	h.once.Do(func() { close(h.entered) })
	<-h.proceed
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "application/octet-stream"}, {"content-length", strconv.Itoa(len(h.big))}}, h.big)
}
func (*abortHandler1068) RouteAsync(_, _ string) bool { return true }
func (*abortHandler1068) HasAsyncRoutes() bool        { return true }

var _ stream.AsyncRouteResolver = (*abortHandler1068)(nil)

// TestAbortedGoroutineDoesNotLoseTheQueuedRemainder1068: the same gap as above
// on the other way a dispatch goroutine ends a conn. abortAsyncHandler stores
// asyncClosed first, then lets go of detachMu and logs the panic with its
// stack, and only then calls endDispatch. A pipelining client's first response
// is partly sent and its remainder queued (the entry the goroutine enqueued
// for it is still in the detach queue) when the second request's handler
// panics; the loop that drains that entry in the window found a closed conn
// whose goroutine was running and closed it over the remainder.
//
// The second request is on the wire while the first handler still runs, so
// the goroutine takes it straight after the first flush without parking. The
// test holds the abort after its log and waits for the loop to look.
func TestAbortedGoroutineDoesNotLoseTheQueuedRemainder1068(t *testing.T) {
	abortedGoroutine1068(t, false)
}

// TestAbortedGoroutineFinishingBetweenTheDrainsReads1068: the loop decides
// whether the goroutine still runs, and whether to leave the close to it, in
// one asyncInMu section. Read in two (asyncRun, then dispatchBusy), a goroutine
// that reached endDispatch between the reads was taken for gone by the second
// and the conn was closed with closeConn, over the queued remainder, and the
// goroutine's own entry found it closed. The drain hook here holds the loop
// after its decision until the goroutine has finished.
func TestAbortedGoroutineFinishingBetweenTheDrainsReads1068(t *testing.T) {
	abortedGoroutine1068(t, true)
}

// abortedGoroutine1068 runs the scenario of the two tests above. The abort
// hook queues an entry for the loop itself, to force the window: the entry
// for the remainder is normally drained before the second request's handler
// has panicked. waitGone holds the loop's drain, after its decision, until the
// aborted goroutine has left (asyncRun false).
func abortedGoroutine1068(t *testing.T, waitGone bool) {
	t.Helper()
	// What stays queued (the body less what the socket takes, about 2.4 MB
	// here) must be under the 4 MiB cap, or the second request is refused
	// before its handler runs (ErrWriteBacklog) and the abort is never reached;
	// and more than the kernel takes from the loop while it decides.
	const body = 5 << 20
	var (
		looked   = make(chan struct{})
		lookOnce sync.Once
		held     = make(chan struct{})
		heldOnce sync.Once
		queued   atomic.Int64
	)
	// The loop's look counts only once the goroutine is held: an earlier
	// drain of the entry for the remainder finds the goroutine still holding
	// detachMu (it has not released it before it logs), and leaves the close
	// to it, as it always did.
	var inWindow atomic.Bool
	drainHook := func(cs *connState) {
		if !inWindow.Load() {
			return
		}
		lookOnce.Do(func() { close(looked) })
		if !waitGone {
			return
		}
		for dl := time.Now().Add(2 * time.Second); time.Now().Before(dl); time.Sleep(time.Millisecond) {
			cs.asyncInMu.Lock()
			running := cs.asyncRun
			cs.asyncInMu.Unlock()
			if !running {
				return
			}
		}
	}
	abortHook := func(l *Loop, cs *connState) {
		queued.Store(int64(queued1068(cs)))
		inWindow.Store(true)
		// An entry naming cs reaches the loop while the goroutine is in the
		// window. (The one enqueued for the remainder is normally drained
		// before the handler of the second request has panicked; one that is
		// drained late, or another, is all the loop needs to look.)
		l.enqueueDetach(cs)
		select {
		case <-looked:
		case <-time.After(500 * time.Millisecond):
		}
		heldOnce.Do(func() { close(held) })
	}
	testHookAbortAsyncLogged.Store(&abortHook)
	testHookDrainAsyncClosed.Store(&drainHook)
	t.Cleanup(func() {
		testHookAbortAsyncLogged.Store(nil)
		testHookDrainAsyncClosed.Store(nil)
	})

	h := &abortHandler1068{big: newPathHandler885(body).big, entered: make(chan struct{}), proceed: make(chan struct{})}
	e, addr := startE2E885(t, true, h, func(*resource.Config) {})
	// A small receive buffer keeps the kernel from taking the remainder on
	// its own (the send buffer grows with the peer's window) while the loop
	// is deciding.
	c := dialRcvBuf885(t, addr, 16<<10)
	req1 := "GET /big HTTP/1.1\r\nHost: x\r\n\r\n"
	req2 := "GET /die HTTP/1.1\r\nHost: x\r\n\r\n"
	if _, err := io.WriteString(c, req1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first handler never ran")
	}
	if _, err := io.WriteString(c, req2); err != nil {
		t.Fatal(err)
	}
	// The loop has read the second request (into the goroutine's input) once
	// the engine has counted both.
	for dl := time.Now().Add(10 * time.Second); e.Metrics().BytesRead < uint64(len(req1)+len(req2)); time.Sleep(time.Millisecond) {
		if time.Now().After(dl) {
			t.Fatalf("the engine read %d of %d request bytes", e.Metrics().BytesRead, len(req1)+len(req2))
		}
	}
	close(h.proceed)
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the dispatch goroutine never aborted")
	}
	// The abort hook waits only so long for the loop; a loop that never looked
	// leaves nothing tested.
	if !isClosed1068(looked) {
		t.Fatal("the loop never drained the closed conn while the aborted goroutine was held: the test would prove nothing")
	}
	if q := queued.Load(); q <= 0 {
		t.Fatalf("%d bytes were queued while the aborted goroutine was held (-1: not readable): the test would prove nothing", q)
	}
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	br := bufio.NewReaderSize(c, 64<<10)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("response 1: %v (looked=%v)", err, isClosed1068(looked))
	}
	n, err := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil || n != body {
		t.Fatalf("response 1: received %d of %d body bytes (%v): the loop closed the conn over the remainder while the aborted goroutine was still on its way out (looked=%v)",
			n, body, err, isClosed1068(looked))
	}
}
