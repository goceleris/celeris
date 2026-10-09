package conn

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// The HTTP/2 outbound flush paths (celeris#903, #904): what a WINDOW_UPDATE or
// a SETTINGS_INITIAL_WINDOW_SIZE sends of a stream's buffered DATA must follow
// the stream's HEADERS and the DATA queued before it, and no DATA frame, from
// any send path, may exceed either window the peer granted (RFC 9113 §6.9.1).
// The tests run in memory: NewH2State and ProcessH2, no engine.

// wireFrame is one frame of what reached the connection.
type wireFrame struct {
	typ, flags byte
	sid        uint32
	payload    []byte
}

func (f wireFrame) String() string {
	switch f.typ {
	case 0x0:
		return fmt.Sprintf("DATA(%d)", len(f.payload))
	case 0x1:
		return "HEADERS"
	case 0x3:
		return "RST_STREAM"
	}
	return fmt.Sprintf("type%d", f.typ)
}

// flushConn is one in-memory connection and what it wrote.
type flushConn struct {
	t    *testing.T
	st   *H2State
	h    stream.Handler
	mu   sync.Mutex
	wire bytes.Buffer
}

func newFlushConn(t *testing.T, h stream.Handler, cfg H2Config) *flushConn {
	t.Helper()
	c := &flushConn{t: t, h: h}
	c.st = NewH2State(h, cfg, c.write, nil)
	t.Cleanup(func() { c.st.processor.GetManager().Close() })
	return c
}

func (c *flushConn) write(b []byte) { c.mu.Lock(); c.wire.Write(b); c.mu.Unlock() }

func (c *flushConn) process(in []byte) {
	c.t.Helper()
	if err := ProcessH2(context.Background(), in, c.st, c.h, c.write, H2Config{}); err != nil {
		c.t.Error(err)
	}
}

func (c *flushConn) drain() { c.st.DrainWriteQueue(c.write) }

// open sends the preface, the client's SETTINGS, a connection WINDOW_UPDATE
// (0 for none) and a GET on each of ids.
func (c *flushConn) open(connCredit uint32, settings []http2.Setting, ids ...uint32) {
	c.t.Helper()
	c.process(append([]byte(http2.ClientPreface), frames893(c.t, func(fr *http2.Framer, enc *hpack.Encoder, hb *bytes.Buffer) {
		_ = fr.WriteSettings(settings...)
		if connCredit > 0 {
			_ = fr.WriteWindowUpdate(0, connCredit)
		}
		for _, id := range ids {
			hb.Reset()
			for _, hf := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/b"}} {
				_ = enc.WriteField(hf)
			}
			_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
		}
	})...))
}

// grant sends WINDOW_UPDATEs: one for the connection, and one for each of ids.
func (c *flushConn) grant(n uint32, connToo bool, ids ...uint32) {
	c.t.Helper()
	c.process(frames893(c.t, func(fr *http2.Framer, _ *hpack.Encoder, _ *bytes.Buffer) {
		for _, id := range ids {
			_ = fr.WriteWindowUpdate(id, n)
		}
		if connToo {
			_ = fr.WriteWindowUpdate(0, n)
		}
	}))
}

func (c *flushConn) setInitialWindow(n uint32) {
	c.t.Helper()
	c.process(frames893(c.t, func(fr *http2.Framer, _ *hpack.Encoder, _ *bytes.Buffer) {
		_ = fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: n})
	}))
}

// frames parses everything written so far.
func (c *flushConn) frames() []wireFrame {
	c.mu.Lock()
	raw := append([]byte(nil), c.wire.Bytes()...)
	c.mu.Unlock()
	var out []wireFrame
	for len(raw) >= 9 {
		n := int(raw[0])<<16 | int(raw[1])<<8 | int(raw[2])
		if len(raw) < 9+n {
			break
		}
		out = append(out, wireFrame{typ: raw[3], flags: raw[4], sid: binary.BigEndian.Uint32(raw[5:9]) & 0x7fffffff, payload: raw[9 : 9+n]})
		raw = raw[9+n:]
	}
	return out
}

// streamWire is what reached the wire for one stream.
type streamWire struct {
	seq    []string // frame kinds in wire order
	data   []byte
	ended  bool // END_STREAM seen
	after  int  // frames after the END_STREAM
	rst    []http2.ErrCode
	hdrIdx int // index of HEADERS in seq, -1 if none
}

