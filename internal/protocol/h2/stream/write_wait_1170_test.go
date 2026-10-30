package stream

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// celeris#1170: a pool handler's wait for window is a record in the stall books,
// judged by the rule of every stalled stream, by the handler's own timer.

// waitRig1170 is a manager with a short write window (real timers) and a pool
// stream whose send window is shut.
func waitRig1170(t *testing.T, wt time.Duration, initWin uint32) (*Manager, *Stream) {
	t.Helper()
	_, m, _ := wdSetup889(t)
	m.SetWriteTimeout(wt)
	m.initWin.Store(initWin)
	s := openStream893(t, m, 1)
	s.flags.Or(flagAsyncRunning)
	s.SetWindowSize(0)
	return m, s
}

// run1170 waits for window as a pool handler with rest bytes to send and reports
// the error and how long it took.
func run1170(s *Stream, w *SendWait, rest int) (time.Duration, error) {
	t0 := time.Now()
	err := s.AwaitSendWindowRate(w, rest)
	return time.Since(t0), err
}

const wait1170 = 40 * time.Millisecond

// waitHeld1170 is the bytes the waiters of m still have to send.
func waitHeld1170(m *Manager) int64 {
	m.stallMu.Lock()
	defer m.stallMu.Unlock()
	return m.waitHeldLocked()
}

// Nothing is granted: the wait is judged at the end of its window, like a
// buffered tail. With no bytes in flight that is the first window; with a 1 MiB
// window the seed covers four more (the bounds of TestWriteUnreadStreamBounds889).
func TestWaiterNothingGrantedIsResetAtTheBound1170(t *testing.T) {
	for _, tc := range []struct {
		initWin uint32
		windows int
	}{{0, 1}, {65535, 1}, {1 << 20, 5}} {
		m, s := waitRig1170(t, wait1170, tc.initWin)
		var w SendWait
		took, err := run1170(s, &w, 4<<20)
		w.End(s, s.Gen())
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("initWin %d: err %v, want os.ErrDeadlineExceeded", tc.initWin, err)
		}
		if lo, hi := time.Duration(tc.windows-1)*wait1170+wait1170/2, time.Duration(tc.windows)*wait1170+500*time.Millisecond; took < lo || took > hi {
			t.Fatalf("initWin %d: reset after %v, want about %d windows of %v", tc.initWin, took, tc.windows, wait1170)
		}
		if w.BlockedLoop() {
			t.Fatal("reported a blocked loop")
		}
		if n := m.stalledStreams(); n != 0 || waitHeld1170(m) != 0 {
			t.Fatalf("after the verdict: %d records, waitHeld %d", n, waitHeld1170(m))
		}
	}
}

// A grant that comes inside the bank's cover is sent, and the wait goes on.
func TestWaiterGrantInsideTheSeedIsServed1170(t *testing.T) {
	m, s := waitRig1170(t, wait1170, 1<<20)
	go func() {
		time.Sleep(3*wait1170 + wait1170/2)
		s.SetWindowSize(100000)
		m.notifyStreamWindow(s.ID)
	}()
	var w SendWait
	took, err := run1170(s, &w, 4<<20)
	if err != nil || took < 3*wait1170 {
		t.Fatalf("err %v after %v, want nil after the grant at 3.5 windows", err, took)
	}
	if m.stalledStreams() != 1 || waitHeld1170(m) != 4<<20 {
		t.Fatalf("while the handler sends: %d records, waitHeld %d, want its record", m.stalledStreams(), waitHeld1170(m))
	}
	w.End(s, s.Gen())
	if m.stalledStreams() != 0 || waitHeld1170(m) != 0 {
		t.Fatalf("after End: %d records, waitHeld %d", m.stalledStreams(), waitHeld1170(m))
	}
}

