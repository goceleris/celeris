//go:build linux

package epoll

// celeris#885 item 41, the other half of the read budget. The gate that fails
// without it is TestDrainReadIsBoundedUnderAFlood885: a turn of drainRead is
// bounded. These tests are guards of what the bound must not break, and none of
// them fails without it (a conn with no budget reads everything): a turn that
// stops at the budget leaves bytes in the socket, edge-triggered epoll will not
// report them again, and the conn must be given another turn (yieldRead's
// EPOLL_CTL_MOD) or the rest of a burst, a body or a pipeline waits for bytes
// the peer will never send. Each is checked against a mutant that drops the MOD.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// withBudget885 sets drainReadBudget to n for the test.
func withBudget885(t *testing.T, n int) {
	t.Helper()
	old := drainReadBudget
	drainReadBudget = n
	t.Cleanup(func() { drainReadBudget = old })
}

// readyForFD885 reports the event ready for fd on l's epoll set right now.
func readyForFD885(l *Loop, fd int) (unix.EpollEvent, bool) {
	evs := make([]unix.EpollEvent, 16)
	n, _ := unix.EpollWait(l.epollFD, evs, 0)
	for i := 0; i < n; i++ {
		if int(evs[i].Fd) == fd {
			return evs[i], true
		}
	}
	return unix.EpollEvent{}, false
}

// budgetConn885 is a conn of the engine's own making (attachAdoptedFD) over a
// socketpair, whose responses go to a 4 MiB queue.
func budgetConn885(t *testing.T, bufSize int) (l *Loop, fd, peer int, cs *connState) {
	t.Helper()
	l = newLedgerLoop(t)
	l.resolved = resource.ResolvedResources{BufferSize: bufSize}
	l.handler = newPathHandler885(1 << 20)
	fd, peer, cs = adoptedPair885(t, l)
	var b [8]byte
	_, _ = unix.Read(fd, b[:]) // the byte adoptedPair885 wrote to have the ADD report EPOLLIN
	for _, s := range []int{fd, peer} {
		_ = unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_SNDBUF, 4<<20)
	}
	drainEvents885(l)
	return l, fd, peer, cs
}

// sendAll885 writes data to peer, in chunks, retrying on EAGAIN for up to 5 s.
// Everything the tests send is in the socket before the loop reads it, so it
// must fit the queue the kernel gives a socketpair (about 210 KB at the default
// net.core.wmem_max, which is what a CI runner has): the largest payload here is
// 170 KB.
func sendAll885(t *testing.T, peer int, data []byte) {
	t.Helper()
	dl := time.Now().Add(5 * time.Second)
	for off := 0; off < len(data); {
		n, err := unix.Write(peer, data[off:min(off+64<<10, len(data))])
		if n > 0 {
			off += n
		}
		if err != nil && err != unix.EAGAIN {
			t.Fatalf("peer write: %v", err)
		}
		if err == unix.EAGAIN && time.Now().After(dl) {
			t.Fatalf("apparatus: the socket took only %d of %d bytes: its queue is smaller than the payload", off, len(data))
		}
	}
}

// takeResponses885 reads the peer's end and returns what it holds.
func takeResponses885(peer int) []byte {
	var out []byte
	buf := make([]byte, 256<<10)
	for {
		n, err := unix.Read(peer, buf)
		if n <= 0 || err != nil {
			return out
		}
		out = append(out, buf[:n]...)
	}
}

// responses885 counts the responses the peer takes, as it takes them: the loop
// parks a conn whose responses the peer has not read once the socket's queue
// is full, which is not what these tests are about.
type responses885 struct {
	peer int
	buf  []byte
}

// take reads what the peer holds.
func (r *responses885) take() { r.buf = append(r.buf, takeResponses885(r.peer)...) }

// count is the number of responses taken.
func (r *responses885) count() int { return bytes.Count(r.buf, []byte("HTTP/1.1 200")) }

// pumpReads885 plays the run loop for fd: while the epoll set reports an event
// for it, drainRead, and the peer takes the responses. It returns the number of
// turns it took.
func pumpReads885(l *Loop, fd int, got *responses885) int {
	turns := 0
	for {
		ev, ok := readyForFD885(l, fd)
		if !ok {
			return turns
		}
		if ev.Events&unix.EPOLLIN != 0 {
			turns++
			l.drainRead(fd, time.Now().UnixNano())
			got.take()
		}
	}
}

