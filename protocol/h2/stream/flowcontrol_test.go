package stream

import (
	"testing"

	"golang.org/x/net/http2"
)

type mockFrameWriter struct {
	windowUpdates map[uint32]uint32
}

func newMockFrameWriter() *mockFrameWriter {
	return &mockFrameWriter{
		windowUpdates: make(map[uint32]uint32),
	}
}

func (m *mockFrameWriter) WriteSettings(_ ...http2.Setting) error { return nil }
func (m *mockFrameWriter) WriteSettingsAck() error                 { return nil }
func (m *mockFrameWriter) WriteHeaders(_ uint32, _ bool, _ []byte, _ uint32) error {
	return nil
}
func (m *mockFrameWriter) WriteData(_ uint32, _ bool, _ []byte) error { return nil }
func (m *mockFrameWriter) WriteWindowUpdate(streamID uint32, increment uint32) error {
	m.windowUpdates[streamID] += increment
	return nil
}
func (m *mockFrameWriter) WriteRSTStream(_ uint32, _ http2.ErrCode) error { return nil }
func (m *mockFrameWriter) WriteGoAway(_ uint32, _ http2.ErrCode, _ []byte) error {
	return nil
}
func (m *mockFrameWriter) WritePing(_ bool, _ [8]byte) error { return nil }
func (m *mockFrameWriter) WritePushPromise(_ uint32, _ uint32, _ bool, _ []byte) error {
	return nil
}

func TestConsumeSendWindow(t *testing.T) {
	m := NewManager()
	s := m.CreateStream(1)
	s.SetState(StateOpen)

	initialConn := m.GetConnectionWindow()
	initialStream := s.GetWindowSize()

	m.ConsumeSendWindow(1, 1000)

	if m.GetConnectionWindow() != initialConn-1000 {
		t.Errorf("Connection window: got %d, want %d", m.GetConnectionWindow(), initialConn-1000)
	}
	if s.GetWindowSize() != initialStream-1000 {
		t.Errorf("Stream window: got %d, want %d", s.GetWindowSize(), initialStream-1000)
	}
}

func TestConsumeSendWindowZero(t *testing.T) {
	m := NewManager()
	initial := m.GetConnectionWindow()
	m.ConsumeSendWindow(1, 0)
	if m.GetConnectionWindow() != initial {
		t.Error("Zero consume should not change window")
	}
}

func TestConsumeSendWindowFast(t *testing.T) {
	m := NewManager()
	s := m.CreateStream(1)

	initialConn := m.GetConnectionWindow()
	initialStream := s.GetWindowSize()

	m.ConsumeSendWindowFast(s, 500)

	if m.GetConnectionWindow() != initialConn-500 {
		t.Errorf("Connection window: got %d, want %d", m.GetConnectionWindow(), initialConn-500)
	}
	if s.GetWindowSize() != initialStream-500 {
		t.Errorf("Stream window: got %d, want %d", s.GetWindowSize(), initialStream-500)
	}
}

func TestAccumulateWindowUpdate(t *testing.T) {
	m := NewManager()
	fw := newMockFrameWriter()

	// Accumulate below threshold
	m.AccumulateWindowUpdate(0, 1000)
	m.AccumulateWindowUpdate(1, 500)

	flushed := m.FlushWindowUpdates(fw, false)
	if flushed {
		t.Error("Should not flush below threshold")
	}

	// Accumulate above threshold for connection
	m.AccumulateWindowUpdate(0, windowUpdateThreshold)
	flushed = m.FlushWindowUpdates(fw, false)
	if !flushed {
		t.Error("Should flush above threshold")
	}
	if fw.windowUpdates[0] == 0 {
		t.Error("Connection window update should have been sent")
	}
}

func TestFlushWindowUpdatesForce(t *testing.T) {
	m := NewManager()
	fw := newMockFrameWriter()

	// Accumulate below threshold
	m.AccumulateWindowUpdate(0, 100)
	m.AccumulateWindowUpdate(1, 50)

	// Force flush
	flushed := m.FlushWindowUpdates(fw, true)
	if !flushed {
		t.Error("Force flush should always send")
	}
	if fw.windowUpdates[0] != 100 {
		t.Errorf("Connection window update: got %d, want 100", fw.windowUpdates[0])
	}
	if fw.windowUpdates[1] != 50 {
		t.Errorf("Stream window update: got %d, want 50", fw.windowUpdates[1])
	}
}

func TestFlushWindowUpdatesResets(t *testing.T) {
	m := NewManager()
	fw := newMockFrameWriter()

	m.AccumulateWindowUpdate(0, 100)
	m.FlushWindowUpdates(fw, true)

	// After flush, accumulator should be reset
	fw2 := newMockFrameWriter()
	flushed := m.FlushWindowUpdates(fw2, true)
	// Should not flush anything since accumulators were reset
	if flushed {
		t.Error("Should not flush after accumulators were reset")
	}
}

func TestBatchThreshold(t *testing.T) {
	m := NewManager()
	fw := newMockFrameWriter()

	// Exactly at threshold
	m.AccumulateWindowUpdate(1, windowUpdateThreshold)
	flushed := m.FlushWindowUpdates(fw, false)
	if !flushed {
		t.Error("Should flush at exact threshold")
	}
	if fw.windowUpdates[1] != windowUpdateThreshold {
		t.Errorf("Stream update: got %d, want %d", fw.windowUpdates[1], windowUpdateThreshold)
	}
}
