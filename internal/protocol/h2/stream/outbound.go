package stream

import (
	"context"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// OutboundBudget is how many bytes of response DATA one HTTP/2 connection's
// streams may hold in their OutboundBuffers while they wait for the peer's
// WINDOW_UPDATE (celeris#893). A handler's response is staged whole: what the
// windows do not let it send at once is copied into its stream's buffer and
// sent as the peer grants window. Without a bound, a peer that opens many
// streams for a large response and never grants window made the server copy
// every one of those bodies (about 150 MiB per connection for 99 streams of a
// 1.5 MiB body). The budget is charged with every byte buffered and refunded
// as the bytes are sent or their stream ends. While it is spent:
//
//   - a new stream's handler does not run inline on the event loop, where it
//     would have to buffer whatever the windows refuse; it runs on the HTTP/2
//     worker pool instead (Processor.runHandler);
//   - a handler on the pool is not given a copy (Stream.TryBufferOutbound):
//     it sends the rest of its body itself, through the write queue, as the
//     windows open (Stream.AwaitSendWindow), the way net/http's server blocks
//     a handler's Write on flow control. It makes no copy while it waits.
//
// A response is never refused for its size: with nothing held, any one body
// is buffered whole. So the copies a connection makes for the peer's window
// come to at most about the budget plus one body. That bounds what it holds
// for a body its streams share (a static asset, the embedded Swagger UI
// bundle). It does not bound a body built per request (c.JSON, a rendered
// page): a waiting handler keeps its own body alive until it is sent, as a
// net/http handler blocked in Write does, so a connection's waiting handlers
// hold up to one such body each, for up to MAX_CONCURRENT_STREAMS streams.
// 4 MiB matches the HTTP/1 per-connection backlog limit (the engines'
// maxPendingBytes).
//
// A waiting handler is blocked while the peer withholds window, for no
// longer than the server's WriteTimeout from its write: then its stream is
// reset (AwaitSendWindow's deadline, set by the conn layer). The connection's
// read and idle timeouts do not bound it: they measure the peer's silence, and
// a peer that withholds window but sends any frame, a PING, keeps them from
// firing. Whatever waits for a waiting handler waits with it: a request that
// needs a lock it holds across its write, a cache's coalesced fill it leads
// (celeris#913). On std that stalls only those requests. On epoll, io_uring
// and adaptive a sync handler that waits for it holds its event loop, and so
// every connection on that loop, until the deadline. With WriteTimeout
// disabled there is no deadline: the wait lasts until the peer grants window
// or the connection closes, and never ends if the loop that would see either
// is the one waiting.
const OutboundBudget = 4 << 20

// OutboundHeld returns how many bytes this connection's streams hold in their
// OutboundBuffers, waiting for the peer's window (celeris#893).
func (m *Manager) OutboundHeld() int64 { return m.outboundHeld.Load() }

// overOutboundBudget reports whether the connection's buffered response bytes
// have reached OutboundBudget.
func (m *Manager) overOutboundBudget() bool {
	return m.outboundHeld.Load() >= OutboundBudget
}

// chargeOutbound counts n bytes a stream has just buffered.
func (m *Manager) chargeOutbound(n int) {
	if n > 0 {
		m.outboundHeld.Add(int64(n))
	}
}

// refundOutbound uncounts n buffered bytes that have been sent or dropped
// with their stream.
func (m *Manager) refundOutbound(n int) {
	if n > 0 {
		m.outboundHeld.Add(-int64(n))
	}
}

// sendWaiter is one handler waiting in awaitSendWindow. Each wait has its own
// waiter and its own wakeup channel, so a frame wakes the waiters it can help
// and no others (celeris#889: a connection's WINDOW_UPDATE used to wake every
// handler of the connection, each of which re-checked its two windows and went
// back to sleep, 4.4 us of CPU per frame with 99 waiters against 0.95 us
// without; a stream's WINDOW_UPDATE woke them all too, 602 ns against 12 ns for
// a stream nobody waits on, and the deadline of each wait woke them all once
// more).
type sendWaiter struct {
	s     *Stream
	id    uint32        // s.ID, read while the use was live
	ch    chan struct{} // capacity 1; a send is a wakeup that stays until the waiter takes it; never closed
	timer *time.Timer   // the wait's deadline, made once per waiter object and re-armed per wait (Reset), stopped when the wait ends
}

// wake leaves a wakeup for the waiter if it has none pending.
func (w *sendWaiter) wake() {
	select {
	case w.ch <- struct{}{}:
	default:
	}
}

// registerSendWaiter adds a waiter for s (whose ID is id) to the connection's
// set. It must come before the waiter checks the windows it waits for (see the
// argument at awaitSendWindow).
func (m *Manager) registerSendWaiter(s *Stream, id uint32) *sendWaiter {
	w := sendWaiterPool.Get().(*sendWaiter)
	w.s, w.id = s, id
	m.sendWindowMu.Lock()
	m.sendWaiters = append(m.sendWaiters, w)
	m.sendWindowMu.Unlock()
	return w
}

// unregisterSendWaiter removes w from the set and returns it to the pool; after
// it returns no notifier holds w, except one that collected it under the lock
// and has not woken it yet (a batch wake, notifyStreamWindow): it may leave a
// token in the channel of a waiter that is in the pool, or already in another
// wait. That wait wakes once for nothing and re-checks, which is all a stale
// token costs.
func (m *Manager) unregisterSendWaiter(w *sendWaiter) {
	m.sendWindowMu.Lock()
	for i, x := range m.sendWaiters {
		if x == w {
			last := len(m.sendWaiters) - 1
			m.sendWaiters[i] = m.sendWaiters[last]
			m.sendWaiters[last] = nil
			m.sendWaiters = m.sendWaiters[:last]
			break
		}
	}
	m.sendWindowMu.Unlock()
	select { // a wakeup nobody took
	case <-w.ch:
	default:
	}
	w.s = nil
	sendWaiterPool.Put(w)
}

// sendWaiterPool holds the waiters (and their channels) of finished waits: a
// wait that blocks costs no allocation for its waiter.
var sendWaiterPool = sync.Pool{New: func() any { return &sendWaiter{ch: make(chan struct{}, 1)} }}

// notifyStreamWindow wakes the handlers waiting for stream id's window, after
// the peer has granted it (a stream WINDOW_UPDATE; celeris#893). A waiter of
// another stream is not woken: it waits for a window of its own. It costs an
// atomic load unless a handler is waiting, and a scan of the waiters, with one
// comparison each, while one is. Safe on any goroutine and under any lock:
// sendWindowMu is a leaf.
func (m *Manager) notifyStreamWindow(id uint32) {
	if m.sendWindowWaiters.Load() == 0 {
		return
	}
	var wake [wakeBatch]*sendWaiter
	n := 0
	m.sendWindowMu.Lock()
	for _, w := range m.sendWaiters {
		if w.id == id {
			if n == len(wake) {
				w.wake() // more than a batch: this one under the lock
				continue
			}
			wake[n] = w
			n++
		}
	}
	m.sendWindowMu.Unlock()
	for _, w := range wake[:n] {
		w.wake()
	}
}

// notifyOpenStreamWindows wakes the handlers whose own stream window has room,
// after the connection's window has grown (a connection WINDOW_UPDATE) or the
// stream windows have changed together (SETTINGS_INITIAL_WINDOW_SIZE;
// celeris#893). A handler whose stream window is closed cannot send whatever
// the connection's window holds: it is woken by the WINDOW_UPDATE for its
// stream (notifyStreamWindow), which is the only thing that can end its wait.
// Same cost and locking as notifyStreamWindow.
func (m *Manager) notifyOpenStreamWindows() {
	if m.sendWindowWaiters.Load() == 0 {
		return
	}
	var wake [wakeBatch]*sendWaiter
	n := 0
	m.sendWindowMu.Lock()
	for _, w := range m.sendWaiters {
		if w.s.windowSize.Load() > 0 {
			if n == len(wake) {
				w.wake()
				continue
			}
			wake[n] = w
			n++
		}
	}
	m.sendWindowMu.Unlock()
	for _, w := range wake[:n] {
		w.wake()
	}
}

// wakeBatch is how many waiters a notifier collects under sendWindowMu to wake
// after it releases it. A wakeup readies a goroutine, which costs far more than
// the comparison that chose it; done under the lock, every waiter that wakes
// and returns to the set (unregisterSendWaiter, registerSendWaiter) queues
// behind the rest of the notifier's wakeups. The waiter may be gone by the time
// it is woken, and back in the pool or in another wait: the token it gets then
// costs that wait one re-check (unregisterSendWaiter).
const wakeBatch = 32

// TryBufferOutbound buffers data as BufferOutbound does when the connection's
// outbound budget has room for it, or holds nothing at all; otherwise it
// buffers nothing and reports false (celeris#893). The caller then sends data
// itself as the windows open (AwaitSendWindow) instead of copying it past the
// budget, and must not buffer any of it later.
//
// Only a handler on its own goroutine can wait. For a stream that is not on
// the worker pool (an inline handler, on the event loop; a stream with no
// manager) data is buffered as before and the result is true: the event loop
// keeps inline handlers off a connection that is over its budget instead.
//
// A stream that would have run inline but was put on the pool by the budget
// (flagBudgetPool) is never buffered, whatever the budget holds by now: it
// sends everything through the write queue, one chunk after another. Since
// celeris#903 a flush of buffered DATA also goes through the queue, behind the
// stream's HEADERS and the chunks already queued, so buffering such a stream's
// tail would no longer put DATA ahead of its HEADERS; the rule is kept as
// #906 made it, and relaxing it is a separate change.
func (s *Stream) TryBufferOutbound(data []byte, endStream bool) bool {
	m := s.manager
	f := s.flags.Load()
	if m == nil || f&flagAsyncRunning == 0 {
		s.BufferOutbound(data, endStream)
		return true
	}
	if f&flagBudgetPool != 0 {
		return false
	}
	n := int64(len(data))
	for {
		held := m.outboundHeld.Load()
		if held > 0 && held+n > OutboundBudget {
			return false
		}
		if m.outboundHeld.CompareAndSwap(held, held+n) {
			break
		}
	}
	s.bufferOutbound(data, endStream)
	return true
}

// AwaitSendWindow blocks the pool handler of s until both of s's send windows
// are open, so some of its DATA can be sent (celeris#893), and then returns
// nil. It returns context.Canceled when s is cancelled: the peer reset it, or
// its connection closed (Manager.Close cancels every stream), and nothing
// more is to be sent. It returns os.ErrDeadlineExceeded once deadline has
// passed, whether or not the windows are open (a zero deadline never
// passes): the caller is then to reset the stream. Passing the same deadline
// to every call for one response bounds the whole of its wait, so a peer that
// grants window a few bytes at a time cannot stretch it.
//
// It holds no lock while it waits, and the deadline needs nothing on the
// event loop to fire: a timer (time.AfterFunc) ends the wait. On the native
// engines the loop may be the thing waiting (a sync handler blocked on a lock
// this handler holds, or on a coalesced call it leads), and then it cannot
// process the WINDOW_UPDATE, the reset or the close that would end the wait.
// The connection's read and idle timeouts do not end it either (see
// OutboundBudget).
func (s *Stream) AwaitSendWindow(deadline time.Time) error {
	m := s.manager
	if m == nil {
		return nil
	}
	if s.IsCancelled() {
		return context.Canceled
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return os.ErrDeadlineExceeded
	}
	if m.WindowsOpen(s) {
		return nil
	}
	return s.awaitSendWindow(m, s.ID, s.Context().Done(), deadline)
}

// AwaitSendWindowUse is AwaitSendWindow for a StreamWriter, which may outlive
// its handler (celeris#904): gen is its use token (Stream.Gen), and the call
// returns ErrStreamEnded once that use is over, never touching the object's
// next use. It also starts the deadline late: *deadline is set to timeout
// from now (if timeout > 0 and *deadline is still zero) only when the call has
// to wait, so a Write that finds the windows open reads no clock. A set
// deadline is kept across calls, so it bounds the whole of one Write.
func (s *Stream) AwaitSendWindowUse(gen uint64, timeout time.Duration, deadline *time.Time) error {
	if err := s.UseLock(gen); err != nil {
		return err
	}
	m := s.manager
	if m == nil {
		s.UseUnlock()
		return ErrStreamEnded
	}
	if !deadline.IsZero() && !time.Now().Before(*deadline) {
		s.UseUnlock()
		return os.ErrDeadlineExceeded
	}
	if m.WindowsOpen(s) {
		s.UseUnlock()
		return nil
	}
	if deadline.IsZero() && timeout > 0 {
		*deadline = time.Now().Add(timeout)
	}
	// This use's context, taken while the use is known to be live: a context
	// made after the object was reset would be handed to its next use.
	done, id := s.Context().Done(), s.ID
	s.UseUnlock()
	return s.awaitSendWindow(m, id, done, *deadline)
}

// awaitSendWindow is the wait of AwaitSendWindow and AwaitSendWindowUse: m,
// id and done are the live use's, taken by the caller.
//
// The wait cannot lose a wakeup. The waiter is counted and registered, with its
// own channel, before it checks anything, and a grant (the CAS that raises a
// window) happens before its notifier looks for waiters. Take one grant and one
// waiter. If the notifier's scan, under sendWindowMu, finds the waiter, it
// leaves a token in the waiter's channel, which stays until the waiter takes
// it whether the waiter is in its select yet or not, and the waiter then
// re-checks both windows. If the scan does not find it (or the fast path saw no
// waiter counted), the waiter counted and registered after that point, so after
// the grant, and its first check sees the window. A notifier skips a waiter
// only for a window the waiter cannot use: a connection frame skips the one
// whose stream window is closed. That waiter needs both windows to open, and
// the grant that opens its stream window is a notifier of its own, with the
// same argument, so its last grant either wakes it or is seen by its check.
// A deadline is one more source, a case of the waiter's own select: no
// notifier is involved, and no other waiter is woken by it.
//
// One more premise: a window rises only through a notifier (a WINDOW_UPDATE, a
// SETTINGS change) or through ReserveSendWindow's refund, which notifies too;
// a window that dips to 0 and back inside a reservation is seen by the scan of
// a connection frame as closed, and the refund's notice (sent when the refund
// reopens a window that read as closed) covers that. A scan that read the
// window as open woke the waiter; one that read it as closed is followed by a
// rise from closed, which is a notifier's or such a refund.
func (s *Stream) awaitSendWindow(m *Manager, id uint32, done <-chan struct{}, deadline time.Time) error {
	open := func() bool { return m.WindowsOpen(s) }
	m.sendWindowWaiters.Add(1)
	defer m.sendWindowWaiters.Add(-1)
	w := m.registerSendWaiter(s, id)
	defer m.unregisterSendWaiter(w)
	// The deadline is a timer channel the waiter owns: armed once for the wait
	// (Reset: no allocation, and no goroutine at expiry, which time.AfterFunc
	// needs), stopped when the wait ends. It is a select case, and a wakeup does
	// not re-arm it: the loop below never touches the timer. Before, an AfterFunc
	// per wait set a flag and woke every waiter of the connection (celeris#889).
	var expired <-chan time.Time
	if !deadline.IsZero() {
		if w.timer == nil {
			w.timer = time.NewTimer(time.Until(deadline))
		} else {
			w.timer.Reset(time.Until(deadline))
		}
		expired = w.timer.C
		defer w.timer.Stop()
		// The timer is armed per wait, not once per response (celeris#889
		// item 7; kept on purpose). A response only waits when the peer
		// withholds window, and a reader that is over the connection's
		// outbound budget with its stream windows open, the connection window
		// being the bottleneck, waits once per chunk, so it arms and stops this
		// timer once per connection WINDOW_UPDATE. Arming it once in the caller
		// (sendRest) and passing the expiry down would save that, at the price
		// of giving the timer, and its Stop on every exit of every caller, to
		// code that does not own the wait. The cost is paid only while a
		// handler waits for a peer that withholds window, and no more than
		// MAX_CONCURRENT_STREAMS of them can (BenchmarkConnWindowReader892
		// measures it, with and without the deadline).
	}
	for {
		if s.IsCancelled() {
			return context.Canceled
		}
		select { // a deadline that has passed ends the wait whether or not the windows are open
		case <-expired:
			return os.ErrDeadlineExceeded
		default:
		}
		if open() {
			return nil
		}
		select {
		case <-w.ch:
		case <-expired:
			return os.ErrDeadlineExceeded
		case <-done:
			return context.Canceled
		}
	}
}

// WindowsOpen reports whether both of s's send windows have room, so some
// DATA of s can be sent (celeris#893).
func (m *Manager) WindowsOpen(s *Stream) bool {
	return s.windowSize.Load() > 0 && atomic.LoadInt32(&m.connectionWindow) > 0
}

// RunsOnPool reports whether s is served by a worker-pool goroutine, one that
// can wait for the peer's window (AwaitSendWindow). An inline handler runs on
// the event loop, which must not wait: there is nobody else to read the
// WINDOW_UPDATE that would end the wait.
func (s *Stream) RunsOnPool() bool {
	return s.manager != nil && s.flags.Load()&flagAsyncRunning != 0
}

// OutboundSend carries the DATA of one send to the connection (celeris#903).
// It is called with the stream's lock held and must consume data, or copy it,
// before it returns: data is a view of the stream's OutboundBuffer or of the
// caller's bytes. endStream marks the last DATA of the stream; data may be
// empty only with endStream.
type OutboundSend func(streamID uint32, endStream bool, data []byte)

// clampWindowRequest is n as a ReserveSendWindow request.
func clampWindowRequest(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}

// FlushOutbound sends as much of s's buffered DATA as both of its send
// windows allow, reserving and debiting both (RFC 9113 §6.9.1) for exactly the
// bytes it sends, and reports whether that was the whole buffer and the stream
// is done: it carried END_STREAM (the handler ended the response), or the
// handler returned without ending it (abandoned; nothing more will come, and
// the stream is to be released without END_STREAM). Whatever the windows
// refuse stays buffered for the next WINDOW_UPDATE. It is called on the event
// loop for a WINDOW_UPDATE or SETTINGS_INITIAL_WINDOW_SIZE, and by a pool
// handler that has just buffered the tail of its response (celeris#903).
//
// The stream's lock is held from the reservation to the send, so two callers
// never send the same bytes, and what is sent is dropped from the front of
// the buffer in place (bytes.Buffer.Next): the work of a flush is in
// proportion to the bytes it sends, not to the bytes that stay buffered
// (celeris#911). A stream that is no longer m's (released, and perhaps in use
// on another connection) is left alone.
func (m *Manager) FlushOutbound(s *Stream, send OutboundSend) (finished bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := s.OutboundBuffer
	if s.manager != m || buf == nil || buf.Len() == 0 {
		return false
	}
	n := int(m.ReserveSendWindow(s, clampWindowRequest(buf.Len())))
	if n <= 0 {
		return false // no room on at least one window: everything stays buffered
	}
	last := n == buf.Len()
	end := last && s.OutboundEndStream
	finished = last && (s.OutboundEndStream || s.outboundAbandoned)
	send(s.ID, end, buf.Next(n))
	m.refundOutbound(n) // celeris#893
	m.noteReleased(s.ID, n)
	if buf.Len() == 0 {
		buf.Reset()
	}
	return finished
}

// StreamWrite is one step of a StreamWriter's Write (celeris#904), taken
// under the stream's lock in one go (the check that gen is still s's use, the
// reservation, the send or the buffering), so a write that outlived its handler
// debits no window of the object's next use and sends or buffers nothing for it.
// It returns how many bytes of data it took, the stream's ID, and whether the
// stream is a pool handler's:
//
//   - On a pool stream (RunsOnPool) it sends what both windows allow now, n
//     bytes (0 when a window is shut), and the caller waits for the windows
//     (AwaitSendWindowUse) and calls again with the rest.
//   - On any other stream (an inline handler on the event loop) it cannot wait:
//     it takes all of data, sending what the windows allow and buffering the
//     rest.
//   - A HEAD response has no body: the data is taken and dropped.
//
// It returns ErrStreamEnded when gen is no longer s's use, and the context
// error when the stream was reset or its connection closed.
func (m *Manager) StreamWrite(s *Stream, gen uint64, data []byte, send OutboundSend) (n int, id uint32, onPool bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.useErr(gen); err != nil {
		return 0, 0, false, err
	}
	if s.manager != m {
		return 0, 0, false, ErrStreamEnded
	}
	id = s.ID
	if s.IsHEAD || len(data) == 0 {
		return len(data), id, false, nil
	}
	if !s.RunsOnPool() {
		m.sendOrBufferLocked(s, data, send)
		s.respWritten += int64(len(data))
		return len(data), id, false, nil
	}
	n = int(m.ReserveSendWindow(s, clampWindowRequest(len(data))))
	if n > 0 {
		send(id, false, data[:n])
		s.respWritten += int64(n)
	}
	return n, id, true, nil
}

// sendOrBufferLocked sends data on s for a caller that cannot wait for the
// peer's window, an inline handler on the event loop streaming its response
// (celeris#904): what both windows allow goes to send now, and the rest is
// buffered on s, charged to the connection's outbound budget, to follow as the
// peer grants window. While anything is buffered, more data joins the buffer,
// so the bytes of one stream reach the connection in the order they were
// written. Whether to send or buffer is decided under the stream's lock (held
// by the caller, StreamWrite), so a flush on the event loop and a detached
// goroutine's write cannot reorder; the same lock is the release's, so a
// goroutine that outlived its handler cannot put bytes in an object that is on
// its way to the pool, in the pool, or in use for another stream.
func (m *Manager) sendOrBufferLocked(s *Stream, data []byte, send OutboundSend) {
	n := 0
	if s.OutboundBuffer == nil || s.OutboundBuffer.Len() == 0 {
		n = int(m.ReserveSendWindow(s, clampWindowRequest(len(data))))
		if n > 0 {
			send(s.ID, false, data[:n])
		}
	}
	if n < len(data) {
		if s.OutboundBuffer == nil {
			s.OutboundBuffer = getBuf()
		}
		s.OutboundBuffer.Write(data[n:])
		m.chargeOutbound(len(data) - n)
		m.noteBuffered(s, len(data)-n)
	}
}

// EndOutbound ends s's response body (celeris#904): END_STREAM goes to send
// at once when nothing is buffered, and otherwise rides on the last of the
// buffered DATA, which the flush sends as the peer grants window, so that
// END_STREAM never reaches the connection ahead of DATA written before it.
// It is refused, sending and marking nothing, when gen is no longer s's use
// or the stream was reset (RFC 9113 §5.1: no frame follows a RST_STREAM),
// as StreamWrite is.
func (m *Manager) EndOutbound(s *Stream, gen uint64, send OutboundSend) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.useErr(gen); err != nil {
		return err
	}
	if s.manager != m {
		return ErrStreamEnded
	}
	s.flags.Or(flagRespEnded)
	if s.OutboundBuffer != nil && s.OutboundBuffer.Len() > 0 {
		s.OutboundEndStream = true
		return nil
	}
	send(s.ID, true, nil)
	return nil
}

