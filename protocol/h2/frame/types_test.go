package frame

import "testing"

func TestFrameTypeConstants(t *testing.T) {
	tests := []struct {
		ft   Type
		val  uint8
		name string
	}{
		{FrameData, 0x0, "DATA"},
		{FrameHeaders, 0x1, "HEADERS"},
		{FramePriority, 0x2, "PRIORITY"},
		{FrameRSTStream, 0x3, "RST_STREAM"},
		{FrameSettings, 0x4, "SETTINGS"},
		{FramePushPromise, 0x5, "PUSH_PROMISE"},
		{FramePing, 0x6, "PING"},
		{FrameGoAway, 0x7, "GOAWAY"},
		{FrameWindowUpdate, 0x8, "WINDOW_UPDATE"},
		{FrameContinuation, 0x9, "CONTINUATION"},
	}

	for _, tt := range tests {
		if uint8(tt.ft) != tt.val {
			t.Errorf("Frame type %s: got 0x%x, want 0x%x", tt.name, uint8(tt.ft), tt.val)
		}
		if tt.ft.String() != tt.name {
			t.Errorf("Frame type String(): got %q, want %q", tt.ft.String(), tt.name)
		}
	}
}

func TestFrameTypeStringUnknown(t *testing.T) {
	unknown := Type(0xFF)
	if unknown.String() != "UNKNOWN" {
		t.Errorf("Unknown frame type String(): got %q, want %q", unknown.String(), "UNKNOWN")
	}
}

func TestFlagConstants(t *testing.T) {
	if FlagAck != 0x1 {
		t.Errorf("FlagAck: got 0x%x, want 0x1", FlagAck)
	}
	if FlagEndStream != 0x1 {
		t.Errorf("FlagEndStream: got 0x%x, want 0x1", FlagEndStream)
	}
	if FlagEndHeaders != 0x4 {
		t.Errorf("FlagEndHeaders: got 0x%x, want 0x4", FlagEndHeaders)
	}
	if FlagPadded != 0x8 {
		t.Errorf("FlagPadded: got 0x%x, want 0x8", FlagPadded)
	}
	if FlagPriority != 0x20 {
		t.Errorf("FlagPriority: got 0x%x, want 0x20", FlagPriority)
	}
}

func TestFrameStruct(t *testing.T) {
	f := Frame{
		Type:     FrameHeaders,
		Flags:    FlagEndStream | FlagEndHeaders,
		StreamID: 1,
		Payload:  []byte("test"),
	}

	if f.Type != FrameHeaders {
		t.Errorf("Frame.Type: got %v, want %v", f.Type, FrameHeaders)
	}
	if f.Flags != FlagEndStream|FlagEndHeaders {
		t.Errorf("Frame.Flags: got 0x%x, want 0x%x", f.Flags, FlagEndStream|FlagEndHeaders)
	}
	if f.StreamID != 1 {
		t.Errorf("Frame.StreamID: got %d, want 1", f.StreamID)
	}
	if string(f.Payload) != "test" {
		t.Errorf("Frame.Payload: got %q, want %q", f.Payload, "test")
	}
}
