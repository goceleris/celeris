package frame

import (
	"bytes"
	"sync"
	"testing"

	"golang.org/x/net/http2"
)

func TestWriteSettings(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteSettings(http2.Setting{
		ID:  http2.SettingMaxConcurrentStreams,
		Val: 100,
	}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("Expected non-empty buffer after WriteSettings")
	}

	// Parse the written frame
	p := NewParser()
	p.InitReader(&buf)
	f, err := p.ReadNextFrame()
	if err != nil {
		t.Fatalf("ReadNextFrame: %v", err)
	}
	sf, ok := f.(*http2.SettingsFrame)
	if !ok {
		t.Fatalf("Expected *http2.SettingsFrame, got %T", f)
	}
	if sf.IsAck() {
		t.Error("Expected non-ACK SETTINGS")
	}
}

func TestWriteSettingsAck(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteSettingsAck(); err != nil {
		t.Fatalf("WriteSettingsAck: %v", err)
	}

	p := NewParser()
	p.InitReader(&buf)
	f, err := p.ReadNextFrame()
	if err != nil {
		t.Fatalf("ReadNextFrame: %v", err)
	}
	sf, ok := f.(*http2.SettingsFrame)
	if !ok {
		t.Fatalf("Expected *http2.SettingsFrame, got %T", f)
	}
	if !sf.IsAck() {
		t.Error("Expected ACK SETTINGS")
	}
}

func TestWriteData(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteData(1, true, []byte("hello")); err != nil {
		t.Fatalf("WriteData: %v", err)
	}

	p := NewParser()
	p.InitReader(&buf)
	f, err := p.ReadNextFrame()
	if err != nil {
		t.Fatalf("ReadNextFrame: %v", err)
	}
	df, ok := f.(*http2.DataFrame)
	if !ok {
		t.Fatalf("Expected *http2.DataFrame, got %T", f)
	}
	if string(df.Data()) != "hello" {
		t.Errorf("Data: got %q, want %q", df.Data(), "hello")
	}
	if !df.StreamEnded() {
		t.Error("Expected END_STREAM flag")
	}
}

func TestWriteDataEmptySuppression(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	// Empty data without END_STREAM should be suppressed
	if err := w.WriteData(1, false, nil); err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	if buf.Len() != 0 {
		t.Error("Empty DATA without END_STREAM should be suppressed")
	}

	// Empty data with END_STREAM should be written
	if err := w.WriteData(1, true, nil); err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("Empty DATA with END_STREAM should be written")
	}
}

func TestWriteHeaders(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	headerBlock := []byte{0x82, 0x86} // :method GET, :path /
	if err := w.WriteHeaders(1, true, headerBlock, 0); err != nil {
		t.Fatalf("WriteHeaders: %v", err)
	}

	p := NewParser()
	p.InitReader(&buf)
	f, err := p.ReadNextFrame()
	if err != nil {
		t.Fatalf("ReadNextFrame: %v", err)
	}
	hf, ok := f.(*http2.HeadersFrame)
	if !ok {
		t.Fatalf("Expected *http2.HeadersFrame, got %T", f)
	}
	if !hf.HeadersEnded() {
		t.Error("Expected END_HEADERS")
	}
	if !hf.StreamEnded() {
		t.Error("Expected END_STREAM")
	}
}

func TestWriteHeadersContinuationFragmentation(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	// Create a header block larger than maxFrameSize
	maxFrameSize := uint32(10)
	headerBlock := make([]byte, 25)
	for i := range headerBlock {
		headerBlock[i] = byte(i)
	}

	if err := w.WriteHeaders(1, false, headerBlock, maxFrameSize); err != nil {
		t.Fatalf("WriteHeaders: %v", err)
	}

	p := NewParser()
	p.InitReader(&buf)

	// First frame: HEADERS without END_HEADERS
	f1, err := p.ReadNextFrame()
	if err != nil {
		t.Fatalf("ReadNextFrame (1): %v", err)
	}
	hf, ok := f1.(*http2.HeadersFrame)
	if !ok {
		t.Fatalf("Expected HEADERS frame, got %T", f1)
	}
	if hf.HeadersEnded() {
		t.Error("First frame should not have END_HEADERS")
	}
	if hf.StreamEnded() {
		t.Error("First frame should not have END_STREAM (endStream=false)")
	}

	// Second frame: CONTINUATION without END_HEADERS
	f2, err := p.ReadNextFrame()
	if err != nil {
		t.Fatalf("ReadNextFrame (2): %v", err)
	}
	_, ok = f2.(*http2.ContinuationFrame)
	if !ok {
		t.Fatalf("Expected CONTINUATION frame, got %T", f2)
	}

	// Third frame: CONTINUATION with END_HEADERS
	f3, err := p.ReadNextFrame()
	if err != nil {
		t.Fatalf("ReadNextFrame (3): %v", err)
	}
	cf, ok := f3.(*http2.ContinuationFrame)
	if !ok {
		t.Fatalf("Expected CONTINUATION frame, got %T", f3)
	}
	if !cf.HeadersEnded() {
		t.Error("Last CONTINUATION should have END_HEADERS")
	}
}

