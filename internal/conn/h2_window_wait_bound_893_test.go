package conn

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

// waitBound893 is one connection of TestPoolHandlerWaitingForWindowIsResetAtWriteTimeout893:
// a pool handler for stream 1 that writes a 300,000-byte body while another
// stream holds the whole outbound budget, so the handler gets no copy and
// waits for the peer's window.
type waitBound893 struct {
	t    *testing.T
	st   *H2State
	h    stream.Handler
	mu   sync.Mutex
	wire bytes.Buffer
	body []byte

	onPool   bool
	started  time.Time
	returned time.Time
	err      error
	ctxErr   error // the handler's context, read as its write returns
	done     chan struct{}
}

func newWaitBound893(t *testing.T, route string, writeTimeout time.Duration) *waitBound893 {
	t.Helper()
	w := &waitBound893{t: t, body: make([]byte, 300000), done: make(chan struct{})}
	for i := range w.body {
		w.body[i] = byte('a' + i%26)
	}
	run := func(ctx context.Context, s *stream.Stream) error {
		defer close(w.done)
		_, onPool := s.ResponseWriter.(*h2ResponseAdapter)
		w.mu.Lock()
		w.onPool, w.started = onPool, time.Now()
		w.mu.Unlock()
		err := s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "application/octet-stream"}}, w.body)
		w.mu.Lock()
		w.returned, w.err, w.ctxErr = time.Now(), err, ctx.Err()
		w.mu.Unlock()
		return err
	}
	if route == "async" {
		w.h = &asyncHandler893{run: run}
	} else {
		w.h = &syncHandler893{run: run} // on the pool only because of the budget
	}
	w.st = NewH2State(w.h, H2Config{WriteTimeout: writeTimeout}, w.write, nil)
	mgr := w.st.processor.GetManager()
	holder := mgr.CreateStream(101)
	holder.SetState(stream.StateOpen)
	holder.SetWindowSize(0)
	holder.BufferOutbound(make([]byte, stream.OutboundBudget), true)
	w.process(append([]byte(http2.ClientPreface), frames893(t, func(fr *http2.Framer, enc *hpack.Encoder, hb *bytes.Buffer) {
		_ = fr.WriteSettings()
		hb.Reset()
		for _, hf := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/b"}} {
			_ = enc.WriteField(hf)
		}
		_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
	})...))
	return w
}

func (w *waitBound893) write(b []byte) { w.mu.Lock(); w.wire.Write(b); w.mu.Unlock() }

func (w *waitBound893) process(in []byte) {
	w.t.Helper()
	if err := ProcessH2(context.Background(), in, w.st, w.h, w.write, H2Config{}); err != nil {
		w.t.Fatal(err)
	}
}

// grant gives stream 1 and the connection n more bytes of window, as one recv.
func (w *waitBound893) grant(n uint32) {
	w.t.Helper()
	w.process(frames893(w.t, func(fr *http2.Framer, _ *hpack.Encoder, _ *bytes.Buffer) {
		_ = fr.WriteWindowUpdate(1, n)
		_ = fr.WriteWindowUpdate(0, n)
	}))
}

func (w *waitBound893) finished() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// wire1 parses what reached the wire for stream 1.
func (w *waitBound893) wire1() (frames []string, data []byte, ended bool, rst []http2.ErrCode) {
	w.mu.Lock()
	raw := append([]byte(nil), w.wire.Bytes()...)
	w.mu.Unlock()
	for len(raw) >= 9 {
		n := int(raw[0])<<16 | int(raw[1])<<8 | int(raw[2])
		typ, flags, sid := raw[3], raw[4], binary.BigEndian.Uint32(raw[5:9])&0x7fffffff
		if len(raw) < 9+n {
			break
		}
		if sid == 1 {
			switch typ {
			case 0x0:
				frames = append(frames, "DATA")
				data = append(data, raw[9:9+n]...)
				ended = ended || flags&0x1 != 0
			case 0x1:
				frames = append(frames, "HEADERS")
				ended = ended || flags&0x1 != 0
			case 0x3:
				frames = append(frames, "RST_STREAM")
				if n == 4 {
					rst = append(rst, http2.ErrCode(binary.BigEndian.Uint32(raw[9:13])))
				}
			default:
				frames = append(frames, fmt.Sprintf("type %d", typ))
			}
		}
		raw = raw[9+n:]
	}
	return frames, data, ended, rst
}