// TestDrainReadBudgetGivesTheConnAnotherTurn885: 3000 pipelined requests are
// already in the socket (about 100 KB) and the budget is 16 KiB, so the first
// turn stops with the socket not empty. The conn must be ready again, with its
// generation, and the turns that follow must answer every request.
func TestDrainReadBudgetGivesTheConnAnotherTurn885(t *testing.T) {
	withBudget885(t, 16<<10)
	l, fd, peer, cs := budgetConn885(t, 4096)
	const reqs = 3000
	sendAll885(t, peer, []byte(strings.Repeat("GET /small HTTP/1.1\r\nHost: x\r\n\r\n", reqs)))

	before := l.bytesReadBatch
	l.drainRead(fd, time.Now().UnixNano())
	read := int(l.bytesReadBatch - before)
	if read < 16<<10 || read > 16<<10+4096 {
		t.Fatalf("the first turn read %d bytes, want the budget (16384) and at most one buffer more", read)
	}
	ev, ok := readyForFD885(l, fd)
	if !ok {
		t.Fatalf("the conn is not ready after a turn that stopped at the budget with the socket not empty: its rest would wait for bytes the peer does not send")
	}
	if ev.Events&unix.EPOLLIN == 0 || uint32(ev.Pad) != cs.gen {
		t.Fatalf("the event after the turn is %#x with Pad %#x, want EPOLLIN and the conn's generation %#x", ev.Events, uint32(ev.Pad), cs.gen)
	}
	// The event was consumed by the check above: ready again only through a
	// MOD, so pump from the state the run loop is in (it would have this event).
	got := &responses885{peer: peer}
	l.drainRead(fd, time.Now().UnixNano())
	got.take()
	turns := 2 + pumpReads885(l, fd, got)
	if got.count() != reqs {
		t.Fatalf("%d of %d requests were answered after %d turns: readiness was lost", got.count(), reqs, turns)
	}
	if turns < 5 {
		t.Fatalf("the burst took %d turns at a 16 KiB budget, want more than 5: the apparatus does not reach the budget", turns)
	}
}

// TestDrainReadBudgetAfterADirectBody885: the budget runs out on the direct-body
// path (a POST body received straight into H1State's buffer, a read of the
// whole body), with 2000 pipelined requests behind it still in the socket; the
// turn that the body ended must give the conn another one.
func TestDrainReadBudgetAfterADirectBody885(t *testing.T) {
	withBudget885(t, 16<<10)
	l, fd, peer, _ := budgetConn885(t, 4096)
	const bodyLen = 100 << 10
	req := "POST /small HTTP/1.1\r\nHost: x\r\nContent-Length: " + fmt.Sprint(bodyLen) + "\r\n\r\n" + strings.Repeat("b", bodyLen)
	const tail = 2000
	sendAll885(t, peer, []byte(req+strings.Repeat("GET /small HTTP/1.1\r\nHost: x\r\n\r\n", tail)))

	got := &responses885{peer: peer}
	l.drainRead(fd, time.Now().UnixNano())
	got.take()
	turns := 1 + pumpReads885(l, fd, got)
	if got.count() != 1+tail {
		t.Fatalf("%d of %d requests (a %d-byte POST, then %d GETs) were answered after %d turns: readiness was lost", got.count(), 1+tail, bodyLen, tail, turns)
	}
	if turns < 2 {
		t.Fatalf("the request took %d turn, want more than 1: the apparatus does not reach the budget", turns)
	}
}