// The connection window the peer raises moves the high mark connInFlight is measured
// from, through the frame the processor applies (not only through
// UpdateConnectionWindow).
func TestConnWindowUpdateFrameMovesTheHighMark1170(t *testing.T) {
	p, m, _ := wdSetup889(t)
	m.connHigh.Store(65535)
	m.connectionWindow = 65535
	m.connectionWindow -= 40000 // 40000 sent
	if got := m.connInFlight(); got != 40000 {
		t.Fatalf("in flight %d, want 40000", got)
	}
	upd := func(n uint32) {
		if err := p.handleWindowUpdate(&http2.WindowUpdateFrame{FrameHeader: http2.FrameHeader{}, Increment: n}); err != nil {
			t.Fatal(err)
		}
	}
	upd(30000) // the client reads and returns 30000
	if got := m.connInFlight(); got != 10000 {
		t.Fatalf("after a return of 30000: %d in flight, want 10000", got)
	}
	upd(1 << 20) // the client opens the window far beyond what was sent
	if got := m.connInFlight(); got != 0 || m.connHigh.Load() < 1<<20 {
		t.Fatalf("after opening beyond: %d in flight, high %d", got, m.connHigh.Load())
	}
}

// A grant that arrives as the window ends is not lost to the timer: the wait
// returns to send it (the window is open when the timer fires), it is not judged.
func TestWaiterGrantAtTheWindowEndIsNotJudged1170(t *testing.T) {
	m, s := waitRig1170(t, wait1170, 0)
	_ = m
	// The window opens without a notice just before the timer: only the timer's own
	// look at the windows can find it.
	go func() {
		time.Sleep(wait1170 / 2)
		s.SetWindowSize(100000)
	}()
	var w SendWait
	_, err := run1170(s, &w, 4<<20)
	w.End(s, s.Gen())
	if err != nil {
		t.Fatalf("err %v: a window that opened was judged as a stall", err)
	}
}

// A cancelled stream is never expired (the peer's RST_STREAM came first): the wait
// ends with context.Canceled, whichever the timer or the cancel reached first.
func TestWaiterCancelledStreamIsNotExpired1170(t *testing.T) {
	m, s := waitRig1170(t, wait1170, 0)
	_ = m
	go func() {
		time.Sleep(wait1170 / 4)
		s.Cancel()
	}()
	var w SendWait
	_, err := run1170(s, &w, 4<<20)
	w.End(s, s.Gen())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	// And at the verdict itself: judgeWait of a cancelled stream does not expire.
	m2, s2 := waitRig1170(t, wait1170, 0)
	var w2 SendWait
	m2.beginWait(s2, s2.ID, &w2, 1000, s2.Gen())
	s2.Cancel()
	if v, _ := m2.judgeWait(s2, s2.ID, &w2, 1000); v != waitCancelled {
		t.Fatalf("verdict %v for a cancelled stream, want waitCancelled", v)
	}
	if m2.stalledStreams() != 0 {
		t.Fatal("the record of a cancelled stream stays")
	}
}

// The record of a live stream has gone (two goroutines on one stream, a removal that
// raced the timer): the wait is neither judged nor closed by anything else, so the
// judge remakes the record, without a seed, and goes on.
func TestWaiterMissingRecordIsRemade1170(t *testing.T) {
	m, s := waitRig1170(t, wait1170, 1<<20) // a seed of four windows, which a remade record must not have
	go func() {
		time.Sleep(wait1170 / 4)
		m.dropStall(s.ID)
	}()
	var w SendWait
	took, err := run1170(s, &w, 4<<20)
	w.End(s, s.Gen())
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err %v, want os.ErrDeadlineExceeded: a wait without a record was never judged", err)
	}
	if took > 3*wait1170+500*time.Millisecond {
		t.Fatalf("reset after %v: the remade record took the seed (4 windows) or was not judged", took)
	}
	// Direct: no record and a live stream is remade, held, with no seed.
	m2, s2 := waitRig1170(t, wait1170, 1<<20)
	var w2 SendWait
	if v, end := m2.judgeWait(s2, s2.ID, &w2, 5000); v != waitHeld || end == 0 {
		t.Fatalf("no record: verdict %v end %d, want waitHeld and a window", v, end)
	}
	m2.stallMu.Lock()
	st := m2.stalls[s2.ID]
	ok := st != nil && st.waiter && st.bank == 0 && st.pending == 5000 && m2.waitHeldLocked() == 5000
	m2.stallMu.Unlock()
	if !ok {
		t.Fatalf("the remade record: %+v, waitHeld %d", st, waitHeld1170(m2))
	}
}

