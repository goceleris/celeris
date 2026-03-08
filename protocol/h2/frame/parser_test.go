package frame

import (
	"bytes"
	"testing"

	"golang.org/x/net/http2"
)

func TestNewParser(t *testing.T) {
	p := NewParser()
	if p == nil {
		t.Fatal("NewParser returned nil")
	}
	if p.buf == nil {
		t.Fatal("Parser buffer is nil")
	}
}

func TestParserInitReader(t *testing.T) {
	p := NewParser()
	r := new(bytes.Buffer)
	p.InitReader(r)
	if p.framer == nil {
		t.Fatal("Parser framer is nil after InitReader")
	}
}

func TestReadNextFrameNotInitialized(t *testing.T) {
	p := NewParser()
	_, err := p.ReadNextFrame()
	if err == nil {
		t.Fatal("Expected error from ReadNextFrame without InitReader")
	}
}

func TestReadNextFrameSettings(t *testing.T) {
	var buf bytes.Buffer
	framer := http2.NewFramer(&buf, nil)
	if err := framer.WriteSettings(http2.Setting{
		ID:  http2.SettingMaxConcurrentStreams,
		Val: 100,
	}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
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
	if sf.IsAck() {
		t.Error("Expected non-ACK SETTINGS frame")
	}
}

func TestReadNextFrameHeaders(t *testing.T) {
	var buf bytes.Buffer
	framer := http2.NewFramer(&buf, nil)
	headerBlock := []byte{0x82} // :method GET (static table index 2)
	if err := framer.WriteRawFrame(http2.FrameHeaders,
		http2.FlagHeadersEndHeaders|http2.FlagHeadersEndStream,
		1, headerBlock); err != nil {
		t.Fatalf("WriteRawFrame: %v", err)
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
	if hf.StreamID != 1 {
		t.Errorf("StreamID: got %d, want 1", hf.StreamID)
	}
	if !hf.HeadersEnded() {
		t.Error("Expected END_HEADERS flag")
	}
	if !hf.StreamEnded() {
		t.Error("Expected END_STREAM flag")
	}
}

func TestReadNextFrameData(t *testing.T) {
	var buf bytes.Buffer
	framer := http2.NewFramer(&buf, nil)
	if err := framer.WriteData(1, true, []byte("hello")); err != nil {
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

func TestParseRaw(t *testing.T) {
	// Build a raw SETTINGS frame: 3-byte length + type + flags + 4-byte stream ID + payload
	var raw bytes.Buffer
	// Length: 6 (one setting = 6 bytes)
	raw.Write([]byte{0, 0, 6})
	// Type: SETTINGS (0x4)
	raw.WriteByte(0x04)
	// Flags: 0
	raw.WriteByte(0x00)
	// Stream ID: 0
	raw.Write([]byte{0, 0, 0, 0})
	// Setting: MAX_CONCURRENT_STREAMS (0x3) = 100
	raw.Write([]byte{0, 3, 0, 0, 0, 100})

	p := NewParser()
	f, err := p.Parse(&raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Type != FrameSettings {
		t.Errorf("Type: got %v, want %v", f.Type, FrameSettings)
	}
	if f.StreamID != 0 {
		t.Errorf("StreamID: got %d, want 0", f.StreamID)
	}
	if len(f.Payload) != 6 {
		t.Errorf("Payload length: got %d, want 6", len(f.Payload))
	}
}