// TestPoolHandlerWaitingForWindowIsResetAtWriteTimeout893: #906's review
// round 2. A pool handler that waits for the peer's window (its connection
// over the outbound budget) had no bound but the peer: the connection's read
// and idle timeouts never fire while the peer sends frames, and on the native
// engines an event-loop handler that waits for this one (a lock held across
// the write) wedged the whole server. It must wait no longer than the
// server's WriteTimeout from its write, then reset its stream with
// INTERNAL_ERROR, as net/http does at its write deadline, and return an error
// wrapping os.ErrDeadlineExceeded. Both kinds of pool stream: an async
// route's, and a sync GET's that is on the pool only for the budget. Both
// kinds of peer: silent, and one that trickles window (the bound is for the
// whole response, not for each wait). And with no WriteTimeout, no bound.
func TestPoolHandlerWaitingForWindowIsResetAtWriteTimeout893(t *testing.T) {
	const writeTimeout = 300 * time.Millisecond
	for _, route := range []string{"async", "sync"} {
		for _, peer := range []string{"silent", "trickle"} {
			t.Run(route+"/"+peer, func(t *testing.T) {
				w := newWaitBound893(t, route, writeTimeout)
				granted := 65535 // the default window, taken with the HEADERS
				for until := time.Now().Add(5 * time.Second); !w.finished() && time.Now().Before(until); {
					if peer == "trickle" {
						w.grant(1000)
						granted += 1000
					}
					time.Sleep(40 * time.Millisecond)
					w.st.DrainWriteQueue(w.write)
				}
				if !w.finished() {
					w.st.processor.GetManager().Close()
					<-w.done
					t.Fatalf("the handler still waited for window 5 s after its write, WriteTimeout %v", writeTimeout)
				}
				w.st.DrainWriteQueue(w.write)
				w.mu.Lock()
				onPool, ctxErr, took, err := w.onPool, w.ctxErr, w.returned.Sub(w.started), w.err
				w.mu.Unlock()
				if !onPool {
					t.Fatal("the handler ran inline, not on the pool")
				}
				if took < writeTimeout-20*time.Millisecond {
					t.Errorf("the write returned after %v, before WriteTimeout %v", took, writeTimeout)
				}
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Errorf("the write returned %v, want an error wrapping os.ErrDeadlineExceeded", err)
				}
				if ctxErr == nil {
					t.Error("the handler's context is not done once its stream is reset")
				}
				frames, data, ended, rst := w.wire1()
				if len(frames) < 3 || frames[0] != "HEADERS" || frames[len(frames)-1] != "RST_STREAM" || len(rst) != 1 || rst[0] != http2.ErrCodeInternal {
					t.Fatalf("stream 1 on the wire: %v, RST_STREAM codes %v; want HEADERS, DATA, then one RST_STREAM INTERNAL_ERROR last", frames, rst)
				}
				if ended {
					t.Errorf("stream 1 ended (END_STREAM) though %d of its %d bytes were not sent", len(w.body)-len(data), len(w.body))
				}
				if len(data) > granted || !bytes.Equal(data, w.body[:len(data)]) {
					t.Errorf("stream 1's DATA: %d bytes (window granted %d), in order %v", len(data), granted, bytes.Equal(data, w.body[:min(len(data), len(w.body))]))
				}
				t.Logf("%s/%s: the write returned after %v (WriteTimeout %v): %v; stream 1 sent %d of %d bytes, then RST_STREAM %v",
					route, peer, took.Round(time.Millisecond), writeTimeout, err, len(data), len(w.body), rst)
			})
		}
	}

	// No WriteTimeout (0): no bound but the peer, the stream's reset and the
	// connection's close, as before.
	t.Run("no-write-timeout", func(t *testing.T) {
		w := newWaitBound893(t, "async", 0)
		for until := time.Now().Add(4 * writeTimeout); time.Now().Before(until); {
			time.Sleep(20 * time.Millisecond)
			w.st.DrainWriteQueue(w.write)
		}
		if w.finished() {
			w.mu.Lock()
			err := w.err
			w.mu.Unlock()
			t.Fatalf("with no WriteTimeout the write returned (%v) while the peer granted no window", err)
		}
		w.st.processor.GetManager().Close()
		select {
		case <-w.done:
		case <-time.After(5 * time.Second):
			t.Fatal("the handler still waited after its connection closed")
		}
		w.st.DrainWriteQueue(w.write)
		if frames, _, _, rst := w.wire1(); len(rst) != 0 {
			t.Errorf("stream 1 was reset (%v, frames %v) with no WriteTimeout", rst, frames)
		}
	})
}
