//go:build linux

package epoll

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/mono"
)

// celeris#1165. closeIdleH1 skipped every conn with a dispatch goroutine
// (asyncPromoted: sticky, set for every conn that ever served an Async route
// and, on a server with any Async route, for every conn whose request once
// arrived in pieces), so such an idle keep-alive conn was held behind another
// conn's send drain for the whole Shutdown budget on a loop with no HTTP/2 conn,
// and for the whole pool wait on one with. It now looks inside the conn as
// io_uring's gate does: closed only when its goroutine is parked with no input,
// nothing is in progress in its parser, nothing is detached or queued; the
// rules below are each pinned by a case that must NOT be closed.

type promoted1165 struct {
	l       *Loop
	cs      *connState
	fd      int
	peer    int
	discons atomic.Int64
}

// newPromoted1165 is a conn as production leaves it after its dispatch goroutine
// answered a request and parked: asyncPromoted, asyncRun and asyncParked set,
// no input, a parser at a request boundary, nothing queued. No goroutine runs:
// the state is what the gate reads.
func newPromoted1165(t *testing.T) *promoted1165 {
	t.Helper()
	r := &promoted1165{}
	r.l = shutdownLoop863(t, func(string) { r.discons.Add(1) })
	r.l.asyncPromoted = &atomic.Uint64{}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	r.fd, r.peer = pair[0], pair[1]
	t.Cleanup(func() { _ = unix.Close(r.peer) })
	if r.fd >= len(r.l.conns) {
		t.Fatalf("socketpair fd %d outside the test conn table", r.fd)
	}
	cs := acquireConnState(t.Context(), r.fd, 4096, true)
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.remoteAddr = "127.0.0.1:9"
	cs.writeFn = r.l.makeWriteFn(cs)
	r.l.conns[r.fd] = cs
	r.l.addLiveConn(cs)
	r.l.connCount++
	r.l.activeConns.Add(1)
	r.l.initProtocol(cs)
	cs.asyncPromoted = true
	r.l.asyncPromoted.Add(1)
	cs.asyncInMu.Lock()
	cs.asyncRun, cs.asyncParked = true, true
	cs.asyncInMu.Unlock()
	r.cs = cs
	return r
}

func TestGateClosesAPromotedConnOnlyWhenItIsIdle1165(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(r *promoted1165)
		closed bool
	}{
		{"parked-idle", func(*promoted1165) {}, true},
		{"goroutine-exited-idle", func(r *promoted1165) {
			r.cs.asyncInMu.Lock()
			r.cs.asyncRun, r.cs.asyncParked = false, false
			r.cs.asyncInMu.Unlock()
		}, true},
		{"input-waiting", func(r *promoted1165) {
			r.cs.asyncInMu.Lock()
			r.cs.asyncInBuf = append(r.cs.asyncInBuf, "GET /x HTTP/1.1\r\n"...)
			r.cs.asyncInMu.Unlock()
		}, false},
		{"inside-a-handler", func(r *promoted1165) {
			r.cs.asyncInMu.Lock()
			r.cs.asyncParked = false
			r.cs.asyncInMu.Unlock()
		}, false},
		{"detachMu-held", func(r *promoted1165) { r.cs.detachMu.Lock() }, false},
		{"request-in-progress", func(r *promoted1165) {
			_ = conn.ProcessH1(context.Background(), []byte("GET /x HTTP/1.1\r\nHo"), r.cs.h1State, r.l.handler, func([]byte) {})
			if !r.cs.h1State.HasPendingData() {
				panic("premise: the partial head is not pending data")
			}
		}, false},
		{"h2c-upgrade-pending", func(r *promoted1165) { r.cs.h1State.UpgradeInfo = &conn.UpgradeInfo{} }, false},
		{"switched-to-h2", func(r *promoted1165) {
			r.cs.asyncInMu.Lock()
			r.cs.h2Switched = true
			r.cs.asyncInMu.Unlock()
		}, false},
		{"response-queued", func(r *promoted1165) { r.cs.writeBuf = append(r.cs.writeBuf, 'x') }, false},
		{"detached-stream-running", func(r *promoted1165) { r.cs.h1State.Detached.Store(true) }, false},
		{"detached-stream-ended", func(r *promoted1165) {
			r.cs.h1State.Detached.Store(true)
			r.cs.detachClosed = true
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPromoted1165(t)
			tc.mutate(r)
			locked := tc.name == "detachMu-held"
			n := r.l.closeIdleH1(false, mono.Now())
			if locked {
				r.cs.detachMu.Unlock()
			}
			if tc.closed {
				if n != 1 || r.l.conns[r.fd] != nil || r.discons.Load() != 1 || r.l.closeCount.Load() != 1 || r.l.activeConns.Load() != 0 {
					t.Fatalf("closed=%d live=%v OnDisconnect=%d closeCount=%d activeConns=%d, want 1 closed once",
						n, r.l.conns[r.fd] != nil, r.discons.Load(), r.l.closeCount.Load(), r.l.activeConns.Load())
				}
				return
			}
			if (tc.name == "request-in-progress" || tc.name == "h2c-upgrade-pending") && !r.cs.h1State.CloseAfter {
				t.Fatal("a request in progress on a promoted conn is not marked close-after: it would be answered with keep-alive")
			}
			if n != 0 || r.l.conns[r.fd] == nil || r.discons.Load() != 0 || r.l.closeCount.Load() != 0 || r.l.activeConns.Load() != 1 {
				t.Fatalf("closed=%d live=%v OnDisconnect=%d closeCount=%d activeConns=%d, want the conn left alone",
					n, r.l.conns[r.fd] != nil, r.discons.Load(), r.l.closeCount.Load(), r.l.activeConns.Load())
			}
		})
	}
}

