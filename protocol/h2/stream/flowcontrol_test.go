package stream

import (
	"sync"
	"sync/atomic"
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
func (m *mockFrameWriter) WriteSettingsAck() error                { return nil }
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

// TestReserveSendWindowConcurrent proves the connection-level send window
// cannot be over-subscribed under concurrent reservations — the race the H2
// 64 KB fix closes. The pre-fix path (load window, clamp, then a separate
// atomic add) let two goroutines clamp against the same stale window and both
// debit, driving the shared connection window negative and sending past the
// peer-authorized credit (RFC 7540 §6.9). ReserveSendWindow does the clamp and
// debit in one CAS, so N goroutines reserving from one window partition it
// EXACTLY: the grants sum to the initial credit and the window lands on 0,
// never below. Runs clean under -race.
func TestReserveSendWindowConcurrent(t *testing.T) {
	const (
		initialConn = 100_000
		goroutines  = 16
		chunk       = 137 // odd so grants never divide the window evenly
	)
	m := NewManager()
	atomic.StoreInt32(&m.connectionWindow, initialConn)

	streams := make([]*Stream, goroutines)
	for g := range streams {
		s := m.CreateStream(uint32(2*g + 1))
		s.SetWindowSize(1 << 30) // huge per-stream window → conn window is the only bottleneck
		streams[g] = s
	}

	granted := make([]int64, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(idx int, st *Stream) {
			defer wg.Done()
			for {
				n := m.ReserveSendWindow(st, chunk)
				if n <= 0 {
					return
				}
				granted[idx] += int64(n)
			}
		}(g, streams[g])
	}
	wg.Wait()

	var total int64
	for _, gtd := range granted {
		total += gtd
	}
	if total != initialConn {
		t.Fatalf("total granted = %d, want exactly %d (window over/under-subscribed)", total, initialConn)
	}
	if cw := m.GetConnectionWindow(); cw != 0 {
		t.Fatalf("connectionWindow = %d after exhaustion, want 0 (must never go negative)", cw)
	}
}

// TestReserveSendWindowClampsBothWindows checks the per-stream window binds when
// smaller than the connection window, and that the connection window is debited
// by exactly the granted (clamped) amount — never over-debited when the stream
// window is the bottleneck.
func TestReserveSendWindowClampsBothWindows(t *testing.T) {
	m := NewManager()
	atomic.StoreInt32(&m.connectionWindow, 10_000)
	s := m.CreateStream(1)
	s.SetWindowSize(300) // stream window is the bottleneck

	if got := m.ReserveSendWindow(s, 1000); got != 300 {
		t.Fatalf("grant = %d, want 300 (clamped by the per-stream window)", got)
	}
	if sw := s.GetWindowSize(); sw != 0 {
		t.Fatalf("stream window = %d, want 0 (debited by grant)", sw)
	}
	if cw := m.GetConnectionWindow(); cw != 10_000-300 {
		t.Fatalf("connection window = %d, want %d (debited by grant, not the request)", cw, 10_000-300)
	}
	// Stream window exhausted → further reservations grant 0 and must not touch
	// the connection window.
	if got := m.ReserveSendWindow(s, 1000); got != 0 {
		t.Fatalf("grant after stream exhaustion = %d, want 0", got)
	}
	if cw := m.GetConnectionWindow(); cw != 10_000-300 {
		t.Fatalf("connection window leaked: %d, want %d", cw, 10_000-300)
	}
}
