package stream

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// The pool handler's wait for window as a minimum rate (celeris#1170, stage 3 of
// celeris#889).
//
// A pool handler whose response the connection's outbound budget could not buffer
// sends it itself (conn.h2ResponseAdapter.sendRest, WriteUse) and blocks in
// AwaitSendWindowRate while both windows are shut. It was bounded by an absolute
// deadline, WriteTimeout from the start of the wait (#906), whatever the client
// read, which cut a client that read at a healthy rate when the bytes ahead of its
// stream in the socket took longer than that. Now the wait is a record in the
// connection's stall books (write_deadline.go), judged by the rule of every
// stalled stream (holdsLocked: the floor per window, the bank and its seed, the
// connection's credit, the share and idle allowance, the tail grace), with these
// differences:
//
//   - the record is the handler's: made by its first blocking call, credited with the
//     bytes the handler sends (NoteWaitSent, or StreamWrite for a StreamWriter),
//     ended with the call (SendWait.End) or with the stream (dropStall);
//   - the judge is the handler, on a timer of its own armed to the end of the record's
//     window, not the sweep, so that the bound holds when the event loop cannot run
//     (a sync handler waiting for a lock this handler holds holds its loop, and a wait
//     that only the loop's sweep could end would end never): the sweep skips waiter
//     records, so one record has one judge and a stream is reset at most once;
//   - the pool handler's own bytes (waitHeld) count in what the connection holds, for
//     the tail grace.
//
// While an event loop of the server is stuck in one batch for as long as a window
// allows (LoopWatch), a waiter that has waited a whole WriteTimeout is bounded as
// #906 bounded it, whatever the client reads: the wait would otherwise be able to
// hold a lock that loop's handlers wait for for as long as the download takes.

// SendWait is the state of one wait of a pool handler for window: one response's
// sendRest, or one StreamWriter Write. The zero value is ready; End must be called
// when the call ends.
type SendWait struct {
	m      *Manager
	id     uint32
	start  int64 // when the wait first blocked (wall clock ns)
	made   bool  // the record was made (and not just re-made)
	failed bool  // judged and reset by the loop-blocked fallback
}

// BlockedLoop reports whether the wait ended because an event loop was stuck,
// not because the client was slow.
func (w *SendWait) BlockedLoop() bool { return w.failed }

// End ends the wait's record, if it has one still, and notes where the stream's
// next record's seed counts from. s must be the stream the wait was on, in the use
// the wait began (gen, from Stream.Gen; the sendRest of a plain handler passes
// s.Gen()).
func (w *SendWait) End(s *Stream, gen uint64) {
	m := w.m
	if m == nil {
		return
	}
	w.m = nil
	s.mu.Lock() // the use cannot end, nor the object be reset, while it is held
	if s.gen.Load() == gen && s.manager == m {
		m.dropWait(w.id)
		s.recordEnded()
	}
	s.mu.Unlock()
}

// NoteWaitSent credits the waiting pool handler of stream id with n bytes it has
// sent: they are released in the record's window, and no longer to be sent. A
// no-op if the stream has no waiter record (WriteTimeout off, or no wait).
func (m *Manager) NoteWaitSent(id uint32, n int) {
	if n <= 0 || m.stallN.Load() == 0 {
		return
	}
	m.stallMu.Lock()
	if st := m.stalls[id]; st != nil && st.waiter {
		st.released += n
		d := min(n, st.pending)
		st.pending -= d
		m.waitHeld -= int64(d)
		if st.pending <= 0 {
			m.delStallLocked(id)
		}
	}
	m.stallMu.Unlock()
}

// noteConnWindow records that the peer raised the connection window to cw: the
// highest it has been is where the bytes in flight are measured from.
func (m *Manager) noteConnWindow(cw int32) {
	for {
		cur := m.connHigh.Load()
		if cw <= cur || m.connHigh.CompareAndSwap(cur, cw) {
			return
		}
	}
}

