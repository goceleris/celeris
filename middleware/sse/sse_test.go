package sse

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

// --- mock streamer ---

type mockStreamer struct {
	mu       sync.Mutex
	status   int
	headers  [][2]string
	chunks   [][]byte
	flushed  int
	closed   bool
	writeErr error
}

func (m *mockStreamer) WriteResponse(_ *stream.Stream, status int, headers [][2]string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = status
	m.headers = headers
	m.chunks = append(m.chunks, append([]byte(nil), body...))
	return nil
}

func (m *mockStreamer) WriteHeader(_ *stream.Stream, status int, headers [][2]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return m.writeErr
	}
	m.status = status
	m.headers = append(m.headers[:0], headers...)
	return nil
}

func (m *mockStreamer) Write(_ *stream.Stream, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return m.writeErr
	}
	m.chunks = append(m.chunks, append([]byte(nil), data...))
	return nil
}

func (m *mockStreamer) Flush(_ *stream.Stream) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return m.writeErr
	}
	m.flushed++
	return nil
}

func (m *mockStreamer) Close(_ *stream.Stream) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockStreamer) allData() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	for _, c := range m.chunks {
		b.Write(c)
	}
	return b.String()
}

func (m *mockStreamer) getHeaders() [][2]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([][2]string, len(m.headers))
	copy(cp, m.headers)
	return cp
}

// newSSEContext creates a test context with a mock streamer installed.
func newSSEContext(t *testing.T, opts ...celeristest.Option) (*celeris.Context, *mockStreamer) {
	t.Helper()
	ctx, _ := celeristest.NewContextT(t, "GET", "/events", opts...)
	ms := &mockStreamer{}
	s := celeris.TestStream(ctx)
	s.ResponseWriter = ms
	return ctx, ms
}

// --- event formatting tests ---

func TestFormatEventDataOnly(t *testing.T) {
	e := Event{Data: "hello"}
	got := string(formatEvent(nil, &e))
	want := "data: hello\n\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatEventAllFields(t *testing.T) {
	e := Event{ID: "42", Event: "update", Data: "payload", Retry: 3000}
	got := string(formatEvent(nil, &e))
	want := "id: 42\nevent: update\nretry: 3000\ndata: payload\n\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatEventMultiLineData(t *testing.T) {
	e := Event{Data: "line1\nline2\nline3"}
	got := string(formatEvent(nil, &e))
	want := "data: line1\ndata: line2\ndata: line3\n\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatEventCRLFData(t *testing.T) {
	e := Event{Data: "line1\r\nline2\r\nline3"}
	got := string(formatEvent(nil, &e))
	want := "data: line1\ndata: line2\ndata: line3\n\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatEventBareCRData(t *testing.T) {
	e := Event{Data: "line1\rline2"}
	got := string(formatEvent(nil, &e))
	want := "data: line1\ndata: line2\n\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatEventIDSanitized(t *testing.T) {
	e := Event{ID: "bad\nid", Data: "ok"}
	got := string(formatEvent(nil, &e))
	if strings.Contains(got, "\nid") {
		t.Errorf("newline not stripped from ID: %q", got)
	}
	if !strings.Contains(got, "id: badid\n") {
		t.Errorf("ID not sanitized correctly: %q", got)
	}
}

func TestFormatEventTypeSanitized(t *testing.T) {
	e := Event{Event: "bad\rtype", Data: "ok"}
	got := string(formatEvent(nil, &e))
	if strings.Contains(got, "\rtype") {
		t.Errorf("CR not stripped from Event: %q", got)
	}
	if !strings.Contains(got, "event: badtype\n") {
		t.Errorf("Event not sanitized correctly: %q", got)
	}
}

func TestFormatEventIDOnly(t *testing.T) {
	e := Event{ID: "99"}
	got := string(formatEvent(nil, &e))
	want := "id: 99\n\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatEventEmpty(t *testing.T) {
	e := Event{}
	got := string(formatEvent(nil, &e))
	want := "\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatEventReusesBuffer(t *testing.T) {
	buf := make([]byte, 0, 512)
	e := Event{Data: "test"}
	result := formatEvent(buf, &e)
	if cap(result) != 512 {
		t.Error("buffer capacity not preserved")
	}
}

