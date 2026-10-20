//go:build linux

package epoll

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#885 items 35 and 45, review round 2: every flush that takes bytes of a
// response the conn has parked, and leaves the rest queued, restarts the write
// clock before the conn is parked again (noteWriteProgress, then armEpollOut,
// which keeps a clock that is set). Before, only the EPOLLOUT edge's flush
// (handleWritable), the dirty pass and the HTTP/2 queue drain did, and a client
// that sent anything while it read (every segment is an EPOLLIN event: an
// HTTP/2 PING or WINDOW_UPDATE, a pipelined request) had the room it made taken
// by drainRead's inline flush, which was invisible to the clock, and was cut at
// WriteTimeout while it still took the response. The sites are driven here
// through the real drainRead (inline flush, body-recv flush, h2c upgrade) and
// the real flushCarriedReplay and makeWriteBodyFn, on a conn of the engine's
// own making (attachAdoptedFD) over a socketpair whose queue is 3 MiB.

// sitesConn885 is a conn parked on a 3 MiB keep-alive response, as the
// engine parks one: GET /big read by drainRead, its response flushed as far
// as the socket takes it, armEpollOut called.
type sitesConn885 struct {
	t    *testing.T
	l    *Loop
	fd   int
	peer int
	cs   *connState
	got  int
}

func newSitesConn885(t *testing.T, cfg resource.Config) *sitesConn885 {
	t.Helper()
	l := newLedgerLoop(t)
	l.cfg = cfg
	l.handler = newPathHandler885(3 << 20)
	fd, peer, cs := adoptedPair885(t, l)
	var b [8]byte
	_, _ = unix.Read(fd, b[:]) // the byte adoptedPair885 wrote to be detected
	r := &sitesConn885{t: t, l: l, fd: fd, peer: peer, cs: cs}
	r.send("GET /big HTTP/1.1\r\nHost: x\r\n\r\n")
	if r.cs.writeSince == 0 || !r.cs.epollOut || !csWritePending(r.cs) {
		t.Fatalf("apparatus: the conn is not parked on its response: writeSince=%d epollOut=%v pending=%d",
			r.cs.writeSince, r.cs.epollOut, csPendingBytes(r.cs))
	}
	r.cs.lastActivity = time.Now().UnixNano()
	return r
}

// send writes data from the peer and has the loop read it (drainRead).
func (r *sitesConn885) send(data string) {
	r.t.Helper()
	if _, err := unix.Write(r.peer, []byte(data)); err != nil {
		r.t.Fatalf("peer write: %v", err)
	}
	r.l.drainRead(r.fd, time.Now().UnixNano())
}

// read takes up to n bytes of the response at the peer.
func (r *sitesConn885) read(n int) int {
	buf := make([]byte, n)
	k, err := unix.Read(r.peer, buf)
	if err != nil || k < 0 {
		return 0
	}
	r.got += k
	return k
}

func (r *sitesConn885) open() bool { return r.l.conns[r.fd] == r.cs }

func (r *sitesConn885) age() time.Duration {
	return time.Since(time.Unix(0, r.cs.writeSince)).Round(time.Millisecond)
}

// TestDrainReadInlineFlushRestartsTheWriteClock885: the peer takes 64 KiB every
// 100 ms and sends a pipelined request each time, for twice WriteTimeout (300
// ms); the conn must survive every sweep. Then the peer stops, and it is reaped
// a bound later.
func TestDrainReadInlineFlushRestartsTheWriteClock885(t *testing.T) {
	const bound = 300 * time.Millisecond
	r := newSitesConn885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: bound})
	for i := 0; i < 8; i++ {
		time.Sleep(100 * time.Millisecond)
		if r.read(64<<10) == 0 {
			t.Fatalf("apparatus: the peer read nothing")
		}
		r.send("GET /small HTTP/1.1\r\nHost: x\r\n\r\n")
		r.l.checkTimeouts()
		if !r.open() {
			t.Fatalf("celeris#885 items 35 and 45: the conn was reaped %d ms in, the peer taking 64 KiB every 100 ms and sending a request each time, "+
				"WriteTimeout %v: drainRead's inline flush took bytes and did not restart the write clock", (i+1)*100, bound)
		}
	}
	time.Sleep(bound + 100*time.Millisecond)
	r.l.checkTimeouts()
	if r.open() {
		t.Fatalf("a peer that stopped reading was not reaped %v after its last byte", bound+100*time.Millisecond)
	}
}

