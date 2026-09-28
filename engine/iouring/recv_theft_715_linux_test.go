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
//   - A with a hole (TestRecvTheft715ArmAHole): arm A with one of B's
//     candidate sockets, whose number is below A's, closed once the closer is
//     parked, so a free number lies under the one A's close frees. The
//     sibling's first accept is given the hole; it tests nothing (see
//     theftHit) and is skipped, and the next one is given A's number if the
//     close released it. Pins that the verdict cannot pass a tree without
//     the fix because a hole took B (celeris#793 review round 2).
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

// waitEngineQuiet waits, before an attempt fills the descriptor holes, until
// the engine has accepted every connection the trial has dialed (dialed, since
// acceptCount read accepts0) and holds none of them. A candidate that a missed
// attempt left in the closer's accept queue is accepted only once the closer
// is released; its close after the fill opened a hole under the next
// attempt's number, and the holes cascaded into further misses (the round-2
// smoke on the negative control: 4 such accepts skipped in one attempt).
// Holes cannot make a trial pass (theftHit), so a late accept past the
// deadline is logged, not fatal; a connection still open is fatal as before.
func waitEngineQuiet(t *testing.T, e *Engine, accepts0 uint64, dialed int) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; {
		acc := e.metrics.acceptCount.Load() - accepts0
		act := e.metrics.activeConns.Load()
		if acc >= uint64(dialed) && act == 0 {
			return
		}
		if time.Now().After(deadline) {
			if act != 0 {
				t.Fatalf("engine still holds %d connections from the previous attempt", act)
			}
			t.Logf("RECVTHEFT the engine accepted %d of the %d connections dialed so far; going on", acc, dialed)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
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

// theftResult is one trial of arm A, A with a hole, or the control.
//
// A hit is a sibling accept of one of B's candidates, while the closer is
// parked after its close, that tests the theft (theftHit). On a tree that
// frees the number at the close, that is the accept given the number: reused
// is true, and the sibling is parked too, with B's recv prepared. On a tree
// that keeps the number allocated until the owed recv has ended
// (celeris#685), heldByA is true, the accept is given another number (reused
// is false), and B is served with no hold at all. A sibling accept given
// another number while A's number was free filled a lower hole: it is
// skipped (missHole) and the next candidate is dialed. heldOpen, heldByA and
// released read the closer's descriptor: whether the number still named a
// socket while the closer was parked, whether that socket was A's (its peer
// is A's local address), and whether it was released after (closed, or
// reused by something else) within theftReleaseWait of the closer's release,
// so a kept descriptor is not a leak.
type theftResult struct {
	attempts      int
	hit           bool
	fd            int
	closer, sibl  int
	bFD           int
	reused        bool
	heldOpen      bool
	heldByA       bool
	released      bool
	hole          int
	missHole      int
	accepts0      uint64
	dialed        int
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

// theftArm is one arm of the trial: its name in the result line, whether the
// close paths submit before they close (the control), and whether a hole is
// opened under A's number once the closer is parked (A with a hole).
type theftArm struct {
	name              string
	submitBeforeClose bool
	hole              bool
}

// theftHit reports whether the sibling's accept of a candidate as number
// accepted tests the theft of A's number n. It does when the accept was given
// n (the close released it, and B's recv is the one a stale recv can rob), or
// when n was still A's socket while the closer was parked (the close kept it,
// so no accept could be given it, and B must be served). An accept given
// another number while n was free tests nothing: it filled a hole below n,
// which is still free, so B is served with no theft possible on any tree.
// Counting it as a hit passed a tree without the fix (celeris#793 review
// round 2: 1 of 101 trials, b_fd=13 under fd=27). The caller skips it and
// dials the next candidate, whose accept can be given n.
func theftHit(accepted, n int, heldByA bool) bool {
	return accepted == n || heldByA
}

// runRecvTheftTrial drives attempts until the sibling worker's accept of a
// fresh connection B tests the theft (theftHit), then releases the closer,
// then the sibling, and reports what happened to B's request.
func runRecvTheftTrial(t *testing.T, arm theftArm) theftResult {
	e, port := startRecvTheftEngine(t)
	sa := &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	r := theftResult{hole: -1, accepts0: e.metrics.acceptCount.Load()}
	for r.attempts < theftMaxAttempts {
		r.attempts++
		if done := recvTheftAttempt(t, e, sa, arm, &r); done {
			return r
		}
	}
	return r
}

func recvTheftAttempt(t *testing.T, e *Engine, sa *unix.SockaddrInet4, arm theftArm, r *theftResult) bool {
	waitEngineQuiet(t, e, r.accepts0, r.dialed)
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
		SubmitBeforeClose: arm.submitBeforeClose,
		PromoteGate:       2 * time.Second,
		HoldMax:           10 * time.Second,
	})
	defer tr.Disarm()

	a, err := net.DialTimeout("tcp", sockaddrString(sa), time.Second)
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	r.dialed++
	defer func() { _ = a.Close() }()
	if _, err := a.Write([]byte(theftRequestAHead)); err != nil {
		t.Fatalf("write A: %v", err)
	}
	ce, ok := tr.WaitClose(3 * time.Second)
	if !ok {
		r.missNoClose++
		t.Logf("RECVTHEFT715 arm=%s attempt=%d miss=no-close-hold gate=%v witness=+%d", arm.name, r.attempts, tr.GateUsed(),
			recvtheft.CloseWithUnsubmittedRecv()-witness0)
		return false
	}

	// W1 is parked right after its close path, with A's recv unsubmitted.
	// Did that close release N? Read it before any candidate is dialed: the
	// number still names A's socket (its peer is A's local address), or it
	// was released.
	aTarget := fdTarget(ce.FD)
	r.heldOpen = strings.HasPrefix(aTarget, "socket:")
	r.heldByA = r.heldOpen && namesPeer(ce.FD, a.LocalAddr().String())
	if arm.hole {
		// Open a hole under N: close the last candidate, never dialed, whose
		// number was allocated before A's.
		r.hole = cands[len(cands)-1]
		cands = cands[:len(cands)-1]
		if r.hole > ce.FD {
			t.Fatalf("the hole candidate's number %d is not below A's %d", r.hole, ce.FD)
		}
		_ = unix.Close(r.hole)
	}

	// Dial B candidates until the sibling's accept of one tests the theft
	// (theftHit). One that hashes to W1's listener waits in W1's accept
	// queue. The sibling's accept is given the lowest free number: N if the
	// close released N and no hole lies below it, and then the sibling is
	// parked too (its recv for B prepared, not submitted); a hole below N if
	// there is one, and then that B is served, N is still free, and the next
	// candidate is dialed; another number if N is still A's, and then B is
	// served with no hold.
	var hit *recvtheft.AcceptEvent
	var hitFD int
	for i, fd := range cands {
		if err := unix.Connect(fd, sa); err != nil {
			t.Fatalf("connect candidate %d: %v", i, err)
		}
		r.dialed++
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
			t.Logf("RECVTHEFT715 arm=%s attempt=%d candidate=%d accepted by the closer %d as fd %d (target %d)", arm.name, r.attempts, i, ev.Worker, ev.FD, ce.FD)
			continue
		}
		if !theftHit(ev.FD, ce.FD, r.heldByA) {
			r.missHole++
			t.Logf("RECVTHEFT715 arm=%s attempt=%d candidate=%d accepted by the sibling %d as fd %d, a hole below A's released %d: skipped", arm.name, r.attempts, i, ev.Worker, ev.FD, ce.FD)
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
		t.Logf("RECVTHEFT715 arm=%s attempt=%d miss=no-sibling-accept fd=%d closer=%d held_by_a=%v queued=%d closer_accepts=%d hole_accepts=%d",
			arm.name, r.attempts, ce.FD, ce.Worker, r.heldByA, r.missQueued, r.missOtherFD, r.missHole)
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
	t.Logf("RECVTHEFT715 arm=%s result attempts=%d hit=%v fd=%d closer=%d sibling=%d b_fd=%d reused=%v held_open=%v held_by_a=%v released=%v hole=%d gate=%v candidates=%d witness=+%d stale_recv_data_closed=+%d stolen=%v answered=%v answer_err=%q stale=[%s] misses{no_close=%d queued=%d closer_accepts=%d hole_accepts=%d}",
		arm, r.attempts, r.hit, r.fd, r.closer, r.sibl, r.bFD, r.reused, r.heldOpen, r.heldByA, r.released, r.hole, r.gate, r.candidatesHit, r.witness, r.staleClosed, r.stolen, r.answered,
		errText, strings.Join(heads, " "), r.missNoClose, r.missQueued, r.missOtherFD, r.missHole)
}

func judgeTheftTrial(t *testing.T, arm string, r theftResult) {
	t.Helper()
	logTheftResult(t, arm, r)
	if !r.hit {
		skipOrFail656(t, "INCONCLUSIVE: no sibling accept that tests the theft while the closer was parked, in %d attempts", r.attempts)
	}
	if !r.reused && !r.heldByA {
		t.Fatalf("a hit that tests nothing: B was given %d, A's number %d was neither reused nor kept as A's socket (see theftHit)", r.bFD, r.fd)
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
	judgeTheftTrial(t, "A", runRecvTheftTrial(t, theftArm{name: "A"}))
}

// TestRecvTheft715ArmAHole is arm A with a hole opened under A's number once
// the closer is parked (see theftArm). On a tree that releases the number at
// the close the sibling's first accept fills the hole and is skipped, and the
// next is given A's number: it must fail exactly as arm A does. A verdict that
// counted the hole's accept as a hit passed that tree (celeris#793 review
// round 2).
func TestRecvTheft715ArmAHole(t *testing.T) {
	requireRecvTheft715(t)
	judgeTheftTrial(t, "A-hole", runRecvTheftTrial(t, theftArm{name: "A-hole", hole: true}))
}

// TestRecvTheft715Control is arm A's trial with the close paths submitting the
// ring before they close (recvtheft.Options.SubmitBeforeClose): A's recv is
// issued while N still names A's socket. Predicted: B answered, nothing stolen.
func TestRecvTheft715Control(t *testing.T) {
	requireRecvTheft715(t)
	judgeTheftTrial(t, "control", runRecvTheftTrial(t, theftArm{name: "control", submitBeforeClose: true}))
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