// With WriteTimeout off there is no record and no timer: only a grant or the end of
// the stream ends the wait.
func TestWaiterWithoutAWriteTimeoutHasNoRecord1170(t *testing.T) {
	m, s := waitRig1170(t, 0, 1<<20)
	done := make(chan error, 1)
	var w SendWait
	go func() { done <- s.AwaitSendWindowRate(&w, 1<<20) }()
	awaitWaiters889(t, m, 1)
	if n := m.stalledStreams(); n != 0 {
		t.Fatalf("%d records with WriteTimeout off", n)
	}
	select {
	case err := <-done:
		t.Fatalf("returned %v with nothing granted", err)
	case <-time.After(150 * time.Millisecond):
	}
	s.Cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	w.End(s, s.Gen())
}

// The record's books: made by the first block, credited by NoteWaitSent, ended by
// End or by the bytes running out; waitHeld follows; the sweep does not see it.
func TestWaiterRecordBooks1170(t *testing.T) {
	m, s := waitRig1170(t, time.Hour, 0)
	other := stalled889(m, 3, 200000) // a buffered stream beside it (not a tail: the grace would hold it)
	var w SendWait
	if end := m.beginWait(s, s.ID, &w, 10000, s.Gen()); end == 0 {
		t.Fatal("no window")
	}
	if waitHeld1170(m) != 10000 || m.stalledStreams() != 2 {
		t.Fatalf("waitHeld %d, %d records", waitHeld1170(m), m.stalledStreams())
	}
	// The connection holds the buffer and the waiter's bytes.
	m.stallMu.Lock()
	h := m.heldLocked(0)
	m.stallMu.Unlock()
	if h != 210000 {
		t.Fatalf("the connection holds %d, want 210000", h)
	}
	m.NoteWaitSent(s.ID, 4000)
	m.stallMu.Lock()
	st := m.stalls[s.ID]
	got := st.released == 4000 && st.pending == 6000 && m.waitHeldLocked() == 6000
	m.stallMu.Unlock()
	if !got {
		t.Fatalf("after a credit: %+v waitHeld %d", st, waitHeld1170(m))
	}
	m.NoteWaitSent(other.ID, 100) // a buffered tail is credited by the flush, not by this
	m.stallMu.Lock()
	rel := m.stalls[other.ID].released
	m.stallMu.Unlock()
	if rel != 0 {
		t.Fatalf("NoteWaitSent credited a buffered record: %d", rel)
	}
	// The sweep skips it, in the verdicts and in the hint.
	if out := m.ExpiredWrites(time.Now().UnixNano()+int64(2*time.Hour), make([]uint32, 0, 8)); len(out) != 1 || out[0] != other.ID {
		t.Fatalf("the sweep expired %v, want only the buffered stream %d", out, other.ID)
	}
	m.dropStall(other.ID)
	if m.hints.WrDL.Load() != 0 {
		_ = m.ExpiredWrites(time.Now().UnixNano(), make([]uint32, 0, 8))
	}
	if h := m.NextWriteDeadline(); h != 0 {
		t.Fatalf("hint %d with only a waiter: the sweep would spin", h)
	}
	m.NoteWaitSent(s.ID, 6000) // the rest goes out: the record ends with it
	if m.stalledStreams() != 0 || waitHeld1170(m) != 0 {
		t.Fatalf("after the last bytes: %d records, waitHeld %d", m.stalledStreams(), waitHeld1170(m))
	}
	// End after the record is gone is a no-op, and a second End too.
	w.End(s, s.Gen())
	w.End(s, s.Gen())
	// dropAllStalls zeroes waitHeld.
	var w2 SendWait
	m.beginWait(s, s.ID, &w2, 777, s.Gen())
	m.dropAllStalls()
	if waitHeld1170(m) != 0 || m.stalledStreams() != 0 {
		t.Fatalf("dropAllStalls: waitHeld %d, %d records", waitHeld1170(m), m.stalledStreams())
	}
}