// TestDrainReadBodyRecvFlushRestartsTheWriteClock885: the body-recv branch of
// drainRead (a POST body read straight into H1State's buffer) is the second
// inline flush. The first part of the request finds the socket full (no
// progress, the clock stays); the peer then reads, and the rest of the body
// completes the request: that flush takes bytes.
func TestDrainReadBodyRecvFlushRestartsTheWriteClock885(t *testing.T) {
	r := newSitesConn885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: time.Hour})
	started := r.cs.writeSince
	time.Sleep(150 * time.Millisecond)
	r.send("POST /small HTTP/1.1\r\nHost: x\r\nContent-Length: 200\r\n\r\n" + strings.Repeat("a", 50))
	if r.cs.writeSince != started {
		t.Fatalf("apparatus: the first part of the request moved the clock (the peer had read nothing): %d -> %d", started, r.cs.writeSince)
	}
	if r.cs.h1State == nil || r.cs.h1State.NextRecvBuf() == nil {
		t.Fatalf("apparatus: the request is not waiting for its body in H1State's buffer: the body-recv branch is not reachable")
	}
	time.Sleep(100 * time.Millisecond)
	if r.read(256<<10) == 0 {
		t.Fatalf("apparatus: the peer read nothing")
	}
	r.send(strings.Repeat("b", 150))
	if !r.open() || !csWritePending(r.cs) {
		t.Fatalf("apparatus: after the body open=%v pending=%d", r.open(), csPendingBytes(r.cs))
	}
	if age := r.age(); age > 100*time.Millisecond {
		t.Errorf("celeris#885 items 35 and 45: the write clock is %v old after the body-recv branch's flush took bytes of the parked response: want it restarted", age)
	}
}

// TestWriteBodyFlushRestartsTheWriteClock885: the writev of makeWriteBodyFn
// (a body of 8 KiB or more) goes out before the inline flush that parks the
// conn; what it takes of the response queued ahead of it is progress too.
func TestWriteBodyFlushRestartsTheWriteClock885(t *testing.T) {
	r := newSitesConn885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: time.Hour})
	time.Sleep(200 * time.Millisecond)
	if r.read(128<<10) == 0 {
		t.Fatalf("apparatus: the peer read nothing")
	}
	r.l.makeWriteBodyFn(r.cs)(make([]byte, 16<<10))
	if !csWritePending(r.cs) {
		t.Fatalf("apparatus: the writev drained the queue")
	}
	if age := r.age(); age > 100*time.Millisecond {
		t.Errorf("celeris#885 items 35 and 45: the write clock is %v old after the body writer's writev took bytes of the parked response: want it restarted", age)
	}
}

// h2cUpgrade885 is a request the engine answers with a 101 and the HTTP/2
// server preface (finishH1Upgrade).
const h2cUpgrade885 = "GET /small HTTP/1.1\r\nHost: x\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n" +
	"HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\n\r\n"

// TestFinishH1UpgradeFlushRestartsTheWriteClock885: an h2c upgrade pipelined
// behind a parked response queues its 101 and preface behind it; the flush that
// follows takes the room the peer made.
func TestFinishH1UpgradeFlushRestartsTheWriteClock885(t *testing.T) {
	r := newSitesConn885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: time.Hour, EnableH2Upgrade: true})
	time.Sleep(200 * time.Millisecond)
	if r.read(128<<10) == 0 {
		t.Fatalf("apparatus: the peer read nothing")
	}
	r.send(h2cUpgrade885)
	if !r.open() || r.cs.h2State == nil {
		t.Fatalf("apparatus: the upgrade did not switch the conn to HTTP/2 (open=%v)", r.open())
	}
	if !csWritePending(r.cs) {
		t.Fatalf("apparatus: the flush drained the queue")
	}
	if age := r.age(); age > 100*time.Millisecond {
		t.Errorf("celeris#885 items 35 and 45: the write clock is %v old after finishH1Upgrade's flush took bytes of the parked response: want it restarted", age)
	}
}

// TestFlushCarriedReplayFlushRestartsTheWriteClock885: flushCarriedReplay runs
// on a conn fresh from attachAdoptedFD, whose clock is 0 in the engine (so the
// note is inert there and cannot be driven through an adoption); it is called
// here directly on a conn that has the clock, to pin the site's rule.
func TestFlushCarriedReplayFlushRestartsTheWriteClock885(t *testing.T) {
	r := newSitesConn885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: time.Hour})
	time.Sleep(200 * time.Millisecond)
	if r.read(128<<10) == 0 {
		t.Fatalf("apparatus: the peer read nothing")
	}
	r.l.flushCarriedReplay(r.cs, r.fd)
	if !r.open() || !csWritePending(r.cs) {
		t.Fatalf("apparatus: open=%v pending=%d", r.open(), csPendingBytes(r.cs))
	}
	if age := r.age(); age > 100*time.Millisecond {
		t.Errorf("celeris#885 items 35 and 45: the write clock is %v old after flushCarriedReplay's flush took bytes: want it restarted", age)
	}
}

