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
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/drainclock"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#1244. The contract of the first 250 ms of the send drain (celeris#595,
// #806, #1169), pinned with the drain's clock held: a worker that has a response
// going out serves on for the floor, listener open, so
//
//   - a conn the kernel has held back (TCP_DEFER_ACCEPT) and the worker has not
//     accepted, whose first request arrives after the shutdown began, is
//     accepted and answered, with Connection: close;
//   - a conn the worker has accepted and heard nothing from, likewise, and its
//     idle and silent neighbours stay open until the floor is over, and get their
//     FIN then.
//
// test/server's A-deferred-draining and A-accepted-draining pin the same through
// Server.Shutdown, with a time.Sleep(50 ms) for "a little into the drain": a
// test goroutine starved past the floor (250 ms) sent into a closed listener and
// was reset (a reset 287 ms after the drain began on main, 1 run in 20 to 1 in
// 100 with CPU hogs on one CPU). Here the floor cannot pass on its own:
// the drain reads its clock from drainclock (celeris#1176), and the clock is
// held at the instant of the cancel until the test moves it. The request goes in
// when every worker has finished its first pass of the drain (sendDrainSeam: the
// pass's snapshot and the floor's start are taken by then), however long the
// machine took to get there, and what it is answered with does not depend on the
// time it took.

// heldClock1244 is the drain's clock for these tests: the real monotonic clock
// until freeze, and then the instant it was frozen at, plus what advance added.
type heldClock1244 struct {
	epoch time.Time
	held  atomic.Int64 // 0: running
}

func (c *heldClock1244) mono() int64 { return int64(time.Since(c.epoch)) + 1 }

func (c *heldClock1244) read() drainclock.Reading {
	m := c.held.Load()
	if m == 0 {
		m = c.mono()
	}
	return drainclock.Reading{Wall: time.Now().UnixNano(), Mono: m}
}

func (c *heldClock1244) freeze()                 { c.held.Store(c.mono()) }
func (c *heldClock1244) advance(d time.Duration) { c.held.Add(int64(d)) }
func (c *heldClock1244) thaw()                   { c.held.Store(0) }

// bigCount1244 is a handler that serves /big (the response nobody reads) and
// counts the requests for it, on top of holdHandler896's /ping.
type bigCount1244 struct {
	*holdHandler896
	big atomic.Int64
}

func (h *bigCount1244) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.Path == "/big" {
		h.big.Add(1)
	}
	return h.holdHandler896.HandleStream(ctx, s)
}

