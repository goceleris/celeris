package conn

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

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

// RFC 7540 section 3.2.1, on the conn layer (celeris#889, items 41 and 43):
// exactly one HTTP2-Settings header upgrades, and its value may be empty; a
// repeated one is served as HTTP/1.1, as is one that does not decode.
func TestProcessH1_UpgradeH2C_SettingsRule889(t *testing.T) {
	const head = "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n"
	const v = "AAMAAABkAAQAoAAAAAIAAAAA"
	cases := []struct {
		name    string
		lines   string
		upgrade bool
	}{
		{"one", "HTTP2-Settings: " + v + "\r\n", true},
		{"one empty", "HTTP2-Settings: \r\n", true},
		{"missing", "", false},
		{"repeated", "HTTP2-Settings: " + v + "\r\nHTTP2-Settings: " + v + "\r\n", false},
		{"repeated, one empty", "HTTP2-Settings: " + v + "\r\nHTTP2-Settings:\r\n", false},
		{"undecodable", "HTTP2-Settings: !!!\r\n", false},
		{"length not a multiple of 6", "HTTP2-Settings: AAMAAABkAA\r\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := NewH1State()
			state.EnableH2Upgrade = true
			h := &recordingHandler{}
			var writes [][]byte
			err := ProcessH1(context.Background(), []byte(head+tc.lines+"\r\n"), state, h, func(b []byte) {
				writes = append(writes, append([]byte(nil), b...))
			})
			if got := errors.Is(err, ErrUpgradeH2C); got != tc.upgrade {
				t.Fatalf("upgraded = %v (err %v), want %v", got, err, tc.upgrade)
			}
			if h.called == tc.upgrade {
				t.Fatalf("handler called = %v on a request that upgrades = %v", h.called, tc.upgrade)
			}
			if tc.upgrade {
				if len(writes) != 1 || !bytes.HasPrefix(writes[0], []byte("HTTP/1.1 101 ")) {
					t.Fatalf("writes = %q, want one 101", writes)
				}
				if tc.name == "one empty" && len(state.UpgradeInfo.Settings) != 0 {
					t.Fatalf("Settings = %x, want none", state.UpgradeInfo.Settings)
				}
			}
		})
	}
}
