package celeris

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/conn"
)

// A StreamWriter used after its HTTP/2 stream ended (celeris#904, review of
// celeris#965). Context.Detach documents that on HTTP/2 the stream ends when
// the handler returns, detached or not, and the stream object goes back to a
// pool shared by every connection. A goroutine the handler started and that
// writes later must be refused: its bytes must not reach the wire of the
// connection it came from under another stream's ID, and above all not the
// client of whichever connection takes the pooled object next.
//
// These tests drive the real Context.StreamWriter through the in-memory HTTP/2
// engine (conn.ProcessH2), a handler per connection, so the writer is the one
// a user gets.

type lateH2 struct {
	t    *testing.T
	st   *conn.H2State
	ra   *routerAdapter
	mu   sync.Mutex
	wire bytes.Buffer
}

func (l *lateH2) write(b []byte) { l.mu.Lock(); l.wire.Write(b); l.mu.Unlock() }

func (l *lateH2) bytesWritten() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.wire.Bytes()...)
}

func (l *lateH2) drain() { l.st.DrainWriteQueue(l.write) }

func (l *lateH2) process(in []byte) {
	l.t.Helper()
	if err := conn.ProcessH2(context.Background(), in, l.st, l.ra, l.write, conn.H2Config{}); err != nil {
		l.t.Error(err)
	}
}

func lateFrames(t *testing.T, fn func(fr *http2.Framer, enc *hpack.Encoder, hb *bytes.Buffer)) []byte {
	t.Helper()
	var out, hb bytes.Buffer
	fr := http2.NewFramer(&out, nil)
	fn(fr, hpack.NewEncoder(&hb), &hb)
	return out.Bytes()
}

// newLateH2 opens a connection to srv and sends GET path on stream 1.
func newLateH2(t *testing.T, srv *Server, path string) *lateH2 {
	t.Helper()
	l := &lateH2{t: t, ra: &routerAdapter{server: srv}}
	l.st = conn.NewH2State(l.ra, conn.H2Config{}, l.write, nil)
	l.process(append([]byte(http2.ClientPreface), lateFrames(t, func(fr *http2.Framer, enc *hpack.Encoder, hb *bytes.Buffer) {
		_ = fr.WriteSettings()
		for _, hf := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: path}} {
			_ = enc.WriteField(hf)
		}
		_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
	})...))
	l.waitPool()
	l.drain()
	return l
}

// request sends GET path on stream id of an open connection.
func (l *lateH2) request(id uint32, path string) {
	l.t.Helper()
	l.process(lateFrames(l.t, func(fr *http2.Framer, enc *hpack.Encoder, hb *bytes.Buffer) {
		for _, hf := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: path}} {
			_ = enc.WriteField(hf)
		}
		_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
	}))
	l.waitPool()
	l.drain()
}

// waitPool waits for the connection's pool handlers (an async route) to have
// returned and released their streams. A pool stream goes back to the stream
// pool when the event loop ends its next frame batch (celeris#951), so one
// PING is that batch here: without it the stream stays retired (cancelled, its
// request dropped, not reset), where a late call is refused by the cancelled
// flag whatever its use token says, and the next request cannot get the
// object, as it can on a live connection.
func (l *lateH2) waitPool() {
	l.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for l.st.PoolHandlersRunning() {
		if time.Now().After(deadline) {
			l.t.Fatal("pool handler did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	l.process(lateFrames(l.t, func(fr *http2.Framer, _ *hpack.Encoder, _ *bytes.Buffer) {
		_ = fr.WritePing(false, [8]byte{})
	}))
}

// grant sends WINDOW_UPDATEs for stream 1 and the connection.
func (l *lateH2) grant(n uint32) {
	l.t.Helper()
	l.process(lateFrames(l.t, func(fr *http2.Framer, _ *hpack.Encoder, _ *bytes.Buffer) {
		_ = fr.WriteWindowUpdate(1, n)
		_ = fr.WriteWindowUpdate(0, n)
	}))
	l.drain()
}

type lateFrame struct {
	typ    http2.FrameType
	stream uint32
	flags  http2.Flags
	data   []byte
}

func lateParse(t *testing.T, b []byte) []lateFrame {
	t.Helper()
	fr := http2.NewFramer(nil, bytes.NewReader(b))
	var out []lateFrame
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Errorf("parsing the wire: %v", err)
			}
			return out
		}
		lf := lateFrame{typ: f.Header().Type, stream: f.Header().StreamID, flags: f.Header().Flags}
		if d, ok := f.(*http2.DataFrame); ok {
			lf.data = append([]byte(nil), d.Data()...)
		}
		out = append(out, lf)
	}
}

// lateServers returns the two routes under test: /sync (a handler on the event
// loop) and /async (a handler on the worker pool).
func lateServer(secretSW chan<- *StreamWriter, inject func()) *Server {
	srv := New(Config{})
	h := func(c *Context) error {
		sw := c.StreamWriter()
		if sw == nil {
			return errors.New("no StreamWriter")
		}
		if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}}); err != nil {
			return err
		}
		secretSW <- sw // handed to a goroutine the handler "started"; the handler returns
		return nil
	}
	srv.GET("/sync", h)
	srv.GET("/async", h).Async()
	srv.GET("/hello", func(c *Context) error { return c.Blob(200, "text/plain", []byte("hello-b")) })
	// /inject runs the late calls while its own stream, likely the pooled
	// object conn A's writer holds, is the connection's live stream.
	srv.GET("/inject", func(c *Context) error {
		if inject != nil {
			inject()
		}
		return c.Blob(200, "text/plain", []byte("hello-3"))
	})
	return srv
}

