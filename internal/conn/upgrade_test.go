package conn

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

func TestDecodeHTTP2Settings(t *testing.T) {
	// Build a representative settings payload: two entries (id+value pairs, 6 bytes each).
	payload := []byte{
		0x00, 0x03, 0x00, 0x00, 0x00, 0x64, // SETTINGS_MAX_CONCURRENT_STREAMS = 100
		0x00, 0x04, 0x00, 0x01, 0x00, 0x00, // SETTINGS_INITIAL_WINDOW_SIZE = 65536
	}
	rawURL := base64.RawURLEncoding.EncodeToString(payload)
	padded := base64.URLEncoding.EncodeToString(payload)

	cases := []struct {
		name    string
		in      string
		wantErr bool
		want    []byte
	}{
		{"empty", "", true, nil},
		{"raw url encoded", rawURL, false, payload},
		{"padded url encoded", padded, false, payload},
		{"invalid base64", "!!!!", true, nil},
		{"wrong length (5 bytes)", base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3, 4, 5}), true, nil},
		{"wrong length (7 bytes)", base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3, 4, 5, 6, 7}), true, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeHTTP2Settings(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (decoded %x)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("got %x, want %x", got, tc.want)
			}
		})
	}
}

// recordingHandler captures whether HandleStream was called.
type recordingHandler struct{ called bool }

func (r *recordingHandler) HandleStream(_ context.Context, _ *stream.Stream) error {
	r.called = true
	return nil
}

func TestProcessH1_UpgradeH2C_Happy(t *testing.T) {
	payload := []byte{0, 3, 0, 0, 0, 100} // MAX_CONCURRENT_STREAMS=100
	settingsB64 := base64.RawURLEncoding.EncodeToString(payload)
	req := "GET / HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Connection: Upgrade, HTTP2-Settings\r\n" +
		"Upgrade: h2c\r\n" +
		"HTTP2-Settings: " + settingsB64 + "\r\n\r\n"
	// Append a partial H2 preface to simulate a client that pipelined.
	trailer := []byte("PRI * HTT")
	data := append([]byte(req), trailer...)

	state := NewH1State()
	state.EnableH2Upgrade = true
	h := &recordingHandler{}
	var writes [][]byte
	writeFn := func(b []byte) {
		writes = append(writes, append([]byte(nil), b...))
	}
	err := ProcessH1(context.Background(), data, state, h, writeFn)
	if !errors.Is(err, ErrUpgradeH2C) {
		t.Fatalf("err = %v, want ErrUpgradeH2C", err)
	}
	if h.called {
		t.Fatal("handler must not run on upgrade")
	}
	if state.UpgradeInfo == nil {
		t.Fatal("UpgradeInfo not set")
	}
	if !bytes.Equal(state.UpgradeInfo.Settings, payload) {
		t.Fatalf("Settings = %x, want %x", state.UpgradeInfo.Settings, payload)
	}
	if state.UpgradeInfo.Method != "GET" {
		t.Fatalf("Method = %q, want GET", state.UpgradeInfo.Method)
	}
	if !bytes.Equal(state.UpgradeInfo.Remaining, trailer) {
		t.Fatalf("Remaining = %q, want %q", state.UpgradeInfo.Remaining, trailer)
	}
	// 101 response must be first and only write.
	if len(writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(writes))
	}
	wantResp := "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n"
	if string(writes[0]) != wantResp {
		t.Fatalf("101 response = %q, want %q", writes[0], wantResp)
	}
	// Hop-by-hop headers stripped.
	for _, h := range state.UpgradeInfo.Headers {
		switch h[0] {
		case "upgrade", "connection", "http2-settings":
			t.Fatalf("header %q not stripped", h[0])
		}
	}
}

func TestProcessH1_UpgradeH2C_Disabled(t *testing.T) {
	payload := []byte{0, 3, 0, 0, 0, 100}
	settingsB64 := base64.RawURLEncoding.EncodeToString(payload)
	req := "GET / HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Connection: Upgrade, HTTP2-Settings\r\n" +
		"Upgrade: h2c\r\n" +
		"HTTP2-Settings: " + settingsB64 + "\r\n\r\n"
	state := NewH1State()
	state.EnableH2Upgrade = false
	h := &recordingHandler{}
	err := ProcessH1(context.Background(), []byte(req), state, h, func([]byte) {})
	if errors.Is(err, ErrUpgradeH2C) {
		t.Fatalf("upgrade should be ignored when EnableH2Upgrade=false")
	}
	if !h.called {
		t.Fatal("handler must run when upgrade disabled")
	}
	if state.UpgradeInfo != nil {
		t.Fatal("UpgradeInfo must be nil when upgrade disabled")
	}
}

func TestProcessH1_UpgradeH2C_InvalidSettings(t *testing.T) {
	// Invalid base64: contains '!' which is not a valid base64 char.
	req := "GET / HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Connection: Upgrade, HTTP2-Settings\r\n" +
		"Upgrade: h2c\r\n" +
		"HTTP2-Settings: !!!notbase64!!!\r\n\r\n"
	state := NewH1State()
	state.EnableH2Upgrade = true
	h := &recordingHandler{}
	err := ProcessH1(context.Background(), []byte(req), state, h, func([]byte) {})
	if errors.Is(err, ErrUpgradeH2C) {
		t.Fatal("decode failure should fall through to H1, not upgrade")
	}
	if !h.called {
		t.Fatal("handler must run when settings decode fails")
	}
}