// connInFlight is the bytes sent on the connection and not yet granted back at the
// connection level: the highest the connection window has been less what it is. A
// client that returns connection window as it reads (Go's Transport does, as its
// body is read) leaves exactly what is queued ahead of the next byte it reads;
// one that never does leaves the whole of what was sent, which is capped by the
// idle allowance's cap. Atomic loads only.
func (m *Manager) connInFlight() int64 {
	return max(int64(m.connHigh.Load())-int64(atomic.LoadInt32(&m.connectionWindow)), 0)
}

// idleAllowance is how many windows a stream below its share may be held on a
// connection that moves, given that the connection released connRel bytes in the
// window that ends: the windows the client needs to read what is in flight, at the
// rate it reads, between writeIdleWindows and writeIdleCapWindows. Atomic loads
// only (it is called with stallMu held).
func (m *Manager) idleAllowance(connRel int64) int {
	if connRel <= 0 {
		return writeIdleWindows
	}
	k := (m.connInFlight() + connRel - 1) / connRel
	return int(min(max(k, writeIdleWindows), writeIdleCapWindows))
}

// beginWait makes the waiter record of s for a wait with rest bytes still to send,
// and returns the end of its window, or 0 if the call needs none (WriteTimeout off).
// It remakes a record that has gone while the stream lives, without a seed (a record
// that vanished is not evidence of bytes in flight). Called with no lock held.
func (m *Manager) beginWait(s *Stream, id uint32, w *SendWait, rest int, gen uint64) int64 {
	wt := int64(m.wrTimeout)
	if wt <= 0 {
		return 0
	}
	// The seed reads the stream's books under its lock, taken before stallMu.
	seed := 0
	if !w.made {
		if err := s.UseLock(gen); err == nil {
			seed = m.seedLocked(s)
			s.UseUnlock()
		}
	}
	m.stallMu.Lock()
	st := m.stalls[id]
	if st == nil {
		now := time.Now().UnixNano() // read only when a record is made, not for every wait
		if m.stalls == nil {
			m.stalls = make(map[uint32]*stall)
		}
		st = &stall{armedAt: now, bank: seed, waiter: true, held: rest, pending: rest,
			connAt: m.connReleased.Load(), connHeld: m.heldLocked(int64(rest))}
		m.stalls[id] = st
		m.waitHeld += int64(rest)
		m.stallN.Store(int32(len(m.stalls)))
	}
	end := st.armedAt + wt
	m.stallMu.Unlock()
	if w.start == 0 {
		w.start = time.Now().UnixNano()
	}
	w.m, w.id, w.made = m, id, true
	return end
}

// waitVerdict is what judgeWait decided.
type waitVerdict uint8

const (
	waitHeld      waitVerdict = iota // go on waiting, the window was renewed or has not ended
	waitCancelled                    // the stream is cancelled (or its record is not this wait's): do not reset it
	waitExpired                      // reset it
)

