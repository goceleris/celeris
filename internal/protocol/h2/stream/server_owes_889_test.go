package stream

import "testing"

// ServerOwes (celeris#889): a stream is open and every open stream has a whole
// request. The counts it reads move with the stream's state in
// updateActiveCount, whatever closes the stream.

func owes889(t *testing.T, m *Manager, want bool, when string) {
	t.Helper()
	if got := m.ServerOwes(); got != want {
		t.Fatalf("ServerOwes %s = %v, want %v (active %d, request open %d)", when, got, want, m.activeStreams.Load(), m.requestOpen.Load())
	}
}

func TestServerOwesFollowsTheRequest889(t *testing.T) {
	m := NewManager()
	owes889(t, m, false, "with no stream")
	s1, ok := m.TryOpenStream(1)
	if !ok {
		t.Fatal("no stream slot")
	}
	owes889(t, m, false, "with a request open")
	s1.SetState(StateHalfClosedRemote)
	owes889(t, m, true, "with the request whole")
	s3, _ := m.TryOpenStream(3)
	owes889(t, m, false, "with a second request open")
	s3.SetState(StateHalfClosedRemote)
	owes889(t, m, true, "with both requests whole")
	s1.SetState(StateClosed)
	owes889(t, m, true, "with one stream closed")
	s3.SetState(StateClosed)
	owes889(t, m, false, "with every stream closed")
	if m.activeStreams.Load() != 0 || m.requestOpen.Load() != 0 {
		t.Fatalf("counts after every stream closed: active %d, request open %d", m.activeStreams.Load(), m.requestOpen.Load())
	}
}

// A server that answered first (HalfClosedLocal) still waits for the client's
// request: not exempt.
func TestServerOwesHalfClosedLocalStillOwesTheClient889(t *testing.T) {
	m := NewManager()
	s, _ := m.TryOpenStream(1)
	s.SetState(StateHalfClosedLocal)
	owes889(t, m, false, "after the server's END_STREAM with the request open")
	s.SetState(StateClosed)
	owes889(t, m, false, "closed")
	if m.requestOpen.Load() != 0 {
		t.Fatalf("request open = %d after close", m.requestOpen.Load())
	}
}

// Every path that ends a stream lowers both counts once: close by state, delete
// of an open stream, reset by the peer, and a duplicate Closed transition.
func TestServerOwesCountsDoNotLeakOrUnderflow889(t *testing.T) {
	m := NewManager()
	check := func(when string) {
		t.Helper()
		if a, r := m.activeStreams.Load(), m.requestOpen.Load(); a != 0 || r != 0 {
			t.Fatalf("%s: active %d, request open %d, want 0 and 0", when, a, r)
		}
	}
	s1, _ := m.TryOpenStream(1)
	m.DeleteStream(1) // an open request deleted
	check("delete of a request-open stream")
	s3, _ := m.TryOpenStream(3)
	s3.SetState(StateHalfClosedRemote)
	m.DeleteStream(3)
	check("delete of a request-whole stream")
	m.TryOpenStream(5)
	if !m.resetStream(5) {
		t.Fatal("reset of stream 5 found no stream")
	}
	check("reset by the peer")
	s7, _ := m.TryOpenStream(7)
	s7.SetState(StateClosed)
	s7.SetState(StateClosed)
	m.DeleteStream(7)
	check("duplicate Closed transitions")
	_ = s1
}

// A response waiting for the client's window means the client owes the server
// (the WINDOW_UPDATE): not exempt, whatever the request. The bytes are charged
// where the buffer fills and refunded when it is dropped with the stream or
// sent, so a stream that ends with its tail unsent does not exempt the conn.
func TestServerOwesBufferedResponseIsOwedByTheClient889(t *testing.T) {
	m := NewManager()
	s, _ := m.TryOpenStream(1)
	s.SetState(StateHalfClosedRemote)
	owes889(t, m, true, "request whole, nothing buffered")
	s.BufferOutbound(make([]byte, 1<<20), true)
	owes889(t, m, false, "with 1 MiB buffered for the client's window")
	if m.OutboundHeld() != 1<<20 {
		t.Fatalf("OutboundHeld = %d, want 1 MiB", m.OutboundHeld())
	}
	m.DeleteStream(1)
	if m.OutboundHeld() != 0 {
		t.Fatalf("OutboundHeld = %d after the stream was deleted with its tail unsent, want 0 (a leak would exempt the conn for good)", m.OutboundHeld())
	}
	owes889(t, m, false, "with no stream")
	s3, _ := m.TryOpenStream(3)
	s3.SetState(StateHalfClosedRemote)
	owes889(t, m, true, "a new stream whose request is whole")
}

// A pool handler waiting for the client's window (the budget is spent, so it
// waits instead of buffering) is the same debt.
func TestServerOwesAWaitingHandlerIsOwedByTheClient889(t *testing.T) {
	m := NewManager()
	s, _ := m.TryOpenStream(1)
	s.SetState(StateHalfClosedRemote)
	m.sendWindowWaiters.Add(1)
	owes889(t, m, false, "with a handler waiting for window")
	m.sendWindowWaiters.Add(-1)
	owes889(t, m, true, "with the wait over")
}
