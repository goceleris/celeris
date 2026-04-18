package conn

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/goceleris/celeris/protocol/h2/frame"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

// captureHandler records whether HandleStream was called and the stream seen.
type captureHandler struct {
	mu     sync.Mutex
	called bool
	method string
	path   string
	body   string
}

func (c *captureHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.called = true
	for _, h := range s.GetHeaders() {
		switch h[0] {
		case ":method":
			c.method = h[1]
		case ":path":
			c.path = h[1]
		}
	}
	if d := s.GetData(); len(d) > 0 {
		c.body = string(d)
	}
	// Respond so the lifecycle completes.
	if s.ResponseWriter != nil {
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{
			{"content-type", "text/plain"},
		}, []byte("ok"))
	}
	return nil
}

func TestNewH2StateFromUpgrade_Basic(t *testing.T) {
	// Settings: MAX_CONCURRENT_STREAMS=50, INITIAL_WINDOW_SIZE=131072.
	settings := []byte{
		0, 3, 0, 0, 0, 50,
		0, 4, 0, 2, 0, 0,
	}
	info := &UpgradeInfo{
		Settings: settings,
		Method:   "GET",
		URI:      "/hello",
		Headers: [][2]string{
			{"host", "example.com"},
			{"user-agent", "upgrade-test"},
		},
		Body: nil,
	}

	h := &captureHandler{}
	var writes []byte
	var mu sync.Mutex
	write := func(b []byte) {
		mu.Lock()
		writes = append(writes, b...)
		mu.Unlock()
	}
	state, err := NewH2StateFromUpgrade(h, H2Config{}, write, -1, info)
	if err != nil {
		t.Fatalf("NewH2StateFromUpgrade: %v", err)
	}
	if !state.serverPrefaceSent {
		t.Fatal("serverPrefaceSent not set")
	}
	if !h.called {
		t.Fatal("handler not invoked on stream 1")
	}
	if h.method != "GET" || h.path != "/hello" {
		t.Fatalf("method=%q path=%q, want GET /hello", h.method, h.path)
	}
	// Server preface + response must have been written.
	mu.Lock()
	got := writes
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no bytes written (expected SETTINGS + response)")
	}
}

func TestProcessH2_PartialPreface(t *testing.T) {
	settings := []byte{0, 3, 0, 0, 0, 10}
	info := &UpgradeInfo{
		Settings: settings,
		Method:   "GET",
		URI:      "/",
		Headers:  [][2]string{{"host", "x"}},
	}
	h := &captureHandler{}
	var mu sync.Mutex
	var writes []byte
	write := func(b []byte) {
		mu.Lock()
		writes = append(writes, b...)
		mu.Unlock()
	}
	state, err := NewH2StateFromUpgrade(h, H2Config{}, write, -1, info)
	if err != nil {
		t.Fatalf("NewH2StateFromUpgrade: %v", err)
	}

	// Feed only the first half of the H2 client preface.
	half := []byte(frame.ClientPreface)[:10]
	if err := ProcessH2(context.Background(), half, state, h, write, H2Config{}); err != nil {
		t.Fatalf("ProcessH2 partial preface: %v", err)
	}
	if state.initialized {
		t.Fatal("should not be initialized on partial preface")
	}
	if !bytes.Equal(state.prefaceBuf, half) {
		t.Fatalf("prefaceBuf = %q, want %q", state.prefaceBuf, half)
	}

	// Deliver the rest.
	rest := []byte(frame.ClientPreface)[10:]
	if err := ProcessH2(context.Background(), rest, state, h, write, H2Config{}); err != nil {
		t.Fatalf("ProcessH2 rest: %v", err)
	}
	if !state.initialized {
		t.Fatal("should be initialized after full preface")
	}
	if len(state.prefaceBuf) != 0 {
		t.Fatalf("prefaceBuf not cleared: %q", state.prefaceBuf)
	}
}

func TestProcessH2_WrongPrefaceRejected(t *testing.T) {
	// No upgrade — plain H2 connection.
	h := &captureHandler{}
	write := func([]byte) {}
	state := NewH2State(h, H2Config{}, write, -1)
	garbage := []byte("GET / HTTP/1.1\r\n\r\n")
	err := ProcessH2(context.Background(), garbage, state, h, write, H2Config{})
	if err == nil {
		t.Fatal("expected error on invalid preface")
	}
}

func TestInjectStreamHeaders_WithBody(t *testing.T) {
	info := &UpgradeInfo{
		Settings: nil,
		Method:   "POST",
		URI:      "/submit",
		Headers: [][2]string{
			{"host", "example.com"},
			{"content-type", "text/plain"},
		},
		Body: []byte("hello world"),
	}
	h := &captureHandler{}
	write := func([]byte) {}
	state, err := NewH2StateFromUpgrade(h, H2Config{}, write, -1, info)
	_ = state
	if err != nil {
		t.Fatalf("NewH2StateFromUpgrade: %v", err)
	}
	if !h.called {
		t.Fatal("handler not invoked")
	}
	if h.method != "POST" || h.path != "/submit" || h.body != "hello world" {
		t.Fatalf("method=%q path=%q body=%q", h.method, h.path, h.body)
	}
}