// The record follows the handler's own count of what is left, whatever the credits
// said: a send that was not credited does not leave the books ahead of the handler.
func TestWaiterBooksFollowTheHandlersCount1170(t *testing.T) {
	m, s := waitRig1170(t, time.Millisecond, 1<<20) // a seed: the verdict holds
	var w SendWait
	m.beginWait(s, s.ID, &w, 10000, s.Gen())
	time.Sleep(5 * time.Millisecond) // the window ended: due
	// The handler sent 3000 and the credit was not made; it tells its rest to the judge.
	m.judgeWait(s, s.ID, &w, 7000)
	m.stallMu.Lock()
	st := m.stalls[s.ID]
	got := st != nil && st.pending == 7000 && m.waitHeldLocked() == 7000
	m.stallMu.Unlock()
	if !got {
		t.Fatalf("the record after the judge: %+v waitHeld %d, want pending 7000", st, waitHeld1170(m))
	}
}

// A stream is a waiter or a buffer, never both; if it ever were, the sweep judges
// the record and the waiter's judge defers to it.
func TestWaiterAndBufferNeverHaveTwoJudges1170(t *testing.T) {
	m, s := waitRig1170(t, wait1170, 0)
	var w SendWait
	m.beginWait(s, s.ID, &w, 300000, s.Gen())
	s.BufferOutbound(make([]byte, 200000), true) // noteBuffered on a waiter's stream
	m.stallMu.Lock()
	st := m.stalls[s.ID]
	conv := st != nil && !st.waiter && m.waitHeldLocked() == 0 && st.pending == 500000
	m.stallMu.Unlock()
	if !conv {
		t.Fatalf("the record after a buffer joined: %+v waitHeld %d", st, waitHeld1170(m))
	}
	if v, _ := m.judgeWait(s, s.ID, &w, 300000); v != waitHeld {
		t.Fatalf("the waiter's judge decided %v on the sweep's record", v)
	}
	if out := m.ExpiredWrites(time.Now().UnixNano()+int64(wait1170)*2, make([]uint32, 0, 4)); len(out) != 1 {
		t.Fatalf("the sweep expired %v, want the stream (it judges now)", out)
	}
}

// The seed counts no more than what was sent since the stream's last record ended
// (review r2 F2): a dribbler that grants each Write's bytes late got a fresh seed of
// everything in flight for every Write.
func TestWriteSeedCountsOnlyWhatWasSentSinceTheLastRecord1170(t *testing.T) {
	_, m, _ := wdSetup889(t)
	m.initWin.Store(1 << 20) // 256 KiB of seed for a first record
	s := m.CreateStream(1)
	s.SetState(StateOpen)
	s.SetWindowSize(0)
	seed := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		return m.seedLocked(s)
	}
	if got := seed(); got != 256<<10 {
		t.Fatalf("first record: seed %d, want the cap (256 KiB)", got)
	}
	s.mu.Lock()
	s.recordEnded()
	s.mu.Unlock()
	if got := seed(); got != 0 {
		t.Fatalf("nothing sent since the last record: seed %d, want 0", got)
	}
	s.mu.Lock()
	s.noteWritten(5000)
	s.mu.Unlock()
	if got := seed(); got != 5000 {
		t.Fatalf("5000 sent since: seed %d, want 5000", got)
	}
	s.mu.Lock()
	s.noteWritten(1 << 30)
	s.mu.Unlock()
	if got := seed(); got != 256<<10 {
		t.Fatalf("a lot sent since: seed %d, want the cap", got)
	}
}