// TestWriteClockIsNotInheritedByTheNextPark885 (review round 2): a clock left
// running by a response that has been drained is not the next response's. The
// peer takes the whole of the parked response while the next request's handler
// writes a body (makeWriteBodyFn's writev, a drain site that did not end the
// clock), WriteTimeout passes with no sweep, and the next request's big
// response parks the conn, its flushes taking nothing: it must carry a clock of
// its own, started at its park, and not be cut by WriteTimeout at once.
func TestWriteClockIsNotInheritedByTheNextPark885(t *testing.T) {
	const bound = 300 * time.Millisecond
	r := newSitesConn885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: bound})
	write := r.l.makeWriteBodyFn(r.cs)
	for i := 0; i < 400 && csWritePending(r.cs); i++ {
		for r.read(256<<10) > 0 {
		}
		write(make([]byte, 16<<10)) // the writev takes the queue, and the body, as the peer makes room
	}
	for r.read(256<<10) > 0 {
	}
	if csWritePending(r.cs) {
		t.Fatalf("apparatus: the queue did not drain")
	}
	// The socket is full again, the peer having stopped reading (filler written
	// to the descriptor), so that the next response's flushes take nothing and
	// nothing but a clock of its own can say how long it has been parked.
	filler := make([]byte, 64<<10)
	for {
		if _, err := unix.Write(r.fd, filler); err != nil {
			break
		}
	}
	time.Sleep(bound + 50*time.Millisecond) // no sweep meanwhile
	r.cs.lastActivity = time.Now().UnixNano()
	r.send("GET /big HTTP/1.1\r\nHost: x\r\n\r\n") // parks again, its inline flush having taken bytes
	if !csWritePending(r.cs) || r.cs.writeSince == 0 {
		t.Fatalf("apparatus: the next response did not park (pending=%d writeSince=%d)", csPendingBytes(r.cs), r.cs.writeSince)
	}
	age := r.age()
	r.l.checkTimeouts()
	if !r.open() {
		t.Errorf("celeris#885 items 35 and 45: a response parked a moment ago was closed at once by WriteTimeout %v: it inherited the clock of the one before (age %v)",
			bound, age)
	}
}

// nearlyDrained has the peer take the parked response by hand until
// little of it is queued (flushWrites, which does not end the clock), so that
// the next flush empties the queue.
func (r *sitesConn885) nearlyDrained() {
	r.t.Helper()
	for i := 0; i < 20000 && csPendingBytes(r.cs) > 32<<10; i++ {
		r.read(32 << 10)
		_ = r.l.flushWrites(r.cs, true)
	}
	r.cs.pendingBytes = csPendingBytes(r.cs)
	if n := csPendingBytes(r.cs); n == 0 || n > 32<<10 {
		r.t.Fatalf("apparatus: %d bytes queued, want a little", n)
	}
	for r.read(256<<10) > 0 {
	}
}

// TestFinishH1UpgradeEndsTheWriteClockWhenItDrains885 and
// TestFlushCarriedReplayEndsTheWriteClockWhenItDrains885: a flush that empties
// the queue of a conn that has a clock ends it (endWrite), as every other drain
// site does, or the next response's park would inherit its stamp.
func TestFinishH1UpgradeEndsTheWriteClockWhenItDrains885(t *testing.T) {
	r := newSitesConn885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: time.Hour, EnableH2Upgrade: true})
	r.nearlyDrained()
	r.send(h2cUpgrade885)
	if !r.open() || r.cs.h2State == nil || csWritePending(r.cs) {
		t.Fatalf("apparatus: open=%v h2=%v pending=%d, want an HTTP/2 conn with nothing queued", r.open(), r.cs.h2State != nil, csPendingBytes(r.cs))
	}
	if r.cs.writeSince != 0 {
		t.Errorf("finishH1Upgrade's flush emptied the queue and left the write clock running (%v old)", r.age())
	}
}

func TestFlushCarriedReplayEndsTheWriteClockWhenItDrains885(t *testing.T) {
	r := newSitesConn885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: time.Hour})
	r.nearlyDrained()
	r.l.flushCarriedReplay(r.cs, r.fd)
	if !r.open() || csWritePending(r.cs) {
		t.Fatalf("apparatus: open=%v pending=%d, want nothing queued", r.open(), csPendingBytes(r.cs))
	}
	if r.cs.writeSince != 0 {
		t.Errorf("flushCarriedReplay's flush emptied the queue and left the write clock running (%v old)", r.age())
	}
}