func (c *flushConn) stream(sid uint32) streamWire {
	w := streamWire{hdrIdx: -1}
	for _, f := range c.frames() {
		if f.sid != sid {
			continue
		}
		if w.ended {
			w.after++
		}
		w.seq = append(w.seq, f.String())
		switch f.typ {
		case 0x0:
			w.data = append(w.data, f.payload...)
			w.ended = w.ended || f.flags&0x1 != 0
		case 0x1:
			if w.hdrIdx < 0 {
				w.hdrIdx = len(w.seq) - 1
			}
			w.ended = w.ended || f.flags&0x1 != 0
		case 0x3:
			if len(f.payload) == 4 {
				w.rst = append(w.rst, http2.ErrCode(binary.BigEndian.Uint32(f.payload)))
			}
		}
	}
	return w
}

// summary shortens a frame sequence for a failure message.
func (w streamWire) summary() string {
	if len(w.seq) <= 8 {
		return fmt.Sprint(w.seq)
	}
	return fmt.Sprintf("%v ... %v (%d frames)", w.seq[:4], w.seq[len(w.seq)-3:], len(w.seq))
}

func pattern904(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

func waitFlush(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for until := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(until) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func chanClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// checkWholeBody fails t unless s's wire is HEADERS first, then exactly body
// in order, END_STREAM on the last frame and nothing after it.
func checkWholeBody(t *testing.T, label string, w streamWire, body []byte) {
	t.Helper()
	if w.hdrIdx != 0 {
		t.Errorf("%s: stream's first frame on the wire is not HEADERS: %s", label, w.summary())
	}
	if !bytes.Equal(w.data, body) {
		t.Errorf("%s: %d DATA bytes on the wire, want the body's %d, in order (frames %s)", label, len(w.data), len(body), w.summary())
	}
	if !w.ended || w.after != 0 {
		t.Errorf("%s: END_STREAM %v, %d frames after it, want true and 0 (frames %s)", label, w.ended, w.after, w.summary())
	}
}

// TestFlushedDataFollowsTheQueuedHeaders903: a pool handler's WriteResponse
// queues HEADERS and the DATA the windows allow, and buffers the rest on the
// stream. A WINDOW_UPDATE (or SETTINGS_INITIAL_WINDOW_SIZE) processed before
// the loop drains the queue flushed that tail straight to the connection,
// ahead of the stream's HEADERS: DATA on a stream with no HEADERS, a protocol
// error to the peer. Each of the three flush paths.
func TestFlushedDataFollowsTheQueuedHeaders903(t *testing.T) {
	body := pattern904(70000)
	for _, path := range []string{"connection WINDOW_UPDATE", "stream WINDOW_UPDATE", "SETTINGS_INITIAL_WINDOW_SIZE"} {
		t.Run(path, func(t *testing.T) {
			wrote := make(chan struct{})
			h := &asyncHandler893{run: func(_ context.Context, s *stream.Stream) error {
				err := s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "application/octet-stream"}}, body)
				close(wrote)
				return err
			}}
			c := newFlushConn(t, h, H2Config{})
			switch path {
			case "connection WINDOW_UPDATE":
				// The stream window is 1 MiB; the connection's 65,535 lets
				// 65,535 go with the HEADERS and buffers 4,465.
				c.open(0, []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1 << 20}}, 1)
			default:
				// The connection window is wide; the stream's 65,535 lets
				// 65,535 go with the HEADERS and buffers 4,465.
				c.open(1<<20, nil, 1)
			}
			waitFlush(t, "the handler's write", func() bool { return chanClosed(wrote) })
			// The queue still holds stream 1's HEADERS and first DATA. The
			// window now arrives, before the loop drains it.
			switch path {
			case "connection WINDOW_UPDATE":
				c.grant(100000, true)
			case "stream WINDOW_UPDATE":
				c.grant(100000, false, 1)
			default:
				c.setInitialWindow(65535 + 100000)
			}
			c.drain()
			checkWholeBody(t, path, c.stream(1), body)
			if held := c.st.processor.GetManager().OutboundHeld(); held != 0 {
				t.Errorf("%s: the connection still holds %d buffered bytes after the stream ended", path, held)
			}
		})
	}
}