// The detached streaming path of D2 (inline StreamWriter): each Write that buffers
// ends a record when the buffer drains, and the next one's seed counts only what
// the Write sent before it buffered.
func TestInlineStreamWritesDoNotReSeed1170(t *testing.T) {
	_, m, _ := wdSetup889(t)
	m.initWin.Store(1 << 20)
	s := m.CreateStream(1)
	s.SetState(StateOpen)
	s.SetWindowSize(0)
	gen := s.Gen()
	bank := func() int {
		m.stallMu.Lock()
		defer m.stallMu.Unlock()
		if st := m.stalls[1]; st != nil {
			return st.bank
		}
		return -1
	}
	if _, _, _, err := m.StreamWrite(s, gen, make([]byte, 100000), discard889); err != nil {
		t.Fatal(err)
	}
	if got := bank(); got != 256<<10 {
		t.Fatalf("first Write: seed %d, want 256 KiB", got)
	}
	s.SetWindowSize(100000) // the client grants this Write's bytes: the buffer drains, the record ends
	m.FlushOutbound(s, discard889)
	if got := bank(); got != -1 {
		t.Fatalf("the record stays after the buffer drained: bank %d", got)
	}
	s.SetWindowSize(0)
	if _, _, _, err := m.StreamWrite(s, gen, make([]byte, 100000), discard889); err != nil {
		t.Fatal(err)
	}
	if got := bank(); got != 0 {
		t.Fatalf("second Write, which sent nothing before it buffered: seed %d, want 0", got)
	}
}

// The idle allowance (option B of celeris#1170): ceil(in flight / what the
// connection released in the window), between writeIdleWindows and the cap.
func TestIdleAllowance1170(t *testing.T) {
	_, m, _ := wdSetup889(t)
	m.connHigh.Store(int32(1 << 30))
	atomicStoreCW := func(cw int32) { m.connectionWindow = cw }
	for _, tc := range []struct {
		inflight int64
		rel      int64
		want     int
	}{
		{0, 100000, writeIdleWindows},
		{300000, 100000, writeIdleWindows},
		{300001, 100000, 4},
		{1000000, 100000, 10},
		{1600000, 100000, 16},
		{1600001, 100000, writeIdleCapWindows},
		{1 << 28, 1000, writeIdleCapWindows},
		{1 << 20, 0, writeIdleWindows}, // a connection that moves released something; 0 is no basis for a quotient
	} {
		atomicStoreCW(int32(1<<30 - tc.inflight))
		if got := m.idleAllowance(tc.rel); got != tc.want {
			t.Errorf("in flight %d, released %d: allowance %d, want %d", tc.inflight, tc.rel, got, tc.want)
		}
	}
	if writeIdleCapWindows != 16 {
		t.Errorf("the cap is %d, decided 16", writeIdleCapWindows)
	}
}

// connInFlight follows the highest the window has been, and does not go negative.
func TestConnInFlight1170(t *testing.T) {
	m := NewManager()
	if got := m.connInFlight(); got != 0 {
		t.Fatalf("new: %d", got)
	}
	m.UpdateConnectionWindow(1 << 20) // the peer opens the window
	atomicSub := func(n int32) { m.connectionWindow -= n }
	atomicSub(300000)
	if got := m.connInFlight(); got != 300000 {
		t.Fatalf("after sending 300000: %d", got)
	}
	m.UpdateConnectionWindow(100000) // the peer returns some (the high mark follows only above it)
	if got := m.connInFlight(); got != 200000 {
		t.Fatalf("after a grant of 100000: %d", got)
	}
	m.UpdateConnectionWindow(1 << 20) // opens more than it consumed: the new high
	if got := m.connInFlight(); got != 0 {
		t.Fatalf("after opening beyond the high mark: %d", got)
	}
}