// startDraining1244 starts an HTTP/1 io_uring engine on addr with h. A start
// that fails only on ring ENOMEM is retried, as start896's is.
func startDraining1244(t *testing.T, addr string, workers int, disableDefer bool, h stream.Handler) *engineRun896 {
	t.Helper()
	var run *engineRun896
	_, _, err := retryRingENOMEM(func() error {
		e, err := New(resource.Config{
			Addr:               addr,
			Engine:             engine.IOUring,
			Protocol:           engine.HTTP1,
			DisableDeferAccept: disableDefer,
			Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
			Resources:          resource.Resources{Workers: workers},
		}, h)
		if err != nil {
			skipOrFail656(t, "New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done, returned := listenAsync896(ctx, e)
		for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); {
			if e.Addr() != nil {
				run = &engineRun896{e: e, cancel: cancel, done: done, returned: returned}
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
	run.reapAtCleanup(t, nil)
	return run
}

// drainRig1244 is a started engine with stalled responses on its workers, a
// clock that can be held, and a seam that says when every worker is past the
// first pass of its drain.
type drainRig1244 struct {
	t       *testing.T
	run     *engineRun896
	addr    string
	clk     *heldClock1244
	ready   chan struct{} // closed when every worker has made two passes of the drain
	pending map[*Worker]bool
	mu      sync.Mutex
	stall   []net.Conn
}

const stalled1244 = 16

func newDrainRig1244(t *testing.T, disableDefer bool) *drainRig1244 {
	t.Helper()
	r := &drainRig1244{t: t, ready: make(chan struct{}), pending: map[*Worker]bool{}}
	r.clk = &heldClock1244{epoch: time.Now()}

	// Restored last: after the engine has been stopped (its reap is registered
	// by the start below), and the seams are process-wide.
	restoreClock := drainclock.SetSeam(r.clk.read)
	t.Cleanup(restoreClock)

	var (
		nW     atomic.Int64 // set once the engine has started
		passes = map[*Worker]int{}
		once   sync.Once
	)
	hook := func(w *Worker) {
		r.mu.Lock()
		passes[w]++
		if passes[w] == 1 {
			r.pending[w] = w.hasPendingSends()
		}
		all := int64(len(passes)) >= nW.Load()
		for _, n := range passes {
			all = all && n >= 2
		}
		r.mu.Unlock()
		if all {
			once.Do(func() { close(r.ready) })
		}
	}
	sendDrainSeam.Store(&hook)
	t.Cleanup(func() { sendDrainSeam.Store(nil) })

	h := &bigCount1244{holdHandler896: newHoldHandler896(true)}
	r.addr = freeAddr639(t)
	r.run = startDraining1244(t, r.addr, 2, disableDefer, h)
	nW.Store(int64(len(r.run.e.workers)))

	// Registered after the start, so it runs before the engine's reap: the
	// drain ends when the clock runs again and the stalled conns are gone.
	t.Cleanup(func() {
		r.clk.thaw()
		for _, c := range r.stall {
			_ = c.Close()
		}
	})

	// Stalled responses: 16 clients with a 4 KiB receive buffer ask for 32 MiB
	// and read nothing, so that every worker has a send pending (it holds one
	// with p = 1 - 2^-16).
	d := net.Dialer{Control: func(_, _ string, rc syscall.RawConn) error {
		return rc.Control(func(fd uintptr) { _ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096) })
	}}
	for range stalled1244 {
		c, err := d.Dial("tcp", r.addr)
		if err != nil {
			t.Fatalf("dial a stalled client: %v", err)
		}
		r.stall = append(r.stall, c)
		if _, err := io.WriteString(c, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			t.Fatalf("write a stalled client's request: %v", err)
		}
	}
	// A wait for progress, not a bound on speed: the instrumented worker under
	// CPU hogs on one CPU serves these at a few a second.
	for dl := time.Now().Add(120 * time.Second); h.big.Load() < stalled1244; time.Sleep(time.Millisecond) {
		if time.Now().After(dl) {
			t.Fatalf("%d of %d stalled requests served after 120 s", h.big.Load(), stalled1244)
		}
	}
	return r
}

func (r *drainRig1244) dial() net.Conn {
	r.t.Helper()
	c, err := net.DialTimeout("tcp", r.addr, 2*time.Second)
	if err != nil {
		r.t.Fatalf("dial: %v", err)
	}
	r.t.Cleanup(func() { _ = c.Close() })
	return c
}

// beginDrain holds the clock at this instant, cancels Listen's context and
// waits until every worker is past the first pass of its drain. The floor does
// not run out meanwhile, whatever it takes.
func (r *drainRig1244) beginDrain() {
	r.t.Helper()
	r.clk.freeze()
	r.run.cancel()
	// A worker with no send pending exits at its first pass and never makes a
	// second, so the wait looks for it too, and says so.
	for dl := time.Now().Add(60 * time.Second); ; time.Sleep(time.Millisecond) {
		r.checkPremise()
		select {
		case <-r.ready:
			return
		default:
		}
		if time.Now().After(dl) {
			r.t.Fatal("the workers were not both in their drain 60 s after the cancel")
		}
	}
}

// checkPremise ends the test if a worker had no send pending at its first pass
// of the drain: p = 2^-stalled1244 per worker, and then the cell has nothing to
// say of a worker that has a response going out.
func (r *drainRig1244) checkPremise() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for w, p := range r.pending {
		if !p {
			r.t.Fatalf("worker %d had no send pending when its drain began: the premise of the cell is not met (p = 2^-%d)", w.id, stalled1244)
		}
	}
}

// waitConns returns once the engine counts n open conns (the stalled ones
// included), so that every conn a cell dialed is in a worker's table: a conn
// still in the accept queue when the floor is over would get a reset, not a FIN.
func (r *drainRig1244) waitConns(n int64) {
	r.t.Helper()
	for dl := time.Now().Add(60 * time.Second); r.run.e.Metrics().ActiveConnections < n; time.Sleep(time.Millisecond) {
		if time.Now().After(dl) {
			r.t.Fatalf("the engine counts %d conns after 60 s, want %d", r.run.e.Metrics().ActiveConnections, n)
		}
	}
}

const (
	get1244 = "GET /ping HTTP/1.1\r\nHost: x\r\n\r\n"
	// floor1244 is shutdownSendDrainNanos.
	floor1244 = time.Duration(shutdownSendDrainNanos)
)

// answered1244 reads the response to a first request and returns its body.
func answered1244(t *testing.T, c net.Conn, what string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("%s: a first request inside the floor of the send drain, with a send pending: %v, want it answered with Connection: close", what, err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "ok" || !resp.Close {
		t.Errorf("%s: status %d body %q Connection: close %v, want 200, \"ok\" and close", what, resp.StatusCode, body, resp.Close)
	}
}

// fin1244 reads c until it fails, within d, and returns how: nil is a FIN.
func fin1244(c net.Conn, d time.Duration) error {
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, err := c.Read(make([]byte, 1))
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func isTimeout1244(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// TestDrainingFloorAnswersADeferredConn1244 is test/server's A-deferred-draining
// with the floor held: the first request of a conn the kernel held back, sent
// when every worker is past the first pass of its drain, is accepted and
// answered, with Connection: close, whatever the machine's speed.
func TestDrainingFloorAnswersADeferredConn1244(t *testing.T) {
	r := newDrainRig1244(t, false)
	d := r.dial() // TCP_DEFER_ACCEPT keeps it out of the worker
	r.beginDrain()
	if _, err := io.WriteString(d, get1244); err != nil {
		t.Fatal(err)
	}
	answered1244(t, d, "deferred")
}

// TestDrainingFloorAnswersAnAcceptedConn1244 is A-accepted-draining with the
// floor held: the first request of an accepted conn is answered with
// Connection: close, and the silent and the idle conns beside it are open until
// the floor is over, and get their FIN once it is.
func TestDrainingFloorAnswersAnAcceptedConn1244(t *testing.T) {
	r := newDrainRig1244(t, true)
	silent, idle, getter := r.dial(), r.dial(), r.dial()
	_ = idle.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(idle, get1244); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(idle), nil)
	if err != nil || resp.Close {
		t.Fatalf("the keep-alive conn before the shutdown: %v (close=%v)", err, resp != nil && resp.Close)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2))
	r.waitConns(stalled1244 + 3)

	r.beginDrain()
	if _, err := io.WriteString(getter, get1244); err != nil {
		t.Fatal(err)
	}
	answered1244(t, getter, "accepted")

	// The clock is held, so the floor is not over: the silent and the idle conn
	// are open however long the real time. A real wait cannot flake: it asserts
	// that nothing happens.
	for name, c := range map[string]net.Conn{"silent": silent, "idle": idle} {
		if err := fin1244(c, 300*time.Millisecond); !isTimeout1244(err) {
			t.Errorf("the %s conn read %v inside the floor of the drain (clock held), want it open", name, err)
		}
	}
	r.clk.advance(floor1244 + time.Millisecond)
	for name, c := range map[string]net.Conn{"silent": silent, "idle": idle} {
		if err := fin1244(c, 30*time.Second); err != nil {
			t.Errorf("the %s conn read %v once the floor was over, want a FIN", name, err)
		}
	}
}