// DeclaredInvalid is the content-length a response's headers declare when
// they declare one that cannot be held to: a value that is not a plain run of
// digits ("+5", "5.0", "abc", an empty one), or two that differ (a list such as
// "5, 6", or two headers). Such a response is not ended when its handler
// returns without closing the writer, it is reset (RFC 9110 §8.6 makes a
// recipient treat the message as invalid), whatever it wrote. The declared
// value the processor reports is -1 for none declared.
const DeclaredInvalid int64 = -2

// parseContentLength reads the content-length header values vs (one per
// header line) as RFC 9110 §8.6 has it: a plain run of digits, or a list of
// identical such values, which stands for that one. It returns the length, -1
// for no content-length header, or DeclaredInvalid.
func parseContentLength(vs []string) int64 {
	declared := int64(-1)
	for _, v := range vs {
		for {
			item, rest, more := strings.Cut(v, ",")
			item = strings.TrimSpace(item)
			if item == "" || len(item) > 18 {
				return DeclaredInvalid
			}
			var n int64
			for i := 0; i < len(item); i++ {
				c := item[i]
				if c < '0' || c > '9' {
					return DeclaredInvalid
				}
				n = n*10 + int64(c-'0')
			}
			if declared >= 0 && declared != n {
				return DeclaredInvalid
			}
			declared = n
			if !more {
				break
			}
			v = rest
		}
	}
	return declared
}