// TestDrainReadBudgetKeepsEPOLLOUT885: a conn parked on a response the peer has
// not taken (EPOLLOUT armed) yields at the budget; the MOD must keep EPOLLOUT in
// the interest, or the flush that waits for the socket's room never resumes.
func TestDrainReadBudgetKeepsEPOLLOUT885(t *testing.T) {
	withBudget885(t, 16<<10)
	l, fd, peer, cs := budgetConn885(t, 4096)
	// A small send queue, so that the response parks.
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 4096)
	sendAll885(t, peer, []byte("GET /big HTTP/1.1\r\nHost: x\r\n\r\n"))
	l.drainRead(fd, time.Now().UnixNano())
	if !cs.epollOut {
		t.Fatalf("apparatus: the conn is not parked on its response (epollOut false)")
	}
	drainEvents885(l)

	sendAll885(t, peer, []byte(strings.Repeat("GET /small HTTP/1.1\r\nHost: x\r\n\r\n", 2000)))
	l.drainRead(fd, time.Now().UnixNano()) // stops at the budget, MODs
	if !cs.epollOut {
		t.Fatalf("the turn disarmed EPOLLOUT in the conn's own state")
	}
	drainEvents885(l)
	// The peer takes the response: the socket is writable again, an edge that
	// only an interest with EPOLLOUT reports.
	for i := 0; i < 8; i++ {
		takeResponses885(peer)
		time.Sleep(5 * time.Millisecond)
	}
	ev, ok := readyForFD885(l, fd)
	for i := 0; i < 20 && (!ok || ev.Events&unix.EPOLLOUT == 0); i++ {
		time.Sleep(5 * time.Millisecond)
		takeResponses885(peer)
		ev, ok = readyForFD885(l, fd)
	}
	if !ok || ev.Events&unix.EPOLLOUT == 0 {
		t.Fatalf("no EPOLLOUT event (%#x, ready %v) after the peer took the response: the turn's MOD dropped EPOLLOUT from the interest", ev.Events, ok)
	}
}

// TestDrainReadBudgetSparesAClosingConn885: a conn whose close is deferred until
// its response has gone out (peerClosed; closeWhenFlushed's interest has no
// EPOLLIN by design) is not given a MOD with EPOLLIN at the budget.
func TestDrainReadBudgetSparesAClosingConn885(t *testing.T) {
	withBudget885(t, 16<<10)
	l, fd, peer, cs := budgetConn885(t, 4096)
	cs.peerClosed = true
	sendAll885(t, peer, []byte(strings.Repeat("GET /small HTTP/1.1\r\nHost: x\r\n\r\n", 3000)))
	drainEvents885(l)
	before := l.bytesReadBatch
	l.drainRead(fd, time.Now().UnixNano())
	if read := int(l.bytesReadBatch - before); read <= 16<<10+4096 {
		t.Fatalf("a closing conn stopped after %d bytes: the turn must go on as it did without a budget", read)
	}
}

// slowFirst885 is pathHandler885 whose /slow answers after 300 ms: the loop
// (a sync engine) or the conn's dispatch goroutine (an async one) is held on
// the first request of a burst while the rest of it, written at once into the
// socket's large buffers, collects in the kernel: most of it where the buffers
// are capped (a CI runner's net.core.wmem_max and rmem_max give about 425 KB,
// and an explicit SO_RCVBUF turns autotuning off), all of it where they are
// not. The bytes the budget leaves in the socket at the end of the burst are
// not followed by any new segment, so only the re-armed readiness reads them:
// that tail is what tests the re-arm.
type slowFirst885 struct{ *pathHandler885 }

func (h slowFirst885) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.Path == "/slow" {
		time.Sleep(300 * time.Millisecond)
	}
	return h.pathHandler885.HandleStream(ctx, s)
}

