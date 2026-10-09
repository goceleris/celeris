package stream

import (
	"context"
	"math"
	"os"
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

// notifySendWindow wakes every handler waiting in AwaitSendWindow, so it
// re-checks its stream's send windows. Called after the peer grants window
// (WINDOW_UPDATE, SETTINGS_INITIAL_WINDOW_SIZE). It costs an atomic load
// unless a handler is waiting. Safe on any goroutine and under any lock:
// sendWindowMu is a leaf.
func (m *Manager) notifySendWindow() {
	if m.sendWindowWaiters.Load() == 0 {
		return
	}
	m.sendWindowMu.Lock()
	if m.sendWindowCh != nil {
		close(m.sendWindowCh)
		m.sendWindowCh = nil
	}
	m.sendWindowMu.Unlock()
}

// sendWindowChan returns the channel the next notifySendWindow closes.
func (m *Manager) sendWindowChan() chan struct{} {
	m.sendWindowMu.Lock()
	if m.sendWindowCh == nil {
		m.sendWindowCh = make(chan struct{})
	}
	ch := m.sendWindowCh
	m.sendWindowMu.Unlock()
	return ch
}

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
	open := func() bool { return m.WindowsOpen(s) }
	if s.IsCancelled() {
		return context.Canceled
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return os.ErrDeadlineExceeded
	}
	if open() {
		return nil
	}
	done := s.Context().Done()
	// Counted before the channel is taken and the windows re-checked, so a
	// notifier that grants window either sees the waiter (and closes the
	// channel it holds) or granted before the re-check (which sees it).
	m.sendWindowWaiters.Add(1)
	defer m.sendWindowWaiters.Add(-1)
	// The deadline is not a select case: a waiter would then re-arm a
	// runtime timer every time a WINDOW_UPDATE wakes it, and every waiter
	// of the connection wakes on each one. The timer instead sets expired
	// and wakes the waiters once, like a WINDOW_UPDATE; the loop below took
	// its channel before it checks expired, so that wakeup is not lost.
	var expired *atomic.Bool
	if !deadline.IsZero() {
		expired = new(atomic.Bool)
		t := time.AfterFunc(time.Until(deadline), func() {
			expired.Store(true)
			m.notifySendWindow()
		})
		defer t.Stop()
	}
	for {
		ch := m.sendWindowChan()
		if s.IsCancelled() {
			return context.Canceled
		}
		if expired != nil && expired.Load() {
			return os.ErrDeadlineExceeded
		}
		if open() {
			return nil
		}
		select {
		case <-ch:
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
// bytes it sends, and reports whether that was the whole buffer and it carried
// END_STREAM: the stream is then fully sent. Whatever the windows refuse stays
// buffered for the next WINDOW_UPDATE. It is called on the event loop for a
// WINDOW_UPDATE or SETTINGS_INITIAL_WINDOW_SIZE, and by a pool handler that has
// just buffered the tail of its response (celeris#903).
//
// The stream's lock is held from the reservation to the send, so two callers
// never send the same bytes, and what is sent is dropped from the front of
// the buffer in place (bytes.Buffer.Next): the work of a flush is in
// proportion to the bytes it sends, not to the bytes that stay buffered
// (celeris#911).
func (m *Manager) FlushOutbound(s *Stream, send OutboundSend) (finished bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := s.OutboundBuffer
	if buf == nil || buf.Len() == 0 {
		return false
	}
	n := int(m.ReserveSendWindow(s, clampWindowRequest(buf.Len())))
	if n <= 0 {
		return false // no room on at least one window: everything stays buffered
	}
	finished = n == buf.Len() && s.OutboundEndStream
	send(s.ID, finished, buf.Next(n))
	m.refundOutbound(n) // celeris#893
	if buf.Len() == 0 {
		buf.Reset()
	}
	return finished
}

// SendOrBufferOutbound sends data on s for a caller that cannot wait for the
// peer's window, an inline handler on the event loop streaming its response
// (celeris#904): what both windows allow goes to send now, and the rest is
// buffered on s, charged to the connection's outbound budget, to follow as the
// peer grants window. While anything is buffered, more data joins the buffer,
// so the bytes of one stream reach the connection in the order they were
// written. Whether to send or buffer is decided under the stream's lock, so a
// flush on the event loop and a detached goroutine's write cannot reorder.
func (m *Manager) SendOrBufferOutbound(s *Stream, data []byte, send OutboundSend) {
	if len(data) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	}
}

// EndOutbound ends s's response body (celeris#904): END_STREAM goes to send
// at once when nothing is buffered, and otherwise rides on the last of the
// buffered DATA, which the flush sends as the peer grants window, so that
// END_STREAM never reaches the connection ahead of DATA written before it.
func (m *Manager) EndOutbound(s *Stream, send OutboundSend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.OutboundBuffer != nil && s.OutboundBuffer.Len() > 0 {
		s.OutboundEndStream = true
		return
	}
	send(s.ID, true, nil)
}