// TestWindowUpdateBetweenStagingAndTheQueue903: the same, with the
// WINDOW_UPDATE forced into the one place a handler is exposed to it,
// between the windows being reserved for its response and its frames being
// queued (h2BeforeEnqueueHook). Whatever the handler does about the rest of its
// body by then, the HEADERS must reach the wire first, and the credit the
// WINDOW_UPDATE brought must not be lost: the whole body has to arrive with
// no further WINDOW_UPDATE, as the peer sends none.
func TestWindowUpdateBetweenStagingAndTheQueue903(t *testing.T) {
	body := pattern904(70000)
	for _, route := range []string{"async", "sync"} {
		t.Run(route, func(t *testing.T) {
			done := make(chan struct{})
			run := func(_ context.Context, s *stream.Stream) error {
				defer close(done)
				return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "application/octet-stream"}}, body)
			}
			var h stream.Handler = &asyncHandler893{run: run}
			if route == "sync" {
				h = &syncHandler893{run: run}
			}
			c := newFlushConn(t, h, H2Config{})
			var fired atomic.Bool
			prev := h2BeforeEnqueueHook
			h2BeforeEnqueueHook = func(a *h2ResponseAdapter, _ *stream.Stream) {
				if a == &c.st.adapter && fired.CompareAndSwap(false, true) {
					// A recv whose first frames credit both windows,
					// processed while this handler is between its window
					// reservation and its Enqueue.
					c.grant(100000, true, 1)
				}
			}
			t.Cleanup(func() { h2BeforeEnqueueHook = prev })
			if route == "sync" {
				// Over the budget, so the sync GET runs on the pool.
				holder := c.st.processor.GetManager().CreateStream(101)
				holder.SetState(stream.StateOpen)
				holder.SetWindowSize(0)
				holder.BufferOutbound(make([]byte, stream.OutboundBudget), true)
			}
			c.open(0, nil, 1)
			waitFlush(t, "the handler", func() bool { return chanClosed(done) })
			if !fired.Load() {
				t.Fatal("the hook never fired: the handler's response did not reach WriteResponse's queue step")
			}
			c.drain()
			checkWholeBody(t, route, c.stream(1), body)
		})
	}
}

// writeStream904 is the handler of the StreamWriter tests: WriteHeader, then
// each chunk in turn, then Close.
func writeStream904(chunks ...[]byte) func(context.Context, *stream.Stream) error {
	return func(_ context.Context, s *stream.Stream) error {
		sw := s.ResponseWriter.(stream.Streamer)
		if err := sw.WriteHeader(s, 200, [][2]string{{"content-type", "text/plain"}}); err != nil {
			return err
		}
		for _, ch := range chunks {
			if err := sw.Write(s, ch); err != nil {
				return err
			}
		}
		return sw.Close(s)
	}
}

// TestStreamWriterHonoursTheStreamWindow904: a StreamWriter response's DATA
// went to the wire without reserving either window (50,000 bytes past a
// 1,000-byte stream window). It must send what the window allows, wait for
// the rest on its own goroutine, and finish, in order, once the peer grants
// window.
func TestStreamWriterHonoursTheStreamWindow904(t *testing.T) {
	body := pattern904(50000)
	done := make(chan struct{})
	h := &asyncHandler893{run: func(ctx context.Context, s *stream.Stream) error {
		defer close(done)
		return writeStream904(body)(ctx, s)
	}}
	c := newFlushConn(t, h, H2Config{})
	c.open(0, []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1000}}, 1)
	waitFlush(t, "the first 1000 bytes", func() bool { c.drain(); return len(c.stream(1).data) >= 1000 })
	time.Sleep(100 * time.Millisecond) // anything past the window would be queued by now
	c.drain()
	if w := c.stream(1); len(w.data) != 1000 || w.ended || chanClosed(done) {
		t.Fatalf("with a 1000-byte stream window and no WINDOW_UPDATE: %d DATA bytes sent (want 1000), END_STREAM %v, handler finished %v (frames %s)",
			len(w.data), w.ended, chanClosed(done), w.summary())
	}
	// Window arrives in pieces; the handler waits on its goroutine, so the
	// event loop (this test) is never blocked.
	for i := 0; i < 50 && !chanClosed(done); i++ {
		c.grant(1000, true, 1)
		time.Sleep(2 * time.Millisecond)
		c.drain()
		if sent := len(c.stream(1).data); sent > 1000+(i+1)*1000 {
			t.Fatalf("after %d grants of 1000: %d DATA bytes sent, more than the %d granted", i+1, sent, 1000+(i+1)*1000)
		}
	}
	waitFlush(t, "the handler to finish", func() bool { c.grant(10000, true, 1); c.drain(); return chanClosed(done) })
	c.drain()
	checkWholeBody(t, "stream window", c.stream(1), body)
}