// NoteStreamHeader records that s's response is a StreamWriter's: the
// HEADERS went out without END_STREAM, so the response is not whole until its
// handler closes the writer, and the processor settles it if the handler
// returns without (Processor.settleStreamed). It also records the
// content-length the headers declare, which a response ending is held to: a
// body that is not exactly that long is reset, not ended (RFC 9110 §8.6,
// RFC 9113 §8.1.1), and so is any body of a content-length that is not a plain
// number (DeclaredInvalid). A HEAD response and a status that has no content
// (1xx, 204, 304) declare none. Only atomics, so a ResponseWriter may call it
// with the stream's lock held for reading (UseLock).
func (s *Stream) NoteStreamHeader(status int, headers [][2]string) {
	declared := int64(-1)
	if !s.IsHEAD && status >= 200 && status != 204 && status != 304 {
		var one [1]string // the usual single content-length costs no allocation
		vs := one[:0]
		for _, h := range headers {
			if len(h[0]) == len("content-length") && strings.EqualFold(h[0], "content-length") {
				vs = append(vs, h[1])
			}
		}
		if len(vs) > 0 {
			declared = parseContentLength(vs)
		}
	}
	s.respDeclared.Store(declared + 1)
	s.flags.Or(flagStreamed)
}

// FailResponse records that the handler failed after the response was under
// way, with the failure swallowed above the processor: the router recovers a
// panic and answers a handler's error with a 500 only when nothing was sent, so
// a StreamWriter response that the handler broke off returns from HandleStream
// as if it had finished. On HTTP/2 the processor then resets the stream
// instead of ending it (a truncated body must not look complete), whatever
// content-length the response declared. It does nothing for an HTTP/1 stream,
// whose object lives on for the connection's next request, and for a response
// that is not a StreamWriter's (a response written whole is whole).
func (s *Stream) FailResponse() {
	if s.ProtoMajor() == 2 {
		s.flags.Or(flagRespFailed)
	}
}

