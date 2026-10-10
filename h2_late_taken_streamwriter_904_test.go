package celeris

import (
	"bytes"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// A detached HTTP/2 Context used after its handler returned (celeris#904,
// round 2 of the review of celeris#965). The earlier tests cover a
// StreamWriter taken before the handler returned. These cover the calls that
// take the stream at call time: c.StreamWriter(), c.NoContent and the request
// getters of a Context that a goroutine of the handler holds. The pooled
// stream the Context points at may by then serve another connection's request,
// or another request of the same connection, and none of these calls may reach
// it: not its response (bytes, HEADERS), not its request (headers).

// lateSecretHeader is the request header conn B sends; a late c.Header on conn
// A's saved Context must not return it.
const lateSecretHeader = "x-b-secret"
const lateSecretValue = "conn-B-bearer-token"

// newLateH2Hdr is newLateH2 with extra request headers on stream 1.
func newLateH2Hdr(t *testing.T, srv *Server, path string, extra ...hpack.HeaderField) *lateH2 {
	t.Helper()
	l := &lateH2{t: t, ra: &routerAdapter{server: srv}}
	l.st = conn.NewH2State(l.ra, conn.H2Config{}, l.write, nil)
	l.process(append([]byte(http2.ClientPreface), lateFrames(t, func(fr *http2.Framer, enc *hpack.Encoder, hb *bytes.Buffer) {
		_ = fr.WriteSettings()
		hfs := []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: path}}
		for _, hf := range append(hfs, extra...) {
			_ = enc.WriteField(hf)
		}
		_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
	})...))
	l.waitPool()
	l.drain()
	return l
}

// lateTake is what a goroutine of conn A's handler does when it takes its
// writer after the handler returned, then sends, ends and answers.
type lateTake struct {
	nilWriter bool
	errs      []error // NoContent, Blob, JSON, Redirect, Hijack, then the writer's WriteHeader, Write, Flush, Close: all must be refused
	hdr       string  // Header(lateSecretHeader): must be empty
	reqHdrs   int     // len(RequestHeaders()): must be 0
	wsHooks   bool    // UpgradeWebSocket or WSRawWriteFn reached an engine hook: must be false
}

var lateSecret = bytes.Repeat([]byte("SECRET-of-conn-A;"), 64)

func (lt *lateTake) run(saved *Context) {
	// The response methods first: taking a writer marks the Context written,
	// which would answer NoContent with ErrResponseWritten before it reached
	// the stream.
	lt.errs = append(lt.errs, saved.NoContent(204), saved.Blob(200, "text/plain", lateSecret), saved.JSON(200, map[string]string{"s": string(lateSecret)}), saved.Redirect(302, "/"+string(lateSecret)))
	_, herr := saved.Hijack()
	lt.errs = append(lt.errs, herr)
	sw := saved.StreamWriter()
	if sw == nil {
		lt.nilWriter = true
	} else {
		_, werr := sw.Write(lateSecret)
		lt.errs = append(lt.errs, sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}}), werr, sw.Flush(), sw.Close())
	}
	lt.hdr = saved.Header(lateSecretHeader)
	lt.reqHdrs = len(saved.RequestHeaders())
	lt.wsHooks = saved.UpgradeWebSocket(func([]byte) {}) || saved.WSRawWriteFn() != nil
}

// lateDetached returns a server whose three routes detach and hand the Context
// out (sync = on the event loop, async = on the worker pool).
func lateDetachedServer(saved *atomic.Pointer[Context]) *Server {
	srv := New(Config{})
	h := func(c *Context) error { _ = c.Detach(); saved.Store(c); return nil }
	srv.GET("/sync", h)
	srv.GET("/async", h).Async()
	return srv
}

// wireStream returns the DATA body of a stream, whether it carried
// END_STREAM, and how many HEADERS frames it had.
func wireStream(t *testing.T, wire []byte, id uint32) (body []byte, ended bool, headers int) {
	t.Helper()
	for _, f := range lateParse(t, wire) {
		if f.stream != id {
			continue
		}
		switch f.typ {
		case http2.FrameData:
			body = append(body, f.data...)
			ended = ended || f.flags.Has(http2.FlagDataEndStream)
		case http2.FrameHeaders:
			headers++
		}
	}
	return
}

