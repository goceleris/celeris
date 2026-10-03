package stream

import "sync/atomic"

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
//     a handler's Write on flow control. It holds nothing extra while it
//     waits: its body is its own.
//
// A response is never refused for its size: with nothing held, any one body
// is buffered whole. So a connection holds at most about the budget plus one
// body inline plus one on the pool. 4 MiB matches the HTTP/1 per-connection
// backlog limit (the engines' maxPendingBytes).
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
func (s *Stream) TryBufferOutbound(data []byte, endStream bool) bool {
	m := s.manager
	if m == nil || s.flags.Load()&flagAsyncRunning == 0 {
		s.BufferOutbound(data, endStream)
		return true
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
// are open, so some of its DATA can be sent (celeris#893). It reports false
// when s is cancelled: the peer reset it, or its connection closed
// (Manager.Close cancels every stream), and nothing more is to be sent.
//
// It holds no lock while it waits. The peer decides how long that is, as it
// does for a net/http handler blocked in Write; the connection's idle and
// write timeouts bound it.
func (s *Stream) AwaitSendWindow() bool {
	m := s.manager
	if m == nil {
		return true
	}
	open := func() bool {
		return s.windowSize.Load() > 0 && atomic.LoadInt32(&m.connectionWindow) > 0
	}
	if s.IsCancelled() {
		return false
	}
	if open() {
		return true
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
			return false
		}
		if open() {
			return true
		}
		select {
		case <-ch:
		case <-done:
			return false
		}
	}
}