// judgeWait is the verdict at the end of the window of the waiter record of s: the
// rule of holdsLocked, or the fallback of a stuck loop. It returns the end of the
// window the wait goes on in when it holds. Called by the waiter's timer; takes
// stallMu only.
func (m *Manager) judgeWait(s *Stream, id uint32, w *SendWait, rest int) (waitVerdict, int64) {
	if s.IsCancelled() {
		m.dropWait(id)
		return waitCancelled, 0
	}
	wt := int64(m.wrTimeout)
	now := time.Now().UnixNano()
	if lw := m.loops; lw != nil && now-w.start >= wt && lw.blockedFor(now, loopStuckFor(wt)) {
		m.dropWait(id)
		w.failed = true
		return waitExpired, 0
	}
	starved := s.windowSize.Load() > 0 && atomic.LoadInt32(&m.connectionWindow) <= 0
	m.stallMu.Lock()
	defer m.stallMu.Unlock()
	st := m.stalls[id]
	switch {
	case st == nil:
		// The record has gone while the stream lives (two goroutines writing one
		// stream, a removal racing the timer): remake it, without a seed, and go on:
		// a wait nobody judges would hold the connection for ever.
		if m.stalls == nil {
			m.stalls = make(map[uint32]*stall)
		}
		st = &stall{armedAt: now, waiter: true, held: rest, pending: rest,
			connAt: m.connReleased.Load(), connHeld: m.heldLocked(int64(rest))}
		m.stalls[id] = st
		m.waitHeld += int64(rest)
		m.stallN.Store(int32(len(m.stalls)))
		return waitHeld, now + wt
	case !st.waiter:
		return waitHeld, st.armedAt + wt // the sweep's record: it judges
	}
	if st.armedAt+wt > now {
		return waitHeld, st.armedAt + wt // not due: a grant renewed it, or the timer fired early
	}
	if d := int64(rest - st.pending); d != 0 { // the books follow the handler's
		m.waitHeld += d
		st.pending = rest
	}
	if m.holdsLocked(st, starved, now) {
		return waitHeld, st.armedAt + wt
	}
	m.delStallLocked(id)
	return waitExpired, 0
}

// dropWait ends the waiter record of id.
func (m *Manager) dropWait(id uint32) {
	m.stallMu.Lock()
	if st := m.stalls[id]; st != nil && st.waiter {
		m.delStallLocked(id)
	}
	m.stallMu.Unlock()
}

// AwaitSendWindowRate is AwaitSendWindow for a pool handler that has rest bytes to
// send, bounded by WriteTimeout as a minimum rate instead of an absolute deadline.
// It returns nil when both windows are open, context.Canceled when the stream is
// cancelled, and os.ErrDeadlineExceeded when the wait is judged not to make its
// rate (the caller then resets the stream), as AwaitSendWindow does at its
// deadline. w is the caller's SendWait for this response; call w.End when done.
// With WriteTimeout off there is no bound and no record.
func (s *Stream) AwaitSendWindowRate(w *SendWait, rest int) error {
	m := s.manager
	if m == nil {
		return nil
	}
	if s.IsCancelled() {
		return context.Canceled
	}
	if m.WindowsOpen(s) {
		return nil
	}
	return s.awaitSendWindowRate(m, s.ID, s.Context().Done(), s.Gen(), w, rest)
}

// AwaitSendWindowRateUse is AwaitSendWindowRate for a StreamWriter, which may
// outlive its handler (celeris#904): it returns ErrStreamEnded once gen is no
// longer the stream's use.
func (s *Stream) AwaitSendWindowRateUse(gen uint64, w *SendWait, rest int) error {
	if err := s.UseLock(gen); err != nil {
		return err
	}
	m := s.manager
	if m == nil {
		s.UseUnlock()
		return ErrStreamEnded
	}
	if m.WindowsOpen(s) {
		s.UseUnlock()
		return nil
	}
	done, id := s.Context().Done(), s.ID
	s.UseUnlock()
	return s.awaitSendWindowRate(m, id, done, gen, w, rest)
}