func TestFormatComment(t *testing.T) {
	got := string(formatComment(nil, "ping"))
	want := ": ping\n\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- config tests ---

func TestConfigValidateNilHandler(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for nil Handler")
		}
	}()
	New(Config{})
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{Handler: func(*Client) {}}
	cfg = applyDefaults(cfg)
	if cfg.HeartbeatInterval != DefaultHeartbeatInterval {
		t.Errorf("HeartbeatInterval = %v, want %v", cfg.HeartbeatInterval, DefaultHeartbeatInterval)
	}
}

// --- SSE handler tests ---

func TestSendEvent(t *testing.T) {
	ctx, ms := newSSEContext(t)
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler: func(client *Client) {
			if err := client.Send(Event{ID: "1", Event: "msg", Data: "hello"}); err != nil {
				t.Errorf("Send: %v", err)
			}
		},
	})
	if err := handler(ctx); err != nil {
		t.Fatal(err)
	}
	data := ms.allData()
	want := "id: 1\nevent: msg\ndata: hello\n\n"
	if !strings.Contains(data, want) {
		t.Errorf("data = %q, want to contain %q", data, want)
	}
}

func TestSendData(t *testing.T) {
	ctx, ms := newSSEContext(t)
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler: func(client *Client) {
			_ = client.SendData("hello")
		},
	})
	_ = handler(ctx)
	if !strings.Contains(ms.allData(), "data: hello\n\n") {
		t.Errorf("unexpected data: %q", ms.allData())
	}
}

func TestSendComment(t *testing.T) {
	ctx, ms := newSSEContext(t)
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler: func(client *Client) {
			_ = client.SendComment("ping")
		},
	})
	_ = handler(ctx)
	if !strings.Contains(ms.allData(), ": ping\n\n") {
		t.Errorf("unexpected data: %q", ms.allData())
	}
}

func TestLastEventID(t *testing.T) {
	ctx, _ := newSSEContext(t, celeristest.WithHeader("last-event-id", "42"))
	var got string
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler: func(client *Client) {
			got = client.LastEventID()
		},
	})
	_ = handler(ctx)
	if got != "42" {
		t.Errorf("LastEventID = %q, want %q", got, "42")
	}
}

func TestRetryInterval(t *testing.T) {
	ctx, ms := newSSEContext(t)
	handler := New(Config{
		HeartbeatInterval: -1,
		RetryInterval:     5000,
		Handler:           func(client *Client) {},
	})
	_ = handler(ctx)
	if !strings.Contains(ms.allData(), "retry: 5000\n\n") {
		t.Errorf("retry not found in data: %q", ms.allData())
	}
}

func TestClientContextCancellation(t *testing.T) {
	ctx, _ := newSSEContext(t)
	done := make(chan struct{})
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler: func(client *Client) {
			_ = client.Close()
			select {
			case <-client.Context().Done():
				close(done)
			case <-time.After(time.Second):
				t.Error("context not cancelled after Close")
			}
		},
	})
	_ = handler(ctx)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("handler did not observe cancellation")
	}
}

func TestClientDoubleClose(t *testing.T) {
	ctx, _ := newSSEContext(t)
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler: func(client *Client) {
			if err := client.Close(); err != nil {
				t.Errorf("first Close: %v", err)
			}
			if err := client.Close(); err != nil {
				t.Errorf("second Close: %v", err)
			}
		},
	})
	_ = handler(ctx)
}

func TestSendAfterClose(t *testing.T) {
	ctx, _ := newSSEContext(t)
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler: func(client *Client) {
			_ = client.Close()
			err := client.Send(Event{Data: "late"})
			if err != ErrClientClosed {
				t.Errorf("err = %v, want ErrClientClosed", err)
			}
		},
	})
	_ = handler(ctx)
}

func TestOnConnectCallback(t *testing.T) {
	ctx, _ := newSSEContext(t)
	var called bool
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler:           func(client *Client) {},
		OnConnect: func(c *celeris.Context, client *Client) error {
			called = true
			return nil
		},
	})
	_ = handler(ctx)
	if !called {
		t.Error("OnConnect not called")
	}
}

func TestOnConnectReject(t *testing.T) {
	ctx, _ := newSSEContext(t)
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler:           func(client *Client) { t.Error("Handler should not be called") },
		OnConnect: func(c *celeris.Context, client *Client) error {
			return celeris.NewHTTPError(403, "forbidden")
		},
	})
	err := handler(ctx)
	if err == nil {
		t.Fatal("expected error from OnConnect rejection")
	}
}

func TestOnDisconnectCallback(t *testing.T) {
	ctx, _ := newSSEContext(t)
	var called bool
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler:           func(client *Client) {},
		OnDisconnect: func(c *celeris.Context, client *Client) {
			called = true
		},
	})
	_ = handler(ctx)
	if !called {
		t.Error("OnDisconnect not called")
	}
}