// responseOutcome is how a handler that is over left its stream's response.
type responseOutcome uint8

const (
	// outcomeNone: no response was started: no HEADERS, nothing to end.
	outcomeNone responseOutcome = iota
	// outcomeSettled: nothing is left to do: the response was written whole
	// (WriteResponse), or its END_STREAM is sent or queued.
	outcomeSettled
	// outcomeComplete: a streamed response that was not closed, with every
	// byte its headers declared (or none declared) written: it can be ended.
	outcomeComplete
	// outcomeShort: a streamed response that was not closed and whose body is
	// not as long as its headers declared (short of it, or over it; or the
	// declaration cannot be held to, DeclaredInvalid): it must not be ended.
	outcomeShort
)

// responseOutcome reports how s's response stands, with the body bytes a
// StreamWriter wrote and the content-length it declared (-1 for none,
// DeclaredInvalid for one that cannot be held to). A
// response that is not a StreamWriter's costs two atomic loads and no lock.
func (s *Stream) responseOutcome() (o responseOutcome, written, declared int64) {
	declared = -1
	if !s.headersSent.Load() {
		return outcomeNone, 0, declared
	}
	if s.flags.Load()&flagStreamed == 0 {
		return outcomeSettled, 0, declared
	}
	// END_STREAM is marked under the stream's lock (EndOutbound), so the
	// check is made under it too.
	s.mu.RLock()
	written, fl := s.respWritten, s.flags.Load()
	s.mu.RUnlock()
	declared = s.respDeclared.Load() - 1
	switch {
	case fl&flagRespEnded != 0:
		return outcomeSettled, written, declared
	case fl&flagRespFailed != 0, declared == DeclaredInvalid:
		return outcomeShort, written, declared
	case declared < 0 || declared == written:
		return outcomeComplete, written, declared
	}
	return outcomeShort, written, declared
}

// DiscardOutbound drops what s still buffers for the peer's window and gives
// it back to the connection's outbound budget, for a stream the server resets:
// the peer will not read it (RFC 9113 §5.1: no frame follows a RST_STREAM),
// and a flush of it must send nothing.
func (m *Manager) DiscardOutbound(s *Stream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manager != m {
		return
	}
	if buf := s.OutboundBuffer; buf != nil && buf.Len() > 0 {
		m.refundOutbound(buf.Len())
		buf.Reset()
	}
	m.dropStall(s.ID)
	s.OutboundEndStream = false
	s.outboundAbandoned = false
}

// AbandonOutbound is called when an inline handler returns with DATA still
// buffered (celeris#904). A StreamWriter that was never closed leaves the
// response unended; the buffer is still sent as the peer grants window, and
// the stream is released once it is out, but END_STREAM is not invented: a
// truncated body must not look complete to the client. It reports whether
// anything is buffered, that is whether the stream stays for the event loop.
func (s *Stream) AbandonOutbound() (pending bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending = s.OutboundBuffer != nil && s.OutboundBuffer.Len() > 0
	if pending && !s.OutboundEndStream {
		s.outboundAbandoned = true
	}
	return pending
}