// burst885 sends first and then n requests req down one conn in one write
// (larger than the budget) and reads the n+1 responses back, then waits for
// nothing more. With halfClose the client shuts its write side right after the
// burst, so that the FIN rides the same readable edge as the bytes the turn
// leaves in the socket: the EPOLLRDHUP of the event that ended a turn at its
// budget must not close the conn before the rest has been read and answered.
func burst885(t *testing.T, addr, first, req string, n int, halfClose bool) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.(*net.TCPConn).SetWriteBuffer(8 << 20)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(c, strings.NewReader(first+strings.Repeat(req, n)))
		if halfClose {
			_ = c.(*net.TCPConn).CloseWrite()
		}
	}()
	br := bufio.NewReader(c)
	for i := 0; i <= n; i++ {
		_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("response %d of %d: %v (the rest of the burst was never read)", i, n+1, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	wg.Wait()
}

// TestDrainReadBudgetEndToEnd885: the default budget, a real engine, a burst of
// up to 2 MiB of pipelined requests (or 1 MiB of POST body) written at once
// behind a request that takes 300 ms, and then silence, or the client's FIN:
// every response must arrive. On the sync engine and on the async one, where the loop hands the
// bytes to the conn's dispatch goroutine.
func TestDrainReadBudgetEndToEnd885(t *testing.T) {
	const slow = "GET /slow HTTP/1.1\r\nHost: x\r\n\r\n"
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async=%v", async), func(t *testing.T) {
			_, addr := startE2E885(t, async, slowFirst885{newPathHandler885(1 << 10)}, func(c *resource.Config) {
				c.Resources.BufferSize = 8192
				c.Resources.SocketRecv = 8 << 20
			})
			t.Run("pipeline", func(t *testing.T) {
				const req = "GET /small HTTP/1.1\r\nHost: x\r\n\r\n"
				// 2 MiB; the async engine takes less: its dispatch goroutine
				// answers a whole batch before it flushes, and a conn whose
				// queued responses pass the write cap is closed, which is not
				// what this test asks.
				size := 2 << 20
				if async {
					size = 768 << 10
				}
				burst885(t, addr, slow, req, size/len(req), false)
				if !async {
					// The async engine answers none of a burst that ends in a FIN
					// while its goroutine is still in the first request, with or
					// without a budget (the conn is closed with the bytes in
					// asyncInBuf): not what this test asks.
					burst885(t, addr, slow, req, (192<<10)/len(req), true)
				}
			})
			t.Run("body", func(t *testing.T) {
				body := strings.Repeat("p", 1<<20)
				post := "POST /small HTTP/1.1\r\nHost: x\r\nContent-Length: " + fmt.Sprint(len(body)) + "\r\n\r\n" + body
				burst885(t, addr, slow, post, 1, false)
			})
		})
	}
}

// sleepFlood885 is floodHandler885 whose /sleep holds the loop thread for 300 ms
// (a sync engine runs the handler inline).
type sleepFlood885 struct{ floodHandler885 }

func (h sleepFlood885) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.Path == "/sleep" {
		time.Sleep(300 * time.Millisecond)
	}
	return h.floodHandler885.HandleStream(ctx, s)
}

// TestRunLoopHandlesAHalfCloseAfterAYieldedTurn885: the run loop skips the
// half-close, error and transplant steps of an event whose turn yielded
// (Loop.readYielded), and clears the flag before every drainRead. Without the
// clear, the first yield on a loop would leave the flag set for good: every
// later read event on it would skip its steps, and an edge-triggered EPOLLRDHUP
// is not reported again. Two conns on one loop: A floods and yields (the budget
// is 16 KiB, the burst 256 KiB); then A holds the loop thread in a 300 ms
// handler, and B's request and its FIN are both queued behind it, so that B's
// read is a short read that does not see the FIN and only the run loop's
// EPOLLRDHUP step can end B (a FIN that comes after the read is an event of
// its own, read as EOF whatever the flag says). The server must end B.
func TestRunLoopHandlesAHalfCloseAfterAYieldedTurn885(t *testing.T) {
	withBudget885(t, 16<<10) // before the engine starts: the loops read it
	e := startFloodEngineWith885(t, 0, sleepFlood885{})
	a, b, _ := threeConns885(t, e)

	const req = "GET /flood HTTP/1.1\r\nHost: x\r\n\r\n"
	n := (256 << 10) / len(req)
	go func() { _, _ = io.Copy(a.c, strings.NewReader(strings.Repeat(req, n))) }()
	for i := 0; i < n; i++ {
		_ = a.c.SetReadDeadline(time.Now().Add(10 * time.Second))
		resp, err := http.ReadResponse(a.br, nil)
		if err != nil {
			t.Fatalf("flood response %d of %d: %v", i, n, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	// Hold the loop thread, then queue B's request and FIN behind it.
	if _, err := a.c.Write([]byte("GET /sleep HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write /sleep: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	_ = b.c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := b.c.Write([]byte(req)); err != nil {
		t.Fatalf("write B: %v", err)
	}
	if err := b.c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	resp, err := http.ReadResponse(b.br, nil)
	if err != nil {
		t.Fatalf("B's response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if _, err := b.br.ReadByte(); err != io.EOF {
		t.Fatalf("B sent its FIN after a yielded turn on its loop and the server did not end it (read: %v): the half-close was skipped", err)
	}
}