// TestLateStreamWriterIsRefused904 is the regression test for the review's
// blocker: after the handler returned, Write, Close and WriteHeader return an
// error and put nothing on the connection, and nothing of them reaches the next
// connection that reuses the pooled stream object.
func TestLateStreamWriterIsRefused904(t *testing.T) {
	prev := runtime.GOMAXPROCS(1) // one P: streamPool's private slot hands the released stream back to the next NewStream
	defer runtime.GOMAXPROCS(prev)
	secret := bytes.Repeat([]byte("SECRET-of-conn-A;"), 64)
	for _, route := range []string{"/sync", "/async"} {
		t.Run(route[1:], func(t *testing.T) {
			const tries = 20
			leaks := 0
			for i := 0; i < tries; i++ {
				ch := make(chan *StreamWriter, 1)
				var swA *StreamWriter
				var injectErrs [3]error
				a := newLateH2(t, lateServer(ch, func() {
					_, injectErrs[0] = swA.Write(secret)
					injectErrs[1] = swA.Close()
					injectErrs[2] = swA.WriteHeader(200, nil)
				}), route)
				sw := <-ch
				swA = sw
				before := a.bytesWritten()

				// The detached goroutine's calls, after the handler returned.
				if _, err := sw.Write(secret); err == nil {
					t.Errorf("try %d: Write after the handler returned: nil error, want a refusal", i)
				}
				if err := sw.Close(); err == nil {
					t.Errorf("try %d: Close after the handler returned: nil error, want a refusal", i)
				}
				if err := sw.WriteHeader(200, nil); err == nil {
					t.Errorf("try %d: WriteHeader after the handler returned: nil error, want a refusal", i)
				}
				a.drain()
				if after := a.bytesWritten(); !bytes.Equal(after, before) {
					t.Errorf("try %d: the late calls put %d bytes on conn A's wire, want none: %v", i, len(after)-len(before), lateParse(t, after[len(before):]))
				}

				// Connection B first, whose stream is likely the very object conn A's
				// writer holds (conn A's own next stream would otherwise take it).
				b := newLateH2(t, lateServer(make(chan *StreamWriter, 1), nil), "/hello")
				b.grant(1 << 20)
				wire := b.bytesWritten()
				if bytes.Contains(wire, []byte("SECRET-of-conn-A")) {
					leaks++
				}
				var body []byte
				ended := false
				for _, f := range lateParse(t, wire) {
					if f.typ == http2.FrameData && f.stream == 1 {
						body = append(body, f.data...)
						ended = ended || f.flags.Has(http2.FlagDataEndStream)
					}
				}
				if string(body) != "hello-b" || !ended {
					t.Errorf("try %d: conn B's stream 1 body = %q (ended=%v), want %q ended", i, body, ended, "hello-b")
				}

				// Conn A's next request, stream 3, takes the pooled object conn A's
				// writer holds (same connection, same manager): the late calls, made
				// from inside its handler, must not touch it.
				a.request(3, "/inject")
				for j, err := range injectErrs {
					if err == nil {
						t.Errorf("try %d: late call %d made while stream 3 was live returned nil, want a refusal", i, j)
					}
				}
				var a3 []byte
				ended3 := false
				for _, f := range lateParse(t, a.bytesWritten()[len(before):]) {
					if f.stream == 3 && f.typ == http2.FrameData {
						a3 = append(a3, f.data...)
						ended3 = ended3 || f.flags.Has(http2.FlagDataEndStream)
					}
				}
				if string(a3) != "hello-3" || !ended3 {
					t.Errorf("try %d: conn A's stream 3 body = %q (ended=%v), want %q ended: the late calls reached the next stream of the same connection", i, a3, ended3, "hello-3")
				}
			}
			if leaks > 0 {
				t.Errorf("conn A's late StreamWriter bytes reached conn B's client in %d of %d tries", leaks, tries)
			}
		})
	}
}

// TestLateStreamWriterRaceWithRelease904: a goroutine that writes in a loop
// while the handler returns and its stream is released must see its writes
// refused once the stream is over, and the run must be race-free (the release
// resets the stream under the lock the writer takes).
func TestLateStreamWriterRaceWithRelease904(t *testing.T) {
	for _, route := range []string{"/sync", "/async"} {
		t.Run(route[1:], func(t *testing.T) {
			for i := 0; i < 20; i++ {
				ch := make(chan *StreamWriter, 1)
				srv := New(Config{})
				started := make(chan struct{})
				result := make(chan error, 1)
				h := func(c *Context) error {
					sw := c.StreamWriter()
					if err := sw.WriteHeader(200, nil); err != nil {
						return err
					}
					go func() {
						close(started)
						chunk := []byte("0123456789abcdef")
						deadline := time.Now().Add(5 * time.Second)
						for time.Now().Before(deadline) {
							if _, err := sw.Write(chunk); err != nil {
								result <- err
								return
							}
						}
						result <- nil
					}()
					<-started
					ch <- sw
					return nil
				}
				srv.GET("/sync", h)
				srv.GET("/async", h).Async()
				a := newLateH2(t, srv, route)
				select {
				case err := <-result:
					if err == nil {
						t.Fatalf("try %d: the writer was never refused after its stream ended", i)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("try %d: the writer neither finished nor was refused", i)
				}
				a.drain()
				<-ch
			}
		})
	}
}