// The first pass holds, and does not close, a promoted conn whose next request
// is already in the kernel's receive queue (celeris#1104): it is read on, which
// for a promoted conn is a hand-over of the bytes to its goroutine.
func TestGateHoldsAPromotedConnWithAnUnreadRequest1165(t *testing.T) {
	r := newPromoted1165(t)
	if _, err := unix.Write(r.peer, []byte("GET /y HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if n := r.l.closeIdleH1(true, mono.Now()); n != 0 {
		t.Fatalf("the first pass closed %d conns, want the promoted conn with an unread request held", n)
	}
	if r.l.conns[r.fd] == nil || !r.cs.gateRead {
		t.Fatalf("live=%v gateRead=%v, want live and marked to be read on", r.l.conns[r.fd] != nil, r.cs.gateRead)
	}
	r.cs.asyncInMu.Lock()
	got := len(r.cs.asyncInBuf)
	r.cs.asyncInMu.Unlock()
	if got == 0 {
		t.Fatal("the request was not handed to the dispatch goroutine's input")
	}
}

// celeris#1166. The gate reads on, and answers, a request in progress on a
// promoted conn too (a slow upload on a server with any Async route promotes the
// conn whose request arrives in pieces), and refuses what an idle one sends.
func TestRefusesRecvReadsOnAPromotedConnOnlyWhileARequestIsInProgress1166(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(r *promoted1165)
		refuse bool
	}{
		{"idle", func(*promoted1165) {}, true},
		{"idle-with-a-request-arriving-after-the-gate", func(r *promoted1165) {
			if _, err := unix.Write(r.peer, []byte("GET /y HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
				panic(err)
			}
		}, true},
		{"detached-stream-with-bytes-in-its-parser", func(r *promoted1165) {
			_ = conn.ProcessH1(context.Background(), []byte("POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nabc"), r.cs.h1State, r.l.handler, func([]byte) {})
			r.cs.h1State.Detached.Store(true)
		}, true},
		{"partial-request-in-the-parser", func(r *promoted1165) {
			_ = conn.ProcessH1(context.Background(), []byte("POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nabc"), r.cs.h1State, r.l.handler, func([]byte) {})
		}, false},
		{"input-waiting-for-the-goroutine", func(r *promoted1165) {
			r.cs.asyncInMu.Lock()
			r.cs.asyncInBuf = append(r.cs.asyncInBuf, 'x')
			r.cs.asyncInMu.Unlock()
		}, false},
		{"inside-a-handler", func(r *promoted1165) {
			r.cs.asyncInMu.Lock()
			r.cs.asyncParked = false
			r.cs.asyncInMu.Unlock()
		}, false},
		{"switched-to-h2", func(r *promoted1165) {
			r.cs.asyncInMu.Lock()
			r.cs.h2Switched = true
			r.cs.asyncInMu.Unlock()
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPromoted1165(t)
			tc.mutate(r)
			if got := refusesRecv(r.cs); got != tc.refuse {
				t.Fatalf("refusesRecv = %v, want %v", got, tc.refuse)
			}
		})
	}
}

// celeris#1166. h1InProgress is what keeps a loop with no HTTP/2 conn waiting,
// after its context is cancelled, for the request a client is in the middle of
// sending (or the handler answering it); every case below is a state that must
// not end the wait, and the idle ones must.
func TestH1InProgressDecidesTheWaitOfALoopWithoutHTTP21166(t *testing.T) {
	partial := func(r *promoted1165) {
		_ = conn.ProcessH1(context.Background(), []byte("POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nabc"), r.cs.h1State, r.l.handler, func([]byte) {})
	}
	plain := func(f func(r *promoted1165)) func(r *promoted1165) {
		return func(r *promoted1165) { r.cs.asyncPromoted = false; f(r) }
	}
	cases := []struct {
		name       string
		mutate     func(r *promoted1165)
		inProgress bool
	}{
		{"plain-idle", plain(func(*promoted1165) {}), false},
		{"promoted-idle", func(*promoted1165) {}, false},
		{"plain-partial-request", plain(partial), true},
		{"promoted-partial-request", partial, true},
		{"plain-h2c-upgrade-pending", plain(func(r *promoted1165) { r.cs.h1State.UpgradeInfo = &conn.UpgradeInfo{} }), true},
		{"undetected-with-some-bytes", plain(func(r *promoted1165) { r.cs.detected, r.cs.detectN = false, 3 }), true},
		{"undetected-with-none", plain(func(r *promoted1165) { r.cs.detected, r.cs.detectN = false, 0 }), false},
		{"promoted-input-waiting", func(r *promoted1165) {
			r.cs.asyncInMu.Lock()
			r.cs.asyncInBuf = append(r.cs.asyncInBuf, 'x')
			r.cs.asyncInMu.Unlock()
		}, true},
		{"promoted-inside-a-handler", func(r *promoted1165) {
			r.cs.asyncInMu.Lock()
			r.cs.asyncParked = false
			r.cs.asyncInMu.Unlock()
		}, true},
		{"promoted-detached-stream-with-bytes-in-its-parser", func(r *promoted1165) {
			partial(r)
			r.cs.h1State.Detached.Store(true)
		}, false},
		{"promoted-parked-with-a-guarded-write", func(r *promoted1165) { r.cs.detachMu.Lock() }, false},
		{"plain-detached-stream", plain(func(r *promoted1165) {
			partial(r)
			r.cs.h1State.Detached.Store(true)
		}), false},
		{"plain-http2", plain(func(r *promoted1165) {
			partial(r)
			r.cs.protocol = engine.H2C
		}), false},
		{"promoted-switched-to-h2", func(r *promoted1165) {
			partial(r)
			r.cs.asyncInMu.Lock()
			r.cs.h2Switched = true
			r.cs.asyncInMu.Unlock()
		}, false},
		{"hijacked", func(r *promoted1165) {
			partial(r)
			r.cs.hijacked.Store(true)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPromoted1165(t)
			tc.mutate(r)
			locked := tc.name == "promoted-parked-with-a-guarded-write"
			got := r.l.h1InProgress()
			if locked {
				r.cs.detachMu.Unlock()
			}
			if got != tc.inProgress {
				t.Fatalf("h1InProgress = %v, want %v", got, tc.inProgress)
			}
		})
	}
	t.Run("request-unread-in-the-kernel-first-call-only", func(t *testing.T) {
		r := newPromoted1165(t)
		r.cs.asyncPromoted = false
		if _, err := unix.Write(r.peer, []byte("GET /y HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		if !r.l.h1InProgress() {
			t.Fatal("a request in the kernel's receive queue, not read yet, is not in progress on the first call")
		}
		// The pacing clock holds the answer between polls; a second asker after it
		// does not ask the kernel again (the loop reads the bytes as events).
		r.l.h1InProgressNext = 0
		if r.l.h1InProgress() {
			t.Fatal("the second call asked the kernel again")
		}
	})
}

// The wait is paced and bounded: a yes holds until the next poll time, and the
// wait ends at sendDrainWait's end whatever the clients are doing.
func TestPoolWaitSettledWaitsForARequestInProgressAndIsBounded1166(t *testing.T) {
	r := newPromoted1165(t)
	r.cs.asyncPromoted = false
	_ = conn.ProcessH1(context.Background(), []byte("POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nabc"), r.cs.h1State, r.l.handler, func([]byte) {})
	if r.l.poolWaitSettled() {
		t.Fatal("the loop is settled with a request in progress")
	}
	if r.l.h2DrainStart.IsZero() {
		t.Fatal("the wait has no start: its bound would never come")
	}
	// The request ends; the pacing holds the answer until the next poll time.
	r.cs.h1State = conn.NewH1State()
	if r.l.poolWaitSettled() {
		t.Fatal("the loop is settled before the pacing's next poll")
	}
	r.l.h1InProgressNext = 0
	if !r.l.poolWaitSettled() {
		t.Fatal("the loop is not settled with nothing in progress")
	}
	// A request that never ends: the wait ends at its bound (no Shutdown budget:
	// the floor, 250 ms, from the start).
	r.cs.h1State = conn.NewH1State()
	_ = conn.ProcessH1(context.Background(), []byte("POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nabc"), r.cs.h1State, r.l.handler, func([]byte) {})
	r.l.h1InProgressNext = 0
	r.l.h2DrainStart = time.Now().Add(-2 * shutdownSendDrainFloor)
	if !r.l.poolWaitSettled() {
		t.Fatal("the wait for a request in progress is not bounded")
	}
}

// A detached stream's writer goroutine owns the conn's write state (it appends
// to writeBuf under detachMu through the guarded write hook); h1InProgress, on
// the loop thread, must not read that state (csWritePending) of a detached
// conn: CI's -race saw it in TestStalledSSEEndsWithShutdown855. The writer here
// does what the hook does, and the first call asks about the kernel's queue as
// the shutdown's first call does. Run it under -race.
func TestH1InProgressDoesNotReadADetachedStreamsWriteState1166(t *testing.T) {
	r := newPromoted1165(t)
	r.cs.asyncPromoted = false
	r.cs.h1State.Detached.Store(true)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.cs.detachMu.Lock()
			r.cs.writeBuf = append(r.cs.writeBuf[:0], 'x')
			r.cs.writePos = 0
			r.cs.bodyBuf = nil
			r.cs.detachMu.Unlock()
		}
	}()
	for range 2000 {
		r.l.h1InProgressAsked = false // the first call, which asks the kernel
		r.l.h1InProgressNext = 0
		if r.l.h1InProgress() {
			t.Error("a detached stream is a request in progress")
			break
		}
	}
	close(stop)
	<-done
}

