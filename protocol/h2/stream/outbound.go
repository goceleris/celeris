package stream

import (
	"context"
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
// every connection on that loop, until the deadline (with WriteTimeout
// disabled, until the peer grants window).
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
// budget, and must not buffer any of it later: the event loop sends a
// stream's buffered DATA straight to the connection, ahead of what the write
// queue still holds, so one body sent both ways could arrive out of order.
//
// Only a handler on its own goroutine can wait. For a stream that is not on
// the worker pool (an inline handler, on the event loop; a stream with no
// manager) data is buffered as before and the result is true: the event loop
// keeps inline handlers off a connection that is over its budget instead.
//
// A stream that would have run inline but was put on the pool by the budget
// (flagBudgetPool) is never buffered, whatever the budget holds by now: the
// HEADERS of a pool handler wait in the write queue, and the event loop
// flushes a stream's buffered DATA straight to the connection, so its DATA
// could reach the peer before its HEADERS (#903's mechanism, which an inline
// stream does not reach). It sends everything through the queue instead.
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
// event loop to fire: a timer ends the wait. On the native engines the loop
// may be the thing waiting (a sync handler blocked on a lock this handler
// holds, or on a coalesced call it leads), and then it cannot process the
// WINDOW_UPDATE, the reset or the close that would end the wait. The
// connection's read and idle timeouts do not end it either (see
// OutboundBudget).
func (s *Stream) AwaitSendWindow(deadline time.Time) error {
	m := s.manager
	if m == nil {
		return nil
	}
	open := func() bool {
		return s.windowSize.Load() > 0 && atomic.LoadInt32(&m.connectionWindow) > 0
	}
	if s.IsCancelled() {
		return context.Canceled
	}
	var expired <-chan time.Time
	if !deadline.IsZero() {
		d := time.Until(deadline)
		if d <= 0 {
			return os.ErrDeadlineExceeded
		}
		if open() {
			return nil
		}
		t := time.NewTimer(d)
		defer t.Stop()
		expired = t.C
	} else if open() {
		return nil
	}
	done := s.Context().Done()
	// Counted before the channel is taken and the windows re-checked, so a
	// notifier that grants window either sees the waiter (and closes the
	// channel it holds) or granted before the re-check (which sees it).
	m.sendWindowWaiters.Add(1)
	defer m.sendWindowWaiters.Add(-1)
	for {
		ch := m.sendWindowChan()
		if s.IsCancelled() {
			return context.Canceled
		}
		if open() {
			return nil
		}
		select {
		case <-ch:
		case <-done:
			return context.Canceled
		case <-expired:
			return os.ErrDeadlineExceeded
		}
	}
}
