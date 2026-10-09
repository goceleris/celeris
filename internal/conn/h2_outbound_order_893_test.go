package conn

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

type asyncHandler893 struct {
	run func(context.Context, *stream.Stream) error
}

func (h *asyncHandler893) HandleStream(ctx context.Context, s *stream.Stream) error {
	return h.run(ctx, s)
}
func (h *asyncHandler893) RouteAsync(_, _ string) bool { return true }
func (h *asyncHandler893) HasAsyncRoutes() bool        { return true }

// TestPoolHandlerOverTheBudgetSendsItsBodyInOrder893: a pool handler that the
// connection's budget refuses a copy sends the rest of its body itself, as the
// windows open, through the write queue. It must not buffer what is left once
// the budget has room again: the event loop writes a stream's buffered DATA
// straight to the connection, ahead of the chunks still in the queue, and the
// body arrived out of order (seen in the engine test's second phase). Here
// the budget is freed while a chunk is still queued, and a WINDOW_UPDATE is
// processed before the queue is drained.
func TestPoolHandlerOverTheBudgetSendsItsBodyInOrder893(t *testing.T) {
	var mu sync.Mutex
	var wire bytes.Buffer
	write := func(b []byte) { mu.Lock(); wire.Write(b); mu.Unlock() }
	body := make([]byte, 300000)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	done := make(chan struct{})
	h := &asyncHandler893{run: func(_ context.Context, s *stream.Stream) error {
		defer close(done)
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "application/octet-stream"}}, body)
	}}
	st := NewH2State(h, H2Config{}, write, nil)
	mgr := st.processor.GetManager()
	// Another stream holds the whole budget, so stream 1's handler gets no copy.
	holder := mgr.CreateStream(101)
	holder.SetState(stream.StateOpen)
	holder.SetWindowSize(0)
	holder.BufferOutbound(make([]byte, stream.OutboundBudget), true)

	ctx := context.Background()
	in := append([]byte(http2.ClientPreface), frames893(t, func(fr *http2.Framer, enc *hpack.Encoder, hb *bytes.Buffer) {
		_ = fr.WriteSettings()
		hb.Reset()
		for _, hf := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/b"}} {
			_ = enc.WriteField(hf)
		}
		_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
	})...)
	if err := ProcessH2(ctx, in, st, h, write, H2Config{}); err != nil {
		t.Fatal(err)
	}
	// queued reports how many frame buffers the write queue holds (not the
	// pending flag, which a drain can leave set with nothing queued).
	queued := func() int {
		n := 0
		for i := range st.writeQueue.shards {
			sh := &st.writeQueue.shards[i]
			sh.mu.Lock()
			n += len(sh.bufs)
			sh.mu.Unlock()
		}
		return n
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for until := time.Now().Add(2 * time.Second); !cond(); {
			if time.Now().After(until) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	window := func(n uint32) []byte {
		return frames893(t, func(fr *http2.Framer, _ *hpack.Encoder, _ *bytes.Buffer) {
			_ = fr.WriteWindowUpdate(1, n)
			_ = fr.WriteWindowUpdate(0, n)
		})
	}
	// The handler runs on the pool: its HEADERS (with the first 65535 bytes)
	// are either still queued or already drained by ProcessH2's last drain.
	onWire := func() bool {
		mu.Lock()
		defer mu.Unlock()
		raw := wire.Bytes()
		for len(raw) >= 9 {
			n := int(raw[0])<<16 | int(raw[1])<<8 | int(raw[2])
			if binary.BigEndian.Uint32(raw[5:9])&0x7fffffff == 1 {
				return true
			}
			raw = raw[9+n:]
		}
		return false
	}
	waitFor("the HEADERS", func() bool { return queued() > 0 || onWire() })
	st.DrainWriteQueue(write) // HEADERS + the first 65535 bytes
	s1, ok := mgr.GetStream(1)
	if !ok {
		t.Fatal("stream 1 is gone")
	}
	// More window, granted without a drain (as the loop's recv path does
	// before it drains the queue): the handler sends a chunk, which stays
	// queued.
	if err := st.processor.HandleRawWindowUpdate(1, []byte{0, 0, 0xc3, 0x50}); err != nil { // 50000
		t.Fatal(err)
	}
	if err := st.processor.HandleRawWindowUpdate(0, []byte{0, 0, 0xc3, 0x50}); err != nil {
		t.Fatal(err)
	}
	waitFor("the second chunk", func() bool { return queued() > 0 })
	// The budget is free again. The handler must not buffer the rest now.
	mgr.DeleteStream(101)
	time.Sleep(20 * time.Millisecond)
	s1.WriteLock()
	s1.WriteUnlock()
	// Stream window first (the connection window is 0, so nothing moves),
	// then a connection WINDOW_UPDATE arrives as the first frame of a recv,
	// before the loop has drained the queue: whatever is buffered goes
	// straight to the wire.
	if err := st.processor.HandleRawWindowUpdate(1, []byte{0, 0, 0xc3, 0x50}); err != nil {
		t.Fatal(err)
	}
	connWindow := frames893(t, func(fr *http2.Framer, _ *hpack.Encoder, _ *bytes.Buffer) { _ = fr.WriteWindowUpdate(0, 50000) })
	if err := ProcessH2(ctx, connWindow, st, h, write, H2Config{}); err != nil {
		t.Fatal(err)
	}
	// Then the rest of the window, drained as it comes, until the stream ends
	// on the wire.
	parse := func() (got []byte, first string, ended bool) {
		mu.Lock()
		raw := append([]byte(nil), wire.Bytes()...)
		mu.Unlock()
		for len(raw) >= 9 {
			n := int(raw[0])<<16 | int(raw[1])<<8 | int(raw[2])
			typ, flags, sid := raw[3], raw[4], binary.BigEndian.Uint32(raw[5:9])&0x7fffffff
			if sid == 1 {
				if first == "" {
					first = fmt.Sprintf("type %d", typ)
				}
				if typ == 0x0 {
					got = append(got, raw[9:9+n]...)
					ended = ended || flags&0x1 != 0
				}
			}
			raw = raw[9+n:]
		}
		return got, first, ended
	}
	for i := 0; i < 50; i++ {
		if _, _, ended := parse(); ended {
			break
		}
		if err := ProcessH2(ctx, window(50000), st, h, write, H2Config{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
		st.DrainWriteQueue(write)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not finish")
	}
	st.DrainWriteQueue(write)
	got, first, ended := parse()
	if first != "type 1" {
		t.Errorf("stream 1's first frame is %s, want HEADERS (type 1)", first)
	}
	if !ended || len(got) != len(body) {
		t.Fatalf("stream 1: %d of %d body bytes, END_STREAM %v", len(got), len(body), ended)
	}
	for i := range got {
		if got[i] != body[i] {
			t.Fatalf("stream 1's body is out of order from byte %d (%q, want %q)", i, got[i], body[i])
		}
	}
	if b := st.processor.GetManager().OutboundHeld(); b != 0 {
		t.Errorf("held %d after the stream ended, want 0", b)
	}
}

func frames893(t *testing.T, f func(fr *http2.Framer, enc *hpack.Encoder, hb *bytes.Buffer)) []byte {
	t.Helper()
	var out bytes.Buffer
	fr := http2.NewFramer(&out, nil)
	var hb bytes.Buffer
	f(fr, hpack.NewEncoder(&hb), &hb)
	return out.Bytes()
}

type syncHandler893 struct {
	run func(context.Context, *stream.Stream) error
}

func (h *syncHandler893) HandleStream(ctx context.Context, s *stream.Stream) error {
	return h.run(ctx, s)
}

// TestSyncStreamOnThePoolForTheBudgetSendsHeadersFirst893: a sync GET (an
// inline-eligible stream) runs on the worker pool while its connection is
// over the outbound budget. Its response must not go to its stream's
// buffer, even when the budget has room again by the time the handler
// writes: the HEADERS go through the write queue, and a WINDOW_UPDATE that
// is the first frame of a recv writes a stream's buffered DATA straight to
// the connection, before the loop drains the queue. DATA then reached the
// wire before its stream's HEADERS (#903's mechanism, which an inline
// handler could not reach: its HEADERS go to the connection at once).
func TestSyncStreamOnThePoolForTheBudgetSendsHeadersFirst893(t *testing.T) {
	var mu sync.Mutex
	var wire bytes.Buffer
	write := func(b []byte) { mu.Lock(); wire.Write(b); mu.Unlock() }
	body := make([]byte, 300000)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	gate := make(chan struct{})
	onPool := make(chan bool, 1)
	done := make(chan struct{})
	h := &syncHandler893{run: func(_ context.Context, s *stream.Stream) error {
		defer close(done)
		_, pooled := s.ResponseWriter.(*h2ResponseAdapter)
		onPool <- pooled
		if pooled {
			<-gate // inline, the event loop would wait here: do not
		}
		return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "application/octet-stream"}}, body)
	}}
	st := NewH2State(h, H2Config{}, write, nil)
	mgr := st.processor.GetManager()
	// Another stream (a download whose client grants no window) holds the
	// whole budget, and the connection window is spent.
	holder := mgr.CreateStream(101)
	holder.SetState(stream.StateOpen)
	holder.SetWindowSize(0)
	holder.BufferOutbound(make([]byte, stream.OutboundBudget), true)
	mgr.UpdateConnectionWindow(-mgr.GetConnectionWindow())

	ctx := context.Background()
	in := append([]byte(http2.ClientPreface), frames893(t, func(fr *http2.Framer, enc *hpack.Encoder, hb *bytes.Buffer) {
		_ = fr.WriteSettings()
		hb.Reset()
		for _, hf := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/b"}} {
			_ = enc.WriteField(hf)
		}
		_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
	})...)
	if err := ProcessH2(ctx, in, st, h, write, H2Config{}); err != nil {
		t.Fatal(err)
	}
	select {
	case pooled := <-onPool:
		if !pooled {
			<-done
			t.Fatal("the sync stream's handler ran inline on a connection over its outbound budget")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handler did not run")
	}
	// The download ends (its client reset it): the budget has room again
	// before the handler writes.
	mgr.DeleteStream(101)
	if b := mgr.OutboundHeld(); b != 0 {
		t.Fatalf("held %d after the download ended, want 0", b)
	}
	close(gate)
	queued := func() int {
		n := 0
		for i := range st.writeQueue.shards {
			sh := &st.writeQueue.shards[i]
			sh.mu.Lock()
			n += len(sh.bufs)
			sh.mu.Unlock()
		}
		return n
	}
	// The handler has written once its HEADERS are queued (a copy of the
	// body, if it is made, is made before that).
	for until := time.Now().Add(2 * time.Second); queued() == 0; {
		if time.Now().After(until) {
			t.Fatal("timed out waiting for the HEADERS to be queued")
		}
		time.Sleep(time.Millisecond)
	}
	parse := func() (got []byte, first string, ended bool) {
		mu.Lock()
		raw := append([]byte(nil), wire.Bytes()...)
		mu.Unlock()
		for len(raw) >= 9 {
			n := int(raw[0])<<16 | int(raw[1])<<8 | int(raw[2])
			typ, flags, sid := raw[3], raw[4], binary.BigEndian.Uint32(raw[5:9])&0x7fffffff
			if sid == 1 {
				if first == "" {
					first = fmt.Sprintf("type %d", typ)
				}
				if typ == 0x0 {
					got = append(got, raw[9:9+n]...)
					ended = ended || flags&0x1 != 0
				}
			}
			raw = raw[9+n:]
		}
		return got, first, ended
	}
	if got, first, _ := parse(); first != "" || len(got) != 0 {
		t.Fatalf("stream 1 is on the wire (%s, %d DATA bytes) before any window was granted", first, len(got))
	}
	// The peer grants connection window; that WINDOW_UPDATE is the first
	// frame of a recv, processed before the loop drains the queue.
	connWindow := frames893(t, func(fr *http2.Framer, _ *hpack.Encoder, _ *bytes.Buffer) { _ = fr.WriteWindowUpdate(0, 50000) })
	if err := ProcessH2(ctx, connWindow, st, h, write, H2Config{}); err != nil {
		t.Fatal(err)
	}
	window := func(n uint32) []byte {
		return frames893(t, func(fr *http2.Framer, _ *hpack.Encoder, _ *bytes.Buffer) {
			_ = fr.WriteWindowUpdate(1, n)
			_ = fr.WriteWindowUpdate(0, n)
		})
	}
	for i := 0; i < 50; i++ {
		if _, _, ended := parse(); ended {
			break
		}
		time.Sleep(5 * time.Millisecond)
		st.DrainWriteQueue(write)
		if err := ProcessH2(ctx, window(50000), st, h, write, H2Config{}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not finish")
	}
	st.DrainWriteQueue(write)
	got, first, ended := parse()
	if first != "type 1" {
		t.Errorf("stream 1's first frame is %s, want HEADERS (type 1): DATA reached the wire before its HEADERS", first)
	}
	if !ended || len(got) != len(body) {
		t.Fatalf("stream 1: %d of %d body bytes, END_STREAM %v", len(got), len(body), ended)
	}
	for i := range got {
		if got[i] != body[i] {
			t.Fatalf("stream 1's body is out of order from byte %d (%q, want %q)", i, got[i], body[i])
		}
	}
	if b := mgr.OutboundHeld(); b != 0 {
		t.Errorf("held %d after the stream ended, want 0", b)
	}
}