// TestLateTakenStreamWriterOtherConn904: conn A's handler detaches and
// returns; conn B's request takes the pooled stream; A's Context then takes a
// StreamWriter and calls NoContent and Header, from conn B's handler (so B's
// stream is live and the pooled object is B's). Nothing of A reaches B's
// client, B's header is not readable through A's Context, and every write is
// refused. A is a sync and an async route, B the same: four combinations.
func TestLateTakenStreamWriterOtherConn904(t *testing.T) {
	prev := runtime.GOMAXPROCS(1) // one P: streamPool's private slot hands the released stream to the next NewStream
	defer runtime.GOMAXPROCS(prev)
	for _, combo := range [][2]string{{"/sync", "/hello"}, {"/async", "/hello"}, {"/sync", "/helloasync"}, {"/async", "/helloasync"}} {
		route, bpath := combo[0], combo[1]
		t.Run(route[1:]+"-B"+bpath[1:], func(t *testing.T) {
			const tries = 20
			var wireLeaks, bodyBad, refusedNot, hdrLeaks, nilWriters int
			for i := 0; i < tries; i++ {
				var saved atomic.Pointer[Context]
				_ = newLateH2(t, lateDetachedServer(&saved), route)
				ca := saved.Load()
				if ca == nil {
					t.Fatal("handler A did not run")
				}
				var lt lateTake
				srvB := New(Config{})
				hB := func(c *Context) error {
					lt.run(ca)
					return c.Blob(200, "text/plain", []byte("hello-b"))
				}
				srvB.GET("/hello", hB)
				srvB.GET("/helloasync", hB).Async()
				b := newLateH2Hdr(t, srvB, bpath, hpack.HeaderField{Name: lateSecretHeader, Value: lateSecretValue})
				b.grant(1 << 20)
				wire := b.bytesWritten()
				if bytes.Contains(wire, []byte("SECRET-of-conn-A")) {
					wireLeaks++
				}
				body, ended, hdrs := wireStream(t, wire, 1)
				if string(body) != "hello-b" || !ended || hdrs != 1 {
					bodyBad++
					if bodyBad == 1 {
						t.Errorf("try %d: conn B stream 1: body %q ended=%v HEADERS frames=%d, want %q ended, 1 HEADERS", i, body, ended, hdrs, "hello-b")
					}
				}
				if lt.nilWriter {
					nilWriters++
				}
				for _, err := range lt.errs {
					if err == nil {
						refusedNot++
					}
				}
				if lt.hdr != "" || lt.reqHdrs != 0 || lt.wsHooks {
					hdrLeaks++
				}
			}
			if wireLeaks+bodyBad+refusedNot+hdrLeaks+nilWriters > 0 {
				t.Errorf("tries=%d: A's late bytes on B's wire in %d; B's stream 1 damaged in %d; late calls that returned nil (want a refusal): %d; a nil StreamWriter in %d; A's Header()/RequestHeaders() read B's request, or a WebSocket hook was reachable, in %d",
					tries, wireLeaks, bodyBad, refusedNot, nilWriters, hdrLeaks)
			}
		})
	}
}

// TestLateTakenStreamWriterSameConn904: the same, but the next user of the
// pooled stream is conn A's own next request (stream 3), and the late calls
// are made from its handler.
func TestLateTakenStreamWriterSameConn904(t *testing.T) {
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)
	for _, route := range []string{"/sync", "/async"} {
		t.Run(route[1:], func(t *testing.T) {
			const tries = 20
			var wireLeaks, bodyBad, refusedNot, nilWriters, readLeaks int
			for i := 0; i < tries; i++ {
				var saved atomic.Pointer[Context]
				srv := lateDetachedServer(&saved)
				var lt lateTake
				srv.GET("/inject", func(c *Context) error {
					lt.run(saved.Load())
					return c.Blob(200, "text/plain", []byte("hello-3"))
				})
				a := newLateH2(t, srv, route)
				before := len(a.bytesWritten())
				a.request(3, "/inject")
				a.grant(1 << 20)
				wire := a.bytesWritten()[before:]
				if bytes.Contains(wire, []byte("SECRET-of-conn-A")) {
					wireLeaks++
				}
				body, ended, hdrs := wireStream(t, wire, 3)
				if string(body) != "hello-3" || !ended || hdrs != 1 {
					bodyBad++
					if bodyBad == 1 {
						t.Errorf("try %d: stream 3: body %q ended=%v HEADERS frames=%d, want %q ended, 1 HEADERS", i, body, ended, hdrs, "hello-3")
					}
				}
				if lt.nilWriter {
					nilWriters++
				}
				if lt.reqHdrs != 0 || lt.wsHooks {
					readLeaks++
				}
				for _, err := range lt.errs {
					if err == nil {
						refusedNot++
					}
				}
			}
			if wireLeaks+bodyBad+refusedNot+nilWriters+readLeaks > 0 {
				t.Errorf("tries=%d: A's late bytes on its next stream in %d; stream 3 damaged in %d; late calls that returned nil: %d; nil StreamWriter in %d; A's RequestHeaders() read stream 3's request, or a WebSocket hook was reachable, in %d", tries, wireLeaks, bodyBad, refusedNot, nilWriters, readLeaks)
			}
		})
	}
}