// TestStreamWritersShareTheConnectionWindow904: two StreamWriter responses
// whose stream windows are wide (1 MiB) share a connection window of 65,535:
// together they must send no more than that until the peer credits the
// connection, then finish.
func TestStreamWritersShareTheConnectionWindow904(t *testing.T) {
	body := pattern904(50000)
	var wg sync.WaitGroup
	wg.Add(2)
	h := &asyncHandler893{run: func(ctx context.Context, s *stream.Stream) error {
		defer wg.Done()
		return writeStream904(body)(ctx, s)
	}}
	c := newFlushConn(t, h, H2Config{})
	c.open(0, []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1 << 20}}, 1, 3)
	waitFlush(t, "the connection window to be used up", func() bool {
		c.drain()
		return len(c.stream(1).data)+len(c.stream(3).data) >= 65535
	})
	time.Sleep(100 * time.Millisecond)
	c.drain()
	total := len(c.stream(1).data) + len(c.stream(3).data)
	if total != 65535 {
		t.Fatalf("two StreamWriter responses on a 65535-byte connection window sent %d DATA bytes before any WINDOW_UPDATE, want 65535", total)
	}
	fin := make(chan struct{})
	go func() { wg.Wait(); close(fin) }()
	waitFlush(t, "both handlers to finish", func() bool { c.grant(20000, true); c.drain(); return chanClosed(fin) })
	c.drain()
	checkWholeBody(t, "stream 1", c.stream(1), body)
	checkWholeBody(t, "stream 3", c.stream(3), body)
}

// TestSettingsReflushReservesTheConnectionWindow904: the
// SETTINGS_INITIAL_WINDOW_SIZE re-flush sent a stream's buffered DATA up to its
// new stream window without reserving the connection window (165,535 bytes on
// a 65,535-byte connection window). It must send nothing the connection window
// does not allow, and the rest as the peer credits it.
func TestSettingsReflushReservesTheConnectionWindow904(t *testing.T) {
	body := pattern904(200000)
	wrote := make(chan struct{})
	h := &asyncHandler893{run: func(_ context.Context, s *stream.Stream) error {
		err := s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "application/octet-stream"}}, body)
		close(wrote)
		return err
	}}
	c := newFlushConn(t, h, H2Config{})
	// Both windows 65,535: the handler sends 65,535 and buffers the rest.
	c.open(0, nil, 1)
	waitFlush(t, "the handler's write", func() bool { return chanClosed(wrote) })
	c.drain()
	if n := len(c.stream(1).data); n != 65535 {
		t.Fatalf("before the SETTINGS: %d DATA bytes on the wire, want 65535", n)
	}
	// The stream window grows by 100,000; the connection window gets nothing.
	c.setInitialWindow(65535 + 100000)
	c.drain()
	if n := len(c.stream(1).data); n != 65535 {
		t.Errorf("after SETTINGS_INITIAL_WINDOW_SIZE +100000 and no connection WINDOW_UPDATE: %d DATA bytes in all, want 65535 (the connection window is used up)", n)
	}
	// The connection window is credited; the rest goes out, in order.
	c.grant(200000, true)
	c.drain()
	checkWholeBody(t, "after the connection window was credited", c.stream(1), body)
	if held := c.st.processor.GetManager().OutboundHeld(); held != 0 {
		t.Errorf("the connection still holds %d buffered bytes after the stream ended", held)
	}
}