// awaitSendWindowRate is the wait of AwaitSendWindowRate and ...Use: it is
// awaitSendWindow (see its notes on lost wakeups) with the timer armed to the
// record's window end and, when it fires, a verdict instead of an expiry.
func (s *Stream) awaitSendWindowRate(m *Manager, id uint32, done <-chan struct{}, gen uint64, w *SendWait, rest int) error {
	open := func() bool { return m.WindowsOpen(s) }
	m.sendWindowWaiters.Add(1)
	defer m.sendWindowWaiters.Add(-1)
	sw := m.registerSendWaiter(s, id)
	defer m.unregisterSendWaiter(sw)

	end := m.beginWait(s, id, w, rest, gen)
	var expired <-chan time.Time
	if end != 0 {
		d := time.Until(time.Unix(0, end))
		if sw.timer == nil {
			sw.timer = time.NewTimer(d)
		} else {
			sw.timer.Reset(d)
		}
		expired = sw.timer.C
		defer sw.timer.Stop()
	}
	fire := func() error { // the window ended
		if s.IsCancelled() {
			return context.Canceled
		}
		if open() {
			return nil // a grant that came as the window ended is not judged: it is sent, and credited, first
		}
		switch v, next := m.judgeWait(s, id, w, rest); v {
		case waitCancelled:
			return context.Canceled
		case waitExpired:
			return os.ErrDeadlineExceeded
		default:
			sw.timer.Reset(time.Until(time.Unix(0, next)))
			return errStillWaiting
		}
	}
	for {
		if s.IsCancelled() {
			return context.Canceled
		}
		select {
		case <-expired:
			if err := fire(); err != errStillWaiting {
				return err
			}
			continue
		default:
		}
		if open() {
			return nil
		}
		select {
		case <-sw.ch:
		case <-expired:
			if err := fire(); err != errStillWaiting {
				return err
			}
		case <-done:
			return context.Canceled
		}
	}
}

// errStillWaiting is fire's "held": the wait goes on.
var errStillWaiting = &stillWaiting{}

type stillWaiting struct{}

func (*stillWaiting) Error() string { return "still waiting" }

// A loop's watch -------------------------------------------------------------

// LoopWatch is a server's record of whether any of its event loops is stuck in one
// batch. Each loop has a slot it stamps when it starts a batch of events and clears
// before it waits for the next; BlockedFor reads them. Server-wide on purpose:
// the loops that a lock-holding pool handler's wait freezes are the ones whose
// handlers wait for the lock, on any connection, not the waiter's own.
type LoopWatch struct {
	mu    sync.Mutex
	slots atomic.Pointer[[]*LoopSlot]
}

// LoopSlot is one loop's stamp. Padded: each loop writes its own twice a batch.
type LoopSlot struct {
	since atomic.Int64
	_     [56]byte
}

// NewLoopWatch returns an empty watch.
func NewLoopWatch() *LoopWatch { return &LoopWatch{} }

// Slot adds a loop to the watch and returns its slot.
func (w *LoopWatch) Slot() *LoopSlot {
	s := &LoopSlot{}
	w.mu.Lock()
	var cur []*LoopSlot
	if p := w.slots.Load(); p != nil {
		cur = *p
	}
	next := append(append(make([]*LoopSlot, 0, len(cur)+1), cur...), s)
	w.slots.Store(&next)
	w.mu.Unlock()
	return s
}

// Enter stamps the start of a batch at now (wall clock ns).
func (s *LoopSlot) Enter(now int64) {
	if s != nil {
		s.since.Store(now)
	}
}

// Leave clears the stamp: the loop is going to wait.
func (s *LoopSlot) Leave() {
	if s != nil {
		s.since.Store(0)
	}
}

// blockedFor reports whether some loop has been in one batch since at least d
// before now.
func (w *LoopWatch) blockedFor(now, d int64) bool {
	p := w.slots.Load()
	if p == nil {
		return false
	}
	for _, s := range *p {
		if t := s.since.Load(); t != 0 && now-t >= d {
			return true
		}
	}
	return false
}

// minLoopStuck is the shortest a loop's batch is taken as stuck: ordinary batches
// of a loaded loop are not.
const minLoopStuck = 250 * time.Millisecond

// loopStuckFor is how long a loop must have been in one batch to switch the
// fallback on: half a WriteTimeout, and at least minLoopStuck.
func loopStuckFor(wt int64) int64 {
	return max(wt/2, int64(minLoopStuck))
}

// SetLoopWatch sets the server's watch on its event loops. Set once, before the
// first frame; nil (the default) never reports a loop stuck.
func (m *Manager) SetLoopWatch(w *LoopWatch) { m.loops = w }