// TestLateTakenStreamWriterHeldStream904: the handler detaches and returns
// with DATA the peer's window has not taken; the stream is held until the peer
// grants window. The Context's late StreamWriter, taken now, must be refused
// like one taken before: it must not add bytes to what the held stream sends.
func TestLateTakenStreamWriterHeldStream904(t *testing.T) {
	var saved atomic.Pointer[Context]
	srv := New(Config{})
	srv.GET("/sync", func(c *Context) error {
		sw := c.StreamWriter()
		if err := sw.WriteHeader(200, nil); err != nil {
			return err
		}
		if _, err := sw.Write(bytes.Repeat([]byte("x"), 100000)); err != nil { // past the 65535-byte default window
			return err
		}
		_ = c.Detach()
		saved.Store(c)
		return nil
	})
	a := newLateH2(t, srv, "/sync")
	ca := saved.Load()
	if ca == nil {
		t.Fatal("handler did not run")
	}
	var lt lateTake
	lt.run(ca)
	if lt.nilWriter {
		t.Error("a nil StreamWriter")
	}
	for i, err := range lt.errs {
		if err == nil {
			t.Errorf("late call %d returned nil, want a refusal", i)
		}
	}
	a.grant(1 << 20)
	wire := a.bytesWritten()
	if bytes.Contains(wire, []byte("SECRET-of-conn-A")) {
		t.Error("the late writer's bytes were sent on the held stream")
	}
	body, _, _ := wireStream(t, wire, 1)
	if len(body) != 100000 {
		t.Errorf("the held stream sent %d body bytes, want exactly the 100000 the handler wrote", len(body))
	}
}

// TestDetachedStreamWriterTakenInHandler904: the writer a handler takes after
// Detach, while it still runs, is the live use: it sends the response. (The
// token the Context records at Detach must not refuse the handler's own
// writer.)
func TestDetachedStreamWriterTakenInHandler904(t *testing.T) {
	prev := runtime.GOMAXPROCS(1) // one P: the pooled stream warmed up below is the one the request takes
	defer runtime.GOMAXPROCS(prev)
	for _, route := range []string{"/sync", "/async"} {
		t.Run(route[1:], func(t *testing.T) {
			// A pooled stream whose use token is not zero: a Context that
			// recorded no token (zero) must not look live to this test.
			for i := 0; i < 3; i++ {
				stream.NewStream(1).Release()
			}
			srv := New(Config{})
			h := func(c *Context) error {
				done := c.Detach()
				defer done()
				sw := c.StreamWriter()
				if sw == nil {
					return errors.New("no StreamWriter")
				}
				if err := sw.WriteHeader(200, nil); err != nil {
					return err
				}
				if _, err := sw.Write([]byte("taken-after-detach")); err != nil {
					return err
				}
				return sw.Close()
			}
			srv.GET("/sync", h)
			srv.GET("/async", h).Async()
			a := newLateH2(t, srv, route)
			body, ended, hdrs := wireStream(t, a.bytesWritten(), 1)
			if string(body) != "taken-after-detach" || !ended || hdrs != 1 {
				t.Errorf("stream 1: body %q ended=%v HEADERS=%d, want %q ended, 1 HEADERS", body, ended, hdrs, "taken-after-detach")
			}
		})
	}
}

// TestStreamWriterOfReleasedContext904: a Context used after it was released
// (its stream is gone) gives a writer that refuses, not a nil-pointer panic.
func TestStreamWriterOfReleasedContext904(t *testing.T) {
	c := new(Context)
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("StreamWriter on a released Context panicked: %v", r)
		}
	}()
	sw := c.StreamWriter()
	if sw == nil {
		t.Fatal("StreamWriter on a released Context returned nil")
	}
	if _, err := sw.Write([]byte("x")); err == nil {
		t.Error("Write on the released Context's writer returned nil, want a refusal")
	}
	if err := sw.Close(); err == nil {
		t.Error("Close returned nil, want a refusal")
	}
}