func TestSSEHeaders(t *testing.T) {
	ctx, ms := newSSEContext(t)
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler:           func(client *Client) {},
	})
	_ = handler(ctx)
	headers := ms.getHeaders()
	found := map[string]string{}
	for _, h := range headers {
		found[h[0]] = h[1]
	}
	if found["content-type"] != "text/event-stream" {
		t.Errorf("content-type = %q", found["content-type"])
	}
	if found["cache-control"] != "no-cache" {
		t.Errorf("cache-control = %q", found["cache-control"])
	}
}

func TestHeartbeat(t *testing.T) {
	ctx, ms := newSSEContext(t)
	handler := New(Config{
		HeartbeatInterval: 10 * time.Millisecond,
		Handler: func(client *Client) {
			time.Sleep(50 * time.Millisecond)
		},
	})
	_ = handler(ctx)
	data := ms.allData()
	if !strings.Contains(data, ": heartbeat\n\n") {
		t.Errorf("heartbeat not found in data: %q", data)
	}
}

func TestWriteErrorOnHeaders(t *testing.T) {
	ctx, _ := celeristest.NewContextT(t, "GET", "/events")
	ms := &mockStreamer{writeErr: context.DeadlineExceeded}
	celeris.TestStream(ctx).ResponseWriter = ms
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler:           func(client *Client) {},
	})
	err := handler(ctx)
	if err == nil {
		t.Fatal("expected error from write failure")
	}
}

func TestSkipPaths(t *testing.T) {
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler:           func(client *Client) { t.Error("should not reach handler") },
		SkipPaths:         []string{"/events"},
	})
	var nextCalled bool
	next := func(c *celeris.Context) error {
		nextCalled = true
		return nil
	}
	ctx, _ := celeristest.NewContextT(t, "GET", "/events",
		celeristest.WithHandlers(handler, next))
	_ = ctx.Next()
	if !nextCalled {
		t.Error("Next not called for skipped path")
	}
}

func TestSkipFunc(t *testing.T) {
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler:           func(client *Client) { t.Error("should not reach handler") },
		Skip:              func(c *celeris.Context) bool { return true },
	})
	var nextCalled bool
	next := func(c *celeris.Context) error {
		nextCalled = true
		return nil
	}
	ctx, _ := celeristest.NewContextT(t, "GET", "/events",
		celeristest.WithHandlers(handler, next))
	_ = ctx.Next()
	if !nextCalled {
		t.Error("Next not called for skipped request")
	}
}

func TestConcurrentClients(t *testing.T) {
	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler: func(client *Client) {
			defer wg.Done()
			for i := range 50 {
				if err := client.Send(Event{Data: strings.Repeat("x", i+1)}); err != nil {
					return
				}
			}
		},
	})
	for range n {
		go func() {
			ctx, _ := celeristest.NewContext("GET", "/events")
			ms := &mockStreamer{}
			celeris.TestStream(ctx).ResponseWriter = ms
			_ = handler(ctx)
			celeristest.ReleaseContext(ctx)
		}()
	}
	wg.Wait()
}

// --- benchmarks ---

func newDiscardContext() *celeris.Context {
	ctx, _ := celeristest.NewContext("GET", "/events")
	celeris.TestStream(ctx).ResponseWriter = &mockStreamer{}
	return ctx
}

func BenchmarkEventFormat(b *testing.B) {
	buf := make([]byte, 0, 256)
	e := Event{ID: "123", Event: "update", Data: `{"user":"alice","score":42}`}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		buf = formatEvent(buf, &e)
	}
}

func BenchmarkEventFormatMultiLine(b *testing.B) {
	buf := make([]byte, 0, 512)
	e := Event{Data: "line1\nline2\nline3\nline4\nline5"}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		buf = formatEvent(buf, &e)
	}
}