func TestWriteWindowUpdate(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteWindowUpdate(1, 1024); err != nil {
		t.Fatalf("WriteWindowUpdate: %v", err)
	}

	p := NewParser()
	p.InitReader(&buf)
	f, err := p.ReadNextFrame()
	if err != nil {
		t.Fatalf("ReadNextFrame: %v", err)
	}
	wf, ok := f.(*http2.WindowUpdateFrame)
	if !ok {
		t.Fatalf("Expected *http2.WindowUpdateFrame, got %T", f)
	}
	if wf.Increment != 1024 {
		t.Errorf("Increment: got %d, want 1024", wf.Increment)
	}
}

func TestWriteRSTStream(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteRSTStream(1, http2.ErrCodeCancel); err != nil {
		t.Fatalf("WriteRSTStream: %v", err)
	}

	p := NewParser()
	p.InitReader(&buf)
	f, err := p.ReadNextFrame()
	if err != nil {
		t.Fatalf("ReadNextFrame: %v", err)
	}
	rf, ok := f.(*http2.RSTStreamFrame)
	if !ok {
		t.Fatalf("Expected *http2.RSTStreamFrame, got %T", f)
	}
	if rf.ErrCode != http2.ErrCodeCancel {
		t.Errorf("ErrCode: got %v, want %v", rf.ErrCode, http2.ErrCodeCancel)
	}
}

func TestWriteGoAway(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteGoAway(1, http2.ErrCodeNo, []byte("bye")); err != nil {
		t.Fatalf("WriteGoAway: %v", err)
	}

	p := NewParser()
	p.InitReader(&buf)
	f, err := p.ReadNextFrame()
	if err != nil {
		t.Fatalf("ReadNextFrame: %v", err)
	}
	gf, ok := f.(*http2.GoAwayFrame)
	if !ok {
		t.Fatalf("Expected *http2.GoAwayFrame, got %T", f)
	}
	if gf.LastStreamID != 1 {
		t.Errorf("LastStreamID: got %d, want 1", gf.LastStreamID)
	}
	if gf.ErrCode != http2.ErrCodeNo {
		t.Errorf("ErrCode: got %v, want %v", gf.ErrCode, http2.ErrCodeNo)
	}
}

func TestWritePing(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	data := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	if err := w.WritePing(false, data); err != nil {
		t.Fatalf("WritePing: %v", err)
	}

	p := NewParser()
	p.InitReader(&buf)
	f, err := p.ReadNextFrame()
	if err != nil {
		t.Fatalf("ReadNextFrame: %v", err)
	}
	pf, ok := f.(*http2.PingFrame)
	if !ok {
		t.Fatalf("Expected *http2.PingFrame, got %T", f)
	}
	if pf.IsAck() {
		t.Error("Expected non-ACK PING")
	}
	if pf.Data != data {
		t.Errorf("Data: got %v, want %v", pf.Data, data)
	}
}

func TestWritePingAck(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	data := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	if err := w.WritePing(true, data); err != nil {
		t.Fatalf("WritePing: %v", err)
	}

	p := NewParser()
	p.InitReader(&buf)
	f, err := p.ReadNextFrame()
	if err != nil {
		t.Fatalf("ReadNextFrame: %v", err)
	}
	pf, ok := f.(*http2.PingFrame)
	if !ok {
		t.Fatalf("Expected *http2.PingFrame, got %T", f)
	}
	if !pf.IsAck() {
		t.Error("Expected ACK PING")
	}
}

func TestWritePushPromise(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WritePushPromise(1, 2, true, []byte{0x82}); err != nil {
		t.Fatalf("WritePushPromise: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("Expected non-empty buffer")
	}
}

func TestConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(streamID uint32) {
			defer wg.Done()
			_ = w.WriteData(streamID, false, []byte("data"))
		}(uint32(i*2 + 1))
	}
	wg.Wait()

	if buf.Len() == 0 {
		t.Error("Expected non-empty buffer after concurrent writes")
	}
}

func TestFlushNoOp(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

func TestWriteFrame(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	f := &Frame{
		Type:     FrameData,
		Flags:    FlagEndStream,
		StreamID: 1,
		Payload:  []byte("test"),
	}
	if err := w.WriteFrame(f); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	p := NewParser()
	p.InitReader(&buf)
	rf, err := p.ReadNextFrame()
	if err != nil {
		t.Fatalf("ReadNextFrame: %v", err)
	}
	df, ok := rf.(*http2.DataFrame)
	if !ok {
		t.Fatalf("Expected *http2.DataFrame, got %T", rf)
	}
	if string(df.Data()) != "test" {
		t.Errorf("Data: got %q, want %q", df.Data(), "test")
	}
}
