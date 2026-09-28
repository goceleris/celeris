//go:build linux && validation

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
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/recvtheft"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#715 hypothesis (a), the celeris#685 class, made deterministic.
//
// The io_uring worker prepares a recv SQE that reaches the kernel only at its
// next submit, and the SQE names the descriptor NUMBER (fixed files are off).
// finishClose / finishCloseDetached queue the recv's cancel behind it and close
// the descriptor at once. The claim under test: if a SIBLING worker's accept is
// given the freed number before this worker submits, the old recv reads the new
// connection's request (stale_recv_data_closed), and the new connection, whose
// own recv then finds an empty socket, is never answered.
//
// The theft needs one ordering: the sibling's accept after the close, and the
// closing worker's submit before the sibling's submit of its own recv. Two
// holds (internal/recvtheft, -tags=validation only) force it:
//   - W1, the worker that owns connection A, is parked right after closing A's
//     descriptor N with A's recv still unsubmitted (recvtheft.HoldAfterClose);
//   - W2, the other worker, is parked right after it accepted B as N and
//     prepared B's recv, before it submits (recvtheft.AfterAccept);
//   - W1 is released (it submits the old recv), then W2.
//
// A reaches "recv prepared, then closed in the same loop iteration" through the
// engine's own async path: A's first request is on an async route, so the
// inline parse bails, promoteConnToAsync starts the dispatch goroutine and
// re-arms A's recv; the request asks for Connection: close and the handler
// writes nothing, so the goroutine sets asyncClosed with nothing to flush and
// queues A; drainDetachQueue closes A (finishCloseDetached). The one test-only
// step is recvtheft.Options.PromoteGate: the worker waits after the re-arm
// until the goroutine has queued, so the close lands in the same iteration
// instead of racing it.
//
// Arms (one trial per test run; tally the --- lines of -count=N):
//   - A (TestRecvTheft715ArmA): the tree as it is. Asserts the property the
//     fix must restore: B is answered and no stale recv read B's bytes.
//     FAILS on main by design when hypothesis (a) holds. Not in CI; the
//     celeris#685 fix enables it.
//   - control (TestRecvTheft715Control): the same trial with
//     recvtheft.Options.SubmitBeforeClose, the close paths submitting before
//     they close (the fix direction). Asserts the same property.
//   - C (TestRecvTheft715ArmC): hypothesis (c), the promoted connection's
//     hand-off to its dispatch goroutine, with the window between the
//     worker's asyncInMu unlock and its Signal / goroutine start widened
//     (recvtheft.SetWakeHold). Asserts every request is answered.
//
// All three need two io_uring workers (so RLIMIT_MEMLOCK of at least 24 MiB)
// and run only with CELERIS_RECV_THEFT_715=1 in a -tags=validation build.

const envRecvTheft715 = "CELERIS_RECV_THEFT_715"

func requireRecvTheft715(t *testing.T) {
	t.Helper()
	if os.Getenv(envRecvTheft715) != "1" {
		t.Skipf("celeris#715 recv-theft measurement: set %s=1 to run (arm A fails on main by design)", envRecvTheft715)
	}
}

// recvTheftHandler answers every path with "ok" except /close, which writes
// nothing. /close and /async are async routes.
type recvTheftHandler struct{}