// celeris#1166 (review r2 of #1168, MINOR-2). A request that reached the
// kernel before the cancel, on an idle PROMOTED conn, is as much a request in
// progress as on a plain conn: the first call asks the kernel's queue, inside
// the conn's lock and only with the goroutine parked and nothing queued (the
// plain conn's line would read state the dispatch goroutine owns).
func TestH1InProgressAsksTheKernelAboutAPromotedConn1166(t *testing.T) {
	send := func(r *promoted1165) {
		if _, err := unix.Write(r.peer, []byte("GET /y HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("unread-request-first-call", func(t *testing.T) {
		r := newPromoted1165(t)
		send(r)
		if !r.l.h1InProgress() {
			t.Fatal("a request in the kernel's queue of an idle promoted conn is not in progress: the loop would shut down and cut it")
		}
		r.l.h1InProgressNext = 0
		if r.l.h1InProgress() {
			t.Fatal("the second call asked the kernel again")
		}
	})
	t.Run("nothing-unread", func(t *testing.T) {
		r := newPromoted1165(t)
		if r.l.h1InProgress() {
			t.Fatal("an idle promoted conn with an empty queue is in progress")
		}
	})
	t.Run("response-queued-is-not-asked", func(t *testing.T) {
		r := newPromoted1165(t)
		send(r)
		r.cs.writeBuf = append(r.cs.writeBuf, 'x') // a conn that holds a response is not asked, as a plain one
		if r.l.h1InProgress() {
			t.Fatal("a promoted conn with a response queued was asked about the kernel's queue")
		}
	})
	t.Run("detached-stream-is-not-asked", func(t *testing.T) {
		r := newPromoted1165(t)
		send(r)
		r.cs.h1State.Detached.Store(true)
		if r.l.h1InProgress() {
			t.Fatal("a detached stream was asked about the kernel's queue")
		}
	})
}