// The bound of an unread stream on a connection that moves, with the allowance at
// its cap (a long queue ahead in the socket): the seed spends 1+k windows, the
// allowance holds it 16 more, the next is the verdict. The documented 21 for a
// window of 256 KiB or more, 17 for the default one. Without a queue ahead the
// bounds are those of TestWriteUnreadStreamBounds889.
func TestUnreadStreamBoundWithAQueueAhead1170(t *testing.T) {
	for _, tc := range []struct {
		initWin  uint32
		inflight int64
		want     int
	}{
		{65535, 100 << 20, 1 + writeIdleCapWindows},
		{1 << 20, 100 << 20, 4 + writeIdleCapWindows + 1},
		{1 << 20, 0, 8},
		{65535, 0, 4},
		{1 << 20, 700000, 4 + 7 + 1}, // ceil(700000/ 100000-ish): see rate below
	} {
		_, m, _ := wdSetup889(t)
		m.initWin.Store(tc.initWin)
		m.connHigh.Store(int32(1<<30 + 65535)) // wdSetup's window, as the peer opened it
		m.connectionWindow = int32(1<<30+65535) - int32(tc.inflight)
		stalled889(m, 1, 8<<20)
		a := m.CreateStream(3)
		a.SetState(StateOpen)
		a.SetWindowSize(0)
		a.BufferOutbound(make([]byte, 64<<20), true)
		w := firstReset889(m, 1, time.Now().UnixNano(), 60, func(int) {
			a.SetWindowSize(100000) // the sibling takes 100000 a window: the connection moves
			m.FlushOutbound(a, discard889)
			m.connectionWindow = int32(1<<30+65535) - int32(tc.inflight) // the client returns what it reads
		})
		if w != tc.want {
			t.Errorf("window %d, in flight %d: reset in window %d, want %d", tc.initWin, tc.inflight, w, tc.want)
		}
	}
}

// The allowance is fixed when the idle run begins: a queue that drains while the
// stream is idle does not shorten the run.
func TestIdleAllowanceIsFixedWhenTheRunBegins1170(t *testing.T) {
	_, m, _ := wdSetup889(t)
	m.initWin.Store(65535)
	m.connHigh.Store(int32(1<<30 + 65535))
	m.connectionWindow = int32(1<<30+65535) - 100<<20
	stalled889(m, 1, 8<<20)
	a := m.CreateStream(3)
	a.SetState(StateOpen)
	a.SetWindowSize(0)
	a.BufferOutbound(make([]byte, 64<<20), true)
	w := firstReset889(m, 1, time.Now().UnixNano(), 60, func(win int) {
		a.SetWindowSize(100000)
		m.FlushOutbound(a, discard889)
		if win > 1 {
			m.connHigh.Store(0) // the queue is gone after the first window: a re-derived allowance would be 3
		}
	})
	if w != 1+writeIdleCapWindows {
		t.Fatalf("reset in window %d, want %d (the allowance of the first idle window, 16)", w, 1+writeIdleCapWindows)
	}
}

// While an event loop is stuck in one batch for half a WriteTimeout (and at least
// 250 ms) a waiter that has waited a whole WriteTimeout is reset at its next
// judgement, whatever its seed and credit say; just under the threshold it is not.
func TestWaiterLoopBlockedFallback1170(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stuck  time.Duration // how long the loop has been in its batch when the waiter first blocks
		blocks bool
	}{
		{"just under the threshold", 200 * time.Millisecond, false},
		{"just over the threshold", 400 * time.Millisecond, true},
		{"no loop stuck", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s := waitRig1170(t, wait1170, 1<<20) // the seed would hold it for 5 windows
			lw := NewLoopWatch()
			slot := lw.Slot()
			if tc.stuck > 0 {
				// A loop that is always this far into a batch (it re-enters as time passes).
				quit := make(chan struct{})
				defer close(quit)
				stamp := func() { slot.Enter(time.Now().Add(-tc.stuck).UnixNano()) }
				stamp()
				go func() {
					for {
						select {
						case <-quit:
							return
						case <-time.After(2 * time.Millisecond):
							stamp()
						}
					}
				}()
			}
			w := SendWait{Loops: lw}
			took, err := run1170(s, &w, 4<<20)
			w.End(s, s.Gen())
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("err %v", err)
			}
			if tc.blocks {
				if !w.BlockedLoop() || took > 3*wait1170+500*time.Millisecond {
					t.Fatalf("blocked=%v after %v: want the fallback at the first window", w.BlockedLoop(), took)
				}
			} else if w.BlockedLoop() || took < 4*wait1170 {
				t.Fatalf("blocked=%v after %v: the rule's bound (5 windows) applies", w.BlockedLoop(), took)
			}
		})
	}
}

