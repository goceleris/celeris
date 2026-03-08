package stream

import "testing"

func TestNewManager(t *testing.T) {
	m := NewManager()
	if m == nil {
		t.Fatal("NewManager returned nil")
	}
	if m.GetConnectionWindow() != 65535 {
		t.Errorf("Initial connection window: got %d, want 65535", m.GetConnectionWindow())
	}
	if m.GetMaxConcurrentStreams() != 100 {
		t.Errorf("Initial max streams: got %d, want 100", m.GetMaxConcurrentStreams())
	}
	if m.StreamCount() != 0 {
		t.Errorf("Initial stream count: got %d, want 0", m.StreamCount())
	}
}

func TestCreateStream(t *testing.T) {
	m := NewManager()
	s := m.CreateStream(1)
	if s == nil {
		t.Fatal("CreateStream returned nil")
	}
	if s.ID != 1 {
		t.Errorf("Stream ID: got %d, want 1", s.ID)
	}
	if s.GetState() != StateIdle {
		t.Errorf("Stream state: got %v, want Idle", s.GetState())
	}
	if m.StreamCount() != 1 {
		t.Errorf("Stream count: got %d, want 1", m.StreamCount())
	}
}

func TestGetStream(t *testing.T) {
	m := NewManager()
	m.CreateStream(1)

	s, ok := m.GetStream(1)
	if !ok {
		t.Fatal("GetStream returned false for existing stream")
	}
	if s.ID != 1 {
		t.Errorf("Stream ID: got %d, want 1", s.ID)
	}

	_, ok = m.GetStream(99)
	if ok {
		t.Error("GetStream returned true for non-existing stream")
	}
}

func TestDeleteStream(t *testing.T) {
	m := NewManager()
	m.CreateStream(1)
	m.DeleteStream(1)

	_, ok := m.GetStream(1)
	if ok {
		t.Error("GetStream returned true after DeleteStream")
	}
	if m.StreamCount() != 0 {
		t.Errorf("Stream count after delete: got %d, want 0", m.StreamCount())
	}
}

func TestTryOpenStream(t *testing.T) {
	m := NewManager()
	s, ok := m.TryOpenStream(1)
	if !ok {
		t.Fatal("TryOpenStream failed")
	}
	if s.GetState() != StateOpen {
		t.Errorf("Stream state: got %v, want Open", s.GetState())
	}
	if m.CountActiveStreams() != 1 {
		t.Errorf("Active streams: got %d, want 1", m.CountActiveStreams())
	}
}

func TestTryOpenStreamConcurrencyLimit(t *testing.T) {
	m := NewManager()
	m.SetMaxConcurrentStreams(2)

	_, ok := m.TryOpenStream(1)
	if !ok {
		t.Fatal("TryOpenStream(1) failed")
	}
	_, ok = m.TryOpenStream(3)
	if !ok {
		t.Fatal("TryOpenStream(3) failed")
	}

	// Third stream should be refused
	_, ok = m.TryOpenStream(5)
	if ok {
		t.Error("TryOpenStream(5) should have been refused (max 2 concurrent)")
	}

	// Close one stream and try again
	s, _ := m.GetStream(1)
	s.SetState(StateClosed)

	_, ok = m.TryOpenStream(5)
	if !ok {
		t.Error("TryOpenStream(5) should succeed after closing a stream")
	}
}

func TestTryOpenStreamExisting(t *testing.T) {
	m := NewManager()
	s1, ok := m.TryOpenStream(1)
	if !ok {
		t.Fatal("TryOpenStream(1) failed")
	}

	// Opening same stream again should return existing
	s2, ok := m.TryOpenStream(1)
	if !ok {
		t.Fatal("TryOpenStream(1) second call failed")
	}
	if s1 != s2 {
		t.Error("Expected same stream object for duplicate TryOpenStream")
	}
}

func TestGetLastStreamID(t *testing.T) {
	m := NewManager()
	m.CreateStream(1)
	m.CreateStream(3)
	m.CreateStream(5)

	if m.GetLastStreamID() != 5 {
		t.Errorf("GetLastStreamID: got %d, want 5", m.GetLastStreamID())
	}
}

func TestGetLastClientStreamID(t *testing.T) {
	m := NewManager()
	if m.GetLastClientStreamID() != 0 {
		t.Errorf("Initial GetLastClientStreamID: got %d, want 0", m.GetLastClientStreamID())
	}

	m.mu.Lock()
	m.lastClientStream = 5
	m.mu.Unlock()

	if m.GetLastClientStreamID() != 5 {
		t.Errorf("GetLastClientStreamID: got %d, want 5", m.GetLastClientStreamID())
	}
}

func TestUpdateConnectionWindow(t *testing.T) {
	m := NewManager()
	m.UpdateConnectionWindow(-1000)
	if m.GetConnectionWindow() != 65535-1000 {
		t.Errorf("Connection window: got %d, want %d", m.GetConnectionWindow(), 65535-1000)
	}
	m.UpdateConnectionWindow(500)
	if m.GetConnectionWindow() != 65535-500 {
		t.Errorf("Connection window: got %d, want %d", m.GetConnectionWindow(), 65535-500)
	}
}

func TestSetMaxConcurrentStreams(t *testing.T) {
	m := NewManager()
	m.SetMaxConcurrentStreams(50)
	if m.GetMaxConcurrentStreams() != 50 {
		t.Errorf("MaxConcurrentStreams: got %d, want 50", m.GetMaxConcurrentStreams())
	}
}

func TestCountActiveStreams(t *testing.T) {
	m := NewManager()
	s := m.CreateStream(1)
	s.SetState(StateOpen)
	if m.CountActiveStreams() != 1 {
		t.Errorf("Active streams: got %d, want 1", m.CountActiveStreams())
	}

	s.SetState(StateHalfClosedRemote)
	if m.CountActiveStreams() != 1 {
		t.Errorf("Active streams after half-close: got %d, want 1", m.CountActiveStreams())
	}

	s.SetState(StateClosed)
	if m.CountActiveStreams() != 0 {
		t.Errorf("Active streams after close: got %d, want 0", m.CountActiveStreams())
	}
}

func TestGetOrCreateStream(t *testing.T) {
	m := NewManager()
	s1 := m.GetOrCreateStream(1)
	if s1 == nil {
		t.Fatal("GetOrCreateStream returned nil")
	}

	s2 := m.GetOrCreateStream(1)
	if s1 != s2 {
		t.Error("GetOrCreateStream should return existing stream")
	}
}

func TestMarkStreamBufferedEmpty(t *testing.T) {
	m := NewManager()
	m.MarkStreamBuffered(1)

	m.mu.RLock()
	_, exists := m.streamsWithData[1]
	m.mu.RUnlock()
	if !exists {
		t.Error("Stream should be marked as buffered")
	}

	m.MarkStreamEmpty(1)
	m.mu.RLock()
	_, exists = m.streamsWithData[1]
	m.mu.RUnlock()
	if exists {
		t.Error("Stream should not be marked as buffered after MarkStreamEmpty")
	}
}