// TestStreamWriterWaitIsBoundedByWriteTimeout904: the wait a StreamWriter's
// Write now makes for the peer's window takes #906's WriteTimeout deadline
// (AwaitSendWindow, a timer, so it holds while the event loop is blocked):
// Write returns an error wrapping os.ErrDeadlineExceeded after no more than
// about WriteTimeout, and the stream is reset with INTERNAL_ERROR behind
// what it sent. The peer sends PINGs, which keep the connection's read and
// idle timeouts from firing, and no window. With no WriteTimeout there is no
// bound but the peer and the connection's close.
func TestStreamWriterWaitIsBoundedByWriteTimeout904(t *testing.T) {
	const writeTimeout = 300 * time.Millisecond
	run := func(timeout time.Duration) (c *flushConn, took *atomic.Int64, werr *atomic.Value, done chan struct{}, ctxDone *atomic.Bool) {
		took, werr, done, ctxDone = new(atomic.Int64), new(atomic.Value), make(chan struct{}), new(atomic.Bool)
		h := &asyncHandler893{run: func(_ context.Context, s *stream.Stream) error {
			defer close(done)
			sw := s.ResponseWriter.(stream.Streamer)
			_ = sw.WriteHeader(s, 200, nil)
			start := time.Now()
			err := sw.Write(s, make([]byte, 50000))
			took.Store(int64(time.Since(start)))
			ctxDone.Store(s.Context().Err() != nil)
			if err != nil {
				werr.Store(err)
			}
			return err
		}}
		c = newFlushConn(t, h, H2Config{WriteTimeout: timeout})
		c.open(0, []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1000}}, 1)
		return
	}

	t.Run("bounded", func(t *testing.T) {
		c, took, werr, done, ctxDone := run(writeTimeout)
		for until := time.Now().Add(5 * time.Second); !chanClosed(done) && time.Now().Before(until); {
			time.Sleep(20 * time.Millisecond)
			c.drain()
		}
		if !chanClosed(done) {
			c.st.processor.GetManager().Close()
			<-done
			t.Fatalf("the Write still waited for window 5 s after it began, WriteTimeout %v", writeTimeout)
		}
		c.drain()
		err, _ := werr.Load().(error)
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("Write returned %v, want an error wrapping os.ErrDeadlineExceeded", err)
		}
		if d := time.Duration(took.Load()); d < writeTimeout-20*time.Millisecond || d > writeTimeout+2*time.Second {
			t.Errorf("Write returned after %v, want about WriteTimeout %v", d, writeTimeout)
		}
		if !ctxDone.Load() {
			t.Error("the handler's context is not done once its stream is reset")
		}
		w := c.stream(1)
		if len(w.rst) != 1 || w.rst[0] != http2.ErrCodeInternal || w.seq[len(w.seq)-1] != "RST_STREAM" || len(w.data) != 1000 || w.ended {
			t.Errorf("stream 1 on the wire: %s, %d DATA bytes, RST_STREAM codes %v; want HEADERS, DATA(1000) (the window), then one RST_STREAM INTERNAL_ERROR last, no END_STREAM", w.summary(), len(w.data), w.rst)
		}
	})
	t.Run("no-write-timeout", func(t *testing.T) {
		c, _, _, done, _ := run(0)
		for until := time.Now().Add(4 * writeTimeout); time.Now().Before(until); {
			time.Sleep(20 * time.Millisecond)
			c.drain()
		}
		if chanClosed(done) {
			t.Fatal("with no WriteTimeout the Write returned while the peer granted no window")
		}
		c.st.processor.GetManager().Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the Write still waited after its connection closed")
		}
		c.drain()
		if w := c.stream(1); len(w.rst) != 0 {
			t.Errorf("stream 1 was reset (%v) with no WriteTimeout", w.rst)
		}
	})
}

// TestInlineStreamWriterBuffersForTheWindow904: a sync handler that streams
// from the event loop cannot wait for window: it must return promptly, with
// what the window does not allow buffered behind the DATA already sent, and
// its Close must not put END_STREAM ahead of that. The rest, and the
// END_STREAM, go out as the peer grants window, in order.
func TestInlineStreamWriterBuffersForTheWindow904(t *testing.T) {
	var parts [][]byte
	var all []byte
	for i := 0; i < 5; i++ {
		p := pattern904(10000)
		for j := range p {
			p[j] = byte('A' + (i+j)%26)
		}
		parts = append(parts, p)
		all = append(all, p...)
	}
	var onLoop atomic.Bool
	h := &syncHandler893{run: func(ctx context.Context, s *stream.Stream) error {
		onLoop.Store(true)
		_, isInline := s.ResponseWriter.(*h2InlineResponseAdapter)
		if !isInline {
			return errors.New("the sync handler did not run inline")
		}
		return writeStream904(parts...)(ctx, s)
	}}
	c := newFlushConn(t, h, H2Config{})
	start := time.Now()
	c.open(0, []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1000}}, 1)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("ProcessH2 took %v: the inline StreamWriter waited for window on the event loop", d)
	}
	if !onLoop.Load() {
		t.Fatal("the handler did not run")
	}
	c.drain()
	if w := c.stream(1); len(w.data) != 1000 || w.ended {
		t.Fatalf("with a 1000-byte window: %d DATA bytes sent (want 1000), END_STREAM %v (frames %s)", len(w.data), w.ended, w.summary())
	}
	if !c.st.processor.OutboundPending() {
		t.Fatal("the rest of the response is not buffered on its stream")
	}
	for i := 0; i < 100 && !c.stream(1).ended; i++ {
		c.grant(1500, true, 1)
		c.drain()
		if sent := len(c.stream(1).data); sent > 1000+(i+1)*1500 {
			t.Fatalf("after %d grants of 1500: %d DATA bytes sent, more than the %d granted", i+1, sent, 1000+(i+1)*1500)
		}
	}
	checkWholeBody(t, "inline stream", c.stream(1), all)
	if held := c.st.processor.GetManager().OutboundHeld(); held != 0 {
		t.Errorf("the connection still holds %d buffered bytes after the stream ended", held)
	}
}