func (recvTheftHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil || s.Path == "/close" {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}
func (recvTheftHandler) RouteAsync(_, path string) bool { return path == "/close" || path == "/async" }
func (recvTheftHandler) HasAsyncRoutes() bool           { return true }

// startRecvTheftEngine runs an async-handler io_uring engine with two workers
// on a free loopback port and returns it with its port.
func startRecvTheftEngine(t *testing.T) (*Engine, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		Resources:     resource.Resources{Workers: 2},
		AsyncHandlers: true,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, recvTheftHandler{})
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
	for deadline := time.Now().Add(8 * time.Second); e.Addr() == nil; {
		select {
		case err := <-done:
			skipOrFail656(t, "iouring engine failed to start: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("engine did not start listening within 8s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	n := e.NumWorkers()
	t.Logf("RECVTHEFT715 engine workers=%d", n)
	if n < 2 {
		skipOrFail656(t, "celeris#715 needs a sibling io_uring worker: workers=%d (RLIMIT_MEMLOCK funds one per 12 MiB)", n)
	}
	return e, port
}

// fillFDHoles opens /dev/null until the kernel hands out a number above every
// descriptor this process has open, so the next descriptors are allocated at
// the top and a number freed later is the lowest free one. The fillers are
// closed when the test ends.
func fillFDHoles(t *testing.T) {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	top := -1
	for _, ent := range ents {
		if fd, err := strconv.Atoi(ent.Name()); err == nil && fd > top {
			top = fd
		}
	}
	var fillers []int
	for {
		fd, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatalf("open /dev/null: %v", err)
		}
		if fd > top {
			_ = unix.Close(fd)
			break
		}
		fillers = append(fillers, fd)
	}
	t.Cleanup(func() {
		for _, fd := range fillers {
			_ = unix.Close(fd)
		}
	})
}

// readHead reads from a blocking socket until the response head is complete,
// the peer closes, or d passes.
func readHead(fd int, d time.Duration) (string, error) {
	tv := unix.NsecToTimeval(int64(50 * time.Millisecond))
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	var got []byte
	buf := make([]byte, 4096)
	for deadline := time.Now().Add(d); time.Now().Before(deadline); {
		n, err := unix.Read(fd, buf)
		if n > 0 {
			got = append(got, buf[:n]...)
			if bytes.Contains(got, []byte("\r\n\r\n")) {
				return string(got), nil
			}
			continue
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return string(got), err
		}
		return string(got), io.EOF
	}
	return string(got), fmt.Errorf("no response head within %v", d)
}

// theftResult is one trial of arm A or the control.
//
// A hit is the sibling worker accepting one of B's candidates while the
// closer is parked after its close. On a tree that frees the number at the
// close, that accept is given the number (it is the lowest free one), which
// is the theft's precondition: reused is true, and the sibling is parked too.
// On a tree that keeps the number allocated until the owed recv has ended
// (celeris#685), the accept is given another number: reused is false, and B
// is served with no hold at all. heldOpen and released read the closer's
// descriptor through /proc/self/fd: whether the number still named A's
// socket while the closer was parked, and whether it was released after
// (closed, or reused by something else) within theftReleaseWait of the
// closer's release, so a kept descriptor is not a leak.
type theftResult struct {
	attempts      int
	hit           bool
	fd            int
	closer, sibl  int
	bFD           int
	reused        bool
	heldOpen      bool
	released      bool
	gate          bool
	witness       uint64
	staleClosed   uint64
	stale         []recvtheft.StaleRecv
	stolen        bool
	answered      bool
	answer        string
	answerErr     error
	missNoClose   int
	missQueued    int
	missOtherFD   int
	candidatesHit int
}

// fdTarget returns what /proc/self/fd/<fd> names ("socket:[inode]" for a
// socket), or "" when fd is not open.
func fdTarget(fd int) string {
	s, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return ""
	}
	return s
}

const (
	theftMaxAttempts  = 10
	theftCandidates   = 6
	theftAcceptWait   = 300 * time.Millisecond
	theftSubmitWait   = 300 * time.Millisecond
	theftAnswerWait   = 2 * time.Second
	theftReleaseWait  = 2 * time.Second
	theftClosePath    = "/close"
	theftRequestAHead = "GET " + theftClosePath + " HTTP/1.1\r\nHost: recv-theft-715\r\nConnection: close\r\n\r\n"
	theftRequestB     = "GET /b HTTP/1.1\r\nHost: recv-theft-715\r\n\r\n"
)

// runRecvTheftTrial drives attempts until the sibling worker accepts a fresh
// connection B as the number A's held close released (a hit), then releases
// the closer, then the sibling, and reports what happened to B's request.
func runRecvTheftTrial(t *testing.T, arm string, submitBeforeClose bool) theftResult {
	e, port := startRecvTheftEngine(t)
	sa := &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	var r theftResult
	for r.attempts < theftMaxAttempts {
		r.attempts++
		if done := recvTheftAttempt(t, e, sa, arm, submitBeforeClose, &r); done {
			return r
		}
	}
	return r
}

func recvTheftAttempt(t *testing.T, e *Engine, sa *unix.SockaddrInet4, arm string, submitBeforeClose bool, r *theftResult) bool {
	// The previous attempt's connections are closed by the engine as their
	// clients go; a server-side close after the fill below would open a hole
	// under the number this attempt frees, and the sibling's accept would take
	// the hole. So start from an engine with no connection.
	for deadline := time.Now().Add(3 * time.Second); e.metrics.activeConns.Load() != 0; {
		if time.Now().After(deadline) {
			t.Fatalf("engine still holds %d connections from the previous attempt", e.metrics.activeConns.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	fillFDHoles(t)
	// B's candidate sockets exist before A is accepted, so they hold numbers
	// below A's and none of them can take the number A's close frees.
	var cands []int
	defer func() {
		for _, fd := range cands {
			_ = unix.Close(fd)
		}
	}()
	for range theftCandidates {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatalf("socket: %v", err)
		}
		cands = append(cands, fd)
	}

	witness0 := recvtheft.CloseWithUnsubmittedRecv()
	stale0 := e.metrics.handoffLoss.staleRecvDataClosed.Load()
	tr := recvtheft.Arm(recvtheft.Options{
		SubmitBeforeClose: submitBeforeClose,
		PromoteGate:       2 * time.Second,
		HoldMax:           10 * time.Second,
	})
	defer tr.Disarm()

	a, err := net.DialTimeout("tcp", sockaddrString(sa), time.Second)
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	defer func() { _ = a.Close() }()
	if _, err := a.Write([]byte(theftRequestAHead)); err != nil {
		t.Fatalf("write A: %v", err)
	}
	ce, ok := tr.WaitClose(3 * time.Second)
	if !ok {
		r.missNoClose++
		t.Logf("RECVTHEFT715 arm=%s attempt=%d miss=no-close-hold gate=%v witness=+%d", arm, r.attempts, tr.GateUsed(),
			recvtheft.CloseWithUnsubmittedRecv()-witness0)
		return false
	}

	// W1 is parked right after its close path, with A's recv unsubmitted.
	// Did that close release N? Read it before any candidate is dialed, while
	// nothing else can take the number (every hole below it is filled).
	aTarget := fdTarget(ce.FD)
	r.heldOpen = strings.HasPrefix(aTarget, "socket:")

	// Dial B candidates until the sibling accepts one. One that hashes to
	// W1's listener waits in W1's accept queue. The sibling's accept is given
	// the lowest free number: N if the close released it, and then the
	// sibling is parked too (its recv for B prepared, not submitted); another
	// number if N is still A's, and then B is served with no hold.
	var hit *recvtheft.AcceptEvent
	var hitFD int
	for i, fd := range cands {
		if err := unix.Connect(fd, sa); err != nil {
			t.Fatalf("connect candidate %d: %v", i, err)
		}
		if _, err := unix.Write(fd, []byte(theftRequestB)); err != nil {
			t.Fatalf("write candidate %d: %v", i, err)
		}
		ev, ok := tr.NextAccept(theftAcceptWait)
		if !ok {
			r.missQueued++
			continue
		}
		if ev.Worker == ce.Worker {
			r.missOtherFD++
			t.Logf("RECVTHEFT715 arm=%s attempt=%d candidate=%d accepted by the closer %d as fd %d (target %d)", arm, r.attempts, i, ev.Worker, ev.FD, ce.FD)
			continue
		}
		ev0 := ev
		if ev.FD == ce.FD {
			held, ok := tr.WaitAcceptHeld(time.Second)
			if !ok {
				t.Fatalf("accept of fd %d by worker %d reported but its hold did not fire", ev.FD, ev.Worker)
			}
			ev0 = held
			r.reused = true
		}
		hit, hitFD = &ev0, fd
		r.candidatesHit = i + 1
		break
	}
	if hit == nil {
		t.Logf("RECVTHEFT715 arm=%s attempt=%d miss=no-sibling-accept fd=%d closer=%d queued=%d closer_accepts=%d",
			arm, r.attempts, ce.FD, ce.Worker, r.missQueued, r.missOtherFD)
		return false
	}
	r.hit, r.fd, r.closer, r.sibl, r.bFD, r.gate = true, ce.FD, ce.Worker, hit.Worker, hit.FD, tr.GateUsed()

	// Release W1: its next submit issues whatever it still holds for N.
	tr.ReleaseClose()
	for deadline := time.Now().Add(theftSubmitWait); time.Now().Before(deadline) && len(tr.Stale()) == 0; {
		time.Sleep(time.Millisecond)
	}
	// Then the sibling, if parked: it submits B's own recv.
	tr.ReleaseAccept()
	r.answer, r.answerErr = readHead(hitFD, theftAnswerWait)
	r.answered = strings.HasPrefix(r.answer, "HTTP/1.1 200")
	// A number kept open at the close must be released once A's recv has
	// ended: closed, or by now naming something else.
	for deadline := time.Now().Add(theftReleaseWait); ; {
		if cur := fdTarget(ce.FD); !r.heldOpen || cur != aTarget {
			r.released = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	r.witness = recvtheft.CloseWithUnsubmittedRecv() - witness0
	r.staleClosed = e.metrics.handoffLoss.staleRecvDataClosed.Load() - stale0
	r.stale = tr.Stale()
	for _, s := range r.stale {
		if s.FD == r.fd && int(s.Res) == len(theftRequestB) && len(s.Head) > 0 &&
			bytes.Equal(s.Head, []byte(theftRequestB)[:len(s.Head)]) {
			r.stolen = true
		}
	}
	return true
}

func logTheftResult(t *testing.T, arm string, r theftResult) {
	t.Helper()
	var heads []string
	for _, s := range r.stale {
		heads = append(heads, fmt.Sprintf("{worker=%d fd=%d gen=%d res=%d head=%q}", s.Worker, s.FD, s.Gen, s.Res, s.Head))
	}
	errText := "<nil>"
	if r.answerErr != nil {
		errText = r.answerErr.Error()
	}
	t.Logf("RECVTHEFT715 arm=%s result attempts=%d hit=%v fd=%d closer=%d sibling=%d b_fd=%d reused=%v held_open=%v released=%v gate=%v candidates=%d witness=+%d stale_recv_data_closed=+%d stolen=%v answered=%v answer_err=%q stale=[%s] misses{no_close=%d queued=%d closer_accepts=%d}",
		arm, r.attempts, r.hit, r.fd, r.closer, r.sibl, r.bFD, r.reused, r.heldOpen, r.released, r.gate, r.candidatesHit, r.witness, r.staleClosed, r.stolen, r.answered,
		errText, strings.Join(heads, " "), r.missNoClose, r.missQueued, r.missOtherFD)
}

func judgeTheftTrial(t *testing.T, arm string, r theftResult) {
	t.Helper()
	logTheftResult(t, arm, r)
	if !r.hit {
		skipOrFail656(t, "INCONCLUSIVE: no sibling accept while the closer was parked, in %d attempts", r.attempts)
	}
	if r.witness == 0 {
		t.Fatalf("the close hold fired but close_with_unsubmitted_recv did not move")
	}
	if r.stolen || !r.answered {
		t.Fatalf("B (fd %d on worker %d; A's number %d reused=%v, kept open at the close=%v) lost its request: stolen by the closed conn's recv=%v (stale_recv_data_closed +%d), answered=%v (%v)",
			r.bFD, r.sibl, r.fd, r.reused, r.heldOpen, r.stolen, r.staleClosed, r.answered, r.answerErr)
	}
	if !r.released {
		t.Fatalf("A's number %d, kept open at the close, still names A's socket %v after the closer's release: the kept descriptor leaked", r.fd, theftReleaseWait)
	}
}

// TestRecvTheft715ArmA is hypothesis (a) on the tree as it is. It asserts the
// property the celeris#685 close-path fix must restore, and fails on main by
// design when (a) holds: B's request read by A's unsubmitted recv.
func TestRecvTheft715ArmA(t *testing.T) {
	requireRecvTheft715(t)
	judgeTheftTrial(t, "A", runRecvTheftTrial(t, "A", false))
}

// TestRecvTheft715Control is arm A's trial with the close paths submitting the
// ring before they close (recvtheft.Options.SubmitBeforeClose): A's recv is
// issued while N still names A's socket. Predicted: B answered, nothing stolen.
func TestRecvTheft715Control(t *testing.T) {
	requireRecvTheft715(t)
	judgeTheftTrial(t, "control", runRecvTheftTrial(t, "control", true))
}

// TestRecvTheft715ArmC is hypothesis (c): a request the promoted connection's
// own recv read but that never reached its dispatch goroutine. The window
// between the worker's asyncInMu unlock and its Signal / goroutine start is
// widened to 2 ms on every hand-off (recvtheft.SetWakeHold) while keep-alive
// and pipelined requests hit an async route. Asserts every one is answered and
// that the hold actually ran.
func TestRecvTheft715ArmC(t *testing.T) {
	requireRecvTheft715(t)
	_, port := startRecvTheftEngine(t)
	recvtheft.SetWakeHold(2 * time.Millisecond)
	defer recvtheft.SetWakeHold(0)
	holds0 := recvtheft.WakeHolds()

	const conns, rounds, pipeline = 8, 20, 3
	req := "GET /async HTTP/1.1\r\nHost: recv-theft-715\r\n\r\n"
	var mu sync.Mutex
	var lost []string
	answered := 0
	var wg sync.WaitGroup
	for c := range conns {
		wg.Go(func() {
			conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
			if err != nil {
				mu.Lock()
				lost = append(lost, fmt.Sprintf("conn %d: dial: %v", c, err))
				mu.Unlock()
				return
			}
			defer func() { _ = conn.Close() }()
			buf := make([]byte, 0, 4096)
			tmp := make([]byte, 4096)
			for round := range rounds {
				n := 1
				if round%4 == 3 {
					n = pipeline
				}
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				if _, err := conn.Write([]byte(strings.Repeat(req, n))); err != nil {
					mu.Lock()
					lost = append(lost, fmt.Sprintf("conn %d round %d: write: %v", c, round, err))
					mu.Unlock()
					return
				}
				for got := 0; got < n; {
					if i := bytes.Index(buf, []byte("\r\n\r\nok")); i >= 0 {
						buf = buf[i+len("\r\n\r\nok"):]
						got++
						mu.Lock()
						answered++
						mu.Unlock()
						continue
					}
					k, err := conn.Read(tmp)
					if err != nil {
						mu.Lock()
						lost = append(lost, fmt.Sprintf("conn %d round %d: %d of %d answered: %v", c, round, got, n, err))
						mu.Unlock()
						return
					}
					buf = append(buf, tmp[:k]...)
				}
			}
		})
	}
	wg.Wait()
	holds := recvtheft.WakeHolds() - holds0
	want := conns * (rounds/4*(3+pipeline) + rounds%4)
	t.Logf("RECVTHEFT715 arm=C result conns=%d requests=%d answered=%d lost=%d wake_holds=+%d", conns, want, answered, len(lost), holds)
	for _, l := range lost {
		t.Logf("RECVTHEFT715 arm=C lost %s", l)
	}
	if holds == 0 {
		t.Fatal("the hand-off window hold never ran: the async hand-off was not exercised")
	}
	if len(lost) > 0 || answered != want {
		t.Fatalf("%d of %d requests answered with the hand-off window widened; lost: %v", answered, want, lost)
	}
}