// An idle standby loop is not a stuck one: it stamps nothing (it waits), and a loop
// that left its batch clears its stamp.
func TestLoopWatch1170(t *testing.T) {
	lw := NewLoopWatch()
	a, b := lw.Slot(), lw.Slot()
	now := time.Now().UnixNano()
	if lw.BlockedFor(now, 1) {
		t.Fatal("idle loops read as stuck")
	}
	a.Enter(now - int64(time.Second))
	if !lw.BlockedFor(now, int64(500*time.Millisecond)) || lw.BlockedFor(now, int64(2*time.Second)) {
		t.Fatal("a batch of one second: stuck for 500 ms, not for 2 s")
	}
	a.Leave()
	b.Enter(now)
	if lw.BlockedFor(now, int64(time.Millisecond)) {
		t.Fatal("a batch that has just begun, or one that ended, reads as stuck")
	}
	var nilSlot *LoopSlot
	nilSlot.Enter(1)
	nilSlot.Leave() // a loop without a slot (a test) is not a crash
	if loopStuckFor(int64(time.Second)) != int64(500*time.Millisecond) || loopStuckFor(int64(100*time.Millisecond)) != int64(250*time.Millisecond) {
		t.Fatal("the threshold is half a WriteTimeout, at least 250 ms")
	}
}

// An idle run that begins in the tail branch has an allowance too (review r10 F3): a
// tail window, then a window in which a sibling's last bytes move the connection, was
// reset at once (the allowance was only set by the moving branch), where the rule before
// celeris#1170 held it.
func TestIdleRunThatBeginsInATailKeepsAnAllowance1170(t *testing.T) {
	m := NewManager()
	m.SetWriteTimeout(time.Second)
	a := &stall{held: 10, pending: 10, connHeld: 20}
	b := &stall{held: 10, pending: 10, connHeld: 20}
	m.stalls = map[uint32]*stall{1: a, 3: b}
	m.stallN.Store(2)
	now := time.Now().UnixNano()
	m.stallMu.Lock()
	defer m.stallMu.Unlock()
	h1 := m.holdsLocked(a, false, now)
	_ = m.holdsLocked(b, false, now)
	m.connReleased.Add(10) // b's bytes go out in window 2
	b.released, b.pending = 10, 0
	h2 := m.holdsLocked(a, false, now+int64(time.Second))
	if !h1 || !h2 {
		t.Fatalf("window 1 hold=%v, window 2 hold=%v (idle %d, allowance %d): a tail that waited one window and then saw the connection move was reset", h1, h2, a.idle, a.idleMax)
	}
}

// The allowance of an idle run is the largest the run has seen, within the cap
// (review r10 F3): a fast first window (the connection released a lot at once) must not
// fix a small allowance for the whole run.
func TestIdleAllowanceIsTheLargestOfTheRun1170(t *testing.T) {
	run := func(r1, r2 int64) int {
		m := NewManager()
		m.SetWriteTimeout(time.Second)
		m.connHigh.Store(16 << 20)
		m.connectionWindow = 0 // 16 MiB in flight
		st := &stall{held: 1 << 20, pending: 1 << 20, connHeld: 8 << 20}
		other := &stall{held: 1 << 20, pending: 1 << 20, connHeld: 8 << 20}
		m.stalls = map[uint32]*stall{1: st, 3: other}
		m.stallN.Store(2)
		now := time.Now().UnixNano()
		m.stallMu.Lock()
		defer m.stallMu.Unlock()
		for w := 1; w <= 60; w++ {
			r := r2
			if w == 1 {
				r = r1
			}
			m.connReleased.Add(r)
			now += int64(time.Second)
			if !m.holdsLocked(st, false, now) {
				return w
			}
		}
		return 0
	}
	steady := run(1<<20, 1<<20)
	fast := run(4<<20, 1<<20) // the run begins in a window in which 4 MiB were released
	if fast != steady {
		t.Fatalf("reset in window %d after a fast first window, %d when steady: the first window fixed the allowance", fast, steady)
	}
}