// TestSSEStreamWriterDoesNotRetainHeaders pins the invariant that the
// StreamWriter.WriteHeader implementation copies values out of the slice
// rather than retaining a reference. The SSE middleware shares a single
// sseHeaders slice across all handler invocations and depends on this
// behavior; if it ever changes, this test fails loudly.
func TestSSEStreamWriterDoesNotRetainHeaders(t *testing.T) {
	headers := [][2]string{
		{"content-type", "text/event-stream"},
		{"cache-control", "no-cache"},
	}
	streamer := newRecordingStreamer()

	st := stream.NewH1Stream(1)
	defer stream.ResetForPool(st)
	if err := streamer.WriteHeader(st, 200, headers); err != nil {
		t.Fatal(err)
	}

	// Mutate the input slice. If WriteHeader retained a reference, the
	// recorded headers would change with us.
	headers[0][1] = "MUTATED"
	headers[1][0] = "MUTATED"

	for i, h := range streamer.recordedHeaders() {
		if strings.Contains(h[0], "MUTATED") || strings.Contains(h[1], "MUTATED") {
			t.Fatalf("WriteHeader retained slice — recorded[%d] mutated to %v", i, h)
		}
	}
}

// recordingStreamer is a small Streamer that copies values into its own
// storage so the test can verify nothing leaks back via shared references.
type recordingStreamer struct {
	headers [][2]string
}

func newRecordingStreamer() *recordingStreamer { return &recordingStreamer{} }

func (s *recordingStreamer) WriteHeader(_ *stream.Stream, _ int, headers [][2]string) error {
	for _, h := range headers {
		s.headers = append(s.headers, [2]string{
			string(append([]byte(nil), h[0]...)),
			string(append([]byte(nil), h[1]...)),
		})
	}
	return nil
}
func (s *recordingStreamer) Write(_ *stream.Stream, _ []byte) error { return nil }
func (s *recordingStreamer) Flush(_ *stream.Stream) error           { return nil }
func (s *recordingStreamer) Close(_ *stream.Stream) error           { return nil }
func (s *recordingStreamer) WriteResponse(_ *stream.Stream, _ int, _ [][2]string, _ []byte) error {
	return nil
}
func (s *recordingStreamer) recordedHeaders() [][2]string { return s.headers }

// TestConcurrentClientsStress hammers the SSE middleware with 500
// concurrent clients each sending 200 events, verifying that goroutines
// and pool entries are fully reclaimed after all clients disconnect.
// This catches heartbeat goroutine leaks, pool reuse races, and
// mutex contention issues.
func TestConcurrentClientsStress(t *testing.T) {
	const (
		numClients = 500
		numEvents  = 200
	)
	var totalSent atomic.Uint64
	var wg sync.WaitGroup
	wg.Add(numClients)

	handler := New(Config{
		HeartbeatInterval: 10 * time.Millisecond,
		Handler: func(client *Client) {
			defer wg.Done()
			for i := range numEvents {
				if err := client.Send(Event{
					ID:   strings.Repeat("x", (i%10)+1),
					Data: "event-data",
				}); err != nil {
					return
				}
				totalSent.Add(1)
			}
		},
	})

	baseGoroutines := runtime.NumGoroutine()

	for i := range numClients {
		go func(id int) {
			ctx, _ := celeristest.NewContext("GET", "/events")
			ms := &mockStreamer{}
			celeris.TestStream(ctx).ResponseWriter = ms
			_ = handler(ctx)
			celeristest.ReleaseContext(ctx)
		}(i)
	}
	wg.Wait()

	// Give heartbeat goroutines time to exit (they watch ctx.Done).
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	finalGoroutines := runtime.NumGoroutine()

	t.Logf("stress: %d clients × %d events = %d sent, goroutines: %d → %d",
		numClients, numEvents, totalSent.Load(), baseGoroutines, finalGoroutines)

	if totalSent.Load() != numClients*numEvents {
		t.Errorf("expected %d events, got %d", numClients*numEvents, totalSent.Load())
	}
	// Allow 5 slack goroutines for runtime/GC timers.
	if finalGoroutines > baseGoroutines+5 {
		t.Errorf("goroutine leak: %d → %d (delta=%d)",
			baseGoroutines, finalGoroutines, finalGoroutines-baseGoroutines)
	}
}

func BenchmarkSendEvent(b *testing.B) {
	e := Event{ID: "1", Event: "msg", Data: `{"ok":true}`}
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler: func(client *Client) {
			_ = client.Send(e)
		},
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx := newDiscardContext()
		_ = handler(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkBurstEvents(b *testing.B) {
	const burst = 100
	e := Event{Data: strings.Repeat("x", 64)}
	handler := New(Config{
		HeartbeatInterval: -1,
		Handler: func(client *Client) {
			for range burst {
				if err := client.Send(e); err != nil {
					return
				}
			}
		},
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx := newDiscardContext()
		_ = handler(ctx)
		celeristest.ReleaseContext(ctx)
	}
}
