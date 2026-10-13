//go:build linux

package celeris_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris"
)

// celeris#889 (items 12, 32 and 47) on the native engines, end to end over
// h2c: a handler that returns without finishing its response used to leave the
// client waiting for its own timeout. Go's net/http ends the response when a
// handler returns, and so does the processor now:
//   - a complete response that was not closed (no declared length, or the
//     declared length written) ends with END_STREAM;
//   - a body short of its declared content-length, a handler that sent
//     nothing and called runtime.Goexit, ends with RST_STREAM(INTERNAL_ERROR),
//     and the Goexit is logged with its stack.
// Each runs on a sync route (the event loop) and on an async one (the pool).

// h2ReturnArms889 are the engines of these tests: the std engine is the model
// (net/http ends a response when its handler returns, and x/net's server resets
// a stream whose handler panicked or called runtime.Goexit with INTERNAL_ERROR),
// the native engines (epoll, io_uring, and adaptive started on each) are what
// the processor now does the same on.
func h2ReturnArms889() []arm848 {
	return append([]arm848{{"std", celeris.Std, nil, ""}}, arms848()...)
}

type h2Return889Rig struct {
	s      *celeris.Server
	addr   string
	logs   *capture848
	cancel context.CancelFunc
	done   chan error
	goexit atomic.Int64
}

func (r *h2Return889Rig) routes(s *celeris.Server) {
	streamed := func(contentLength, n int) celeris.HandlerFunc {
		return func(c *celeris.Context) error {
			sw := c.StreamWriter()
			var hdrs [][2]string
			if contentLength >= 0 {
				hdrs = [][2]string{{"content-length", fmt.Sprint(contentLength)}}
			}
			if err := sw.WriteHeader(200, hdrs); err != nil {
				return err
			}
			if n > 0 {
				if _, err := sw.Write([]byte(strings.Repeat("x", n))); err != nil {
					return err
				}
			}
			return nil // never closed
		}
	}
	s.GET("/noclose", streamed(-1, 5))
	s.GET("/exact", streamed(5, 5))
	s.GET("/short", streamed(100, 40))
	s.GET("/anoclose", streamed(-1, 5)).Async()
	s.GET("/aexact", streamed(5, 5)).Async()
	s.GET("/ashort", streamed(100, 40)).Async()
	s.GET("/aboom", func(c *celeris.Context) error {
		r.goexit.Add(1)
		runtime.Goexit()
		return nil
	}).Async()
	// A response broken off after its headers: the router recovers the panic
	// and cannot write its 500, and answers an error with nothing; the handler
	// returns as if it had finished, with no content-length declared.
	broken := func(fail func() error) celeris.HandlerFunc {
		return func(c *celeris.Context) error {
			sw := c.StreamWriter()
			if err := sw.WriteHeader(200, nil); err != nil {
				return err
			}
			if _, err := sw.Write([]byte("xxxxx")); err != nil {
				return err
			}
			return fail()
		}
	}
	panics := func() error { panic("boom889") }
	errs := func() error { return errors.New("boom889") }
	s.GET("/panicstream", broken(panics))
	s.GET("/errstream", broken(errs))
	s.GET("/apanicstream", broken(panics)).Async()
	s.GET("/aerrstream", broken(errs)).Async()
	// The inline handler's tail waits for the peer's window (the client below
	// grants 1000 bytes): whole, or short of its declared length.
	s.GET("/bufwhole", streamed(-1, 5000))
	s.GET("/bufshort", streamed(6000, 5000))
	s.GET("/ok", func(c *celeris.Context) error { return c.String(200, "ok") })
}

func startH2Return889(t *testing.T, a arm848) *h2Return889Rig {
	t.Helper()
	eng := a.eng
	if a.start != "" {
		t.Setenv("CELERIS_ADAPTIVE_START", a.start)
	}
	r := &h2Return889Rig{logs: &capture848{}}
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.s = celeris.New(celeris.Config{Engine: eng, Protocol: celeris.H2C, Workers: 2, Logger: slog.New(r.logs)})
		r.routes(r.s)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		r.done = make(chan error, 1)
		go func() { r.done <- r.s.StartWithListenerAndContext(ctx, ln) }()
		if r.addr, err = c714WaitReady(r.s, r.done); err == nil {
			break
		}
		cancel()
		_ = ln.Close()
		if !strings.Contains(err.Error(), "cannot allocate memory") || time.Now().After(deadline) {
			t.Fatalf("server did not start: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Cleanup(r.cancel)
	return r
}

// get fetches path over h2c (prior knowledge) with Go's own HTTP/2 client,
// net/http's, and reads the body.
func (r *h2Return889Rig) get(path string) (body string, err error) {
	tr := &http.Transport{Protocols: new(http.Protocols)}
	tr.Protocols.SetUnencryptedHTTP2(true)
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+r.addr+path, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// isInternalError889 reports whether err is the client's view of a
// RST_STREAM(INTERNAL_ERROR): "stream error: stream ID 1; INTERNAL_ERROR".
func isInternalError889(err error) bool {
	return err != nil && strings.Contains(err.Error(), "INTERNAL_ERROR")
}

func TestH2HandlerReturnWithoutClosingEndsTheStream889(t *testing.T) {
	for _, a := range h2ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH2Return889(t, a)
			for _, path := range []string{"/noclose", "/exact", "/anoclose", "/aexact"} {
				body, err := r.get(path)
				if err != nil || body != "xxxxx" {
					t.Errorf("GET %s: body %q err %v, want the 5 bytes and a clean end of the stream (END_STREAM)", path, body, err)
				}
			}
			// the same connection of a fresh client still works
			if body, err := r.get("/ok"); err != nil || body != "ok" {
				t.Errorf("GET /ok: %q %v", body, err)
			}
		})
	}
}

// The std engine is not an arm here: net/http's server ends the stream with
// END_STREAM after a body short of its declared length and leaves the client
// to notice (x/net's server checks only for more bytes than declared).
func TestH2HandlerShortOfItsContentLengthResetsTheStream889(t *testing.T) {
	for _, a := range arms848() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH2Return889(t, a)
			for _, path := range []string{"/short", "/ashort"} {
				body, err := r.get(path)
				if !isInternalError889(err) {
					t.Errorf("GET %s: body %q err %v, want a stream error INTERNAL_ERROR (RST_STREAM): END_STREAM would make a truncated body look complete, and no frame leaves the client to its timeout", path, body, err)
				}
			}
		})
	}
}

func TestH2AsyncHandlerGoexitResetsTheStreamAndIsLogged889(t *testing.T) {
	for _, a := range h2ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH2Return889(t, a)
			for range 3 {
				_, err := r.get("/aboom")
				if !isInternalError889(err) {
					t.Errorf("GET /aboom: err %v, want a stream error INTERNAL_ERROR (RST_STREAM)", err)
				}
			}
			if got := r.goexit.Load(); got != 3 {
				t.Fatalf("INJECTION: /aboom ran %d times, want 3", got)
			}
			if a.eng == celeris.Std {
				return // net/http's own handling is the reference; its records are not ours
			}
			var recs int
			for dl := time.Now().Add(3 * time.Second); time.Now().Before(dl); time.Sleep(10 * time.Millisecond) {
				if recs = errorRecords848(r.logs); recs >= 3 {
					break
				}
			}
			if recs != 3 {
				t.Errorf("%d error records after 3 Goexits, want 3 (one each, with the stack)", recs)
			}
			r.logs.mu.Lock()
			for _, rec := range r.logs.recs {
				if rec.level >= slog.LevelError {
					if !strings.Contains(rec.msg, "Goexit") || !strings.Contains(attrStr848(rec.attrs["stack"]), "h2Return889Rig") {
						t.Errorf("record %q lacks the Goexit message or a stack through the handler: %v", rec.msg, rec.attrs)
					}
				}
			}
			r.logs.mu.Unlock()
			// the loop and the connection survive
			if body, err := r.get("/ok"); err != nil || body != "ok" {
				t.Errorf("GET /ok after the Goexits: %q %v", body, err)
			}
			if exits := len(r.logs.exits()); exits != 0 {
				t.Errorf("%d loop-exit records, want 0 (an async handler's Goexit does not end the loop)", exits)
			}
		})
	}
}

// A StreamWriter response that its handler broke off, by a panic the router
// recovers or by returning an error, must not be ended as if it were whole: the
// router cannot write its 500 over a response that is under way, so HandleStream
// returns nil, and an unclosed response with no declared length would be ended
// with END_STREAM, a truncated body that looks complete. It is reset. (The std
// engine is not an arm: the router recovers the panic before x/net's server
// sees it, so net/http ends that stream; a known difference.)
func TestH2StreamedResponseBrokenOffIsReset889(t *testing.T) {
	for _, a := range arms848() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH2Return889(t, a)
			for _, path := range []string{"/panicstream", "/errstream", "/apanicstream", "/aerrstream"} {
				body, err := r.get(path)
				if !isInternalError889(err) {
					t.Errorf("GET %s: body %q err %v, want a stream error INTERNAL_ERROR: a response broken off after its headers must not end with END_STREAM", path, body, err)
				}
			}
			if body, err := r.get("/ok"); err != nil || body != "ok" {
				t.Errorf("GET /ok: %q %v", body, err)
			}
		})
	}
}

// An inline handler whose StreamWriter response is not closed while its tail
// is still buffered for the peer's window: a whole response keeps its tail and
// ends with END_STREAM on the last DATA once the peer grants window; one short
// of its declared length is reset at once, its buffer dropped, and nothing
// follows the RST_STREAM. The raw client holds a 1000-byte stream window.
func TestH2UnclosedInlineWriterWithBufferedTail889(t *testing.T) {
	for _, a := range arms848() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH2Return889(t, a)
			c := dialWin904(t, r.addr, 1000, false)
			c.get(t, 1, "/bufwhole")
			c.get(t, 3, "/bufshort")
			// stream 3 is reset without a grant; stream 1 waits for window
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				c.mu.Lock()
				_, rst := c.resets[3]
				c.mu.Unlock()
				if rst {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			c.mu.Lock()
			code, rst := c.resets[3]
			ended3 := c.ended[3]
			ended1 := c.ended[1]
			c.mu.Unlock()
			if !rst || code != http2.ErrCodeInternal || ended3 {
				t.Errorf("stream 3 (6000 declared, 5000 written, unclosed): reset=%v code=%v END_STREAM=%v, want an INTERNAL_ERROR reset and no END_STREAM", rst, code, ended3)
			}
			if ended1 {
				t.Errorf("stream 1 ended before its buffered tail was sent")
			}
			c.grant(t, 1, 1<<20)
			c.grant(t, 3, 1<<20)
			c.check(t, 1, []byte(strings.Repeat("x", 5000)))
			c.settled(3, 200*time.Millisecond)
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.ended[3] || len(c.data[3]) > 1000 {
				t.Errorf("stream 3 after the grant: END_STREAM %v, %d body bytes (at most the first 1000 were sent before the reset)", c.ended[3], len(c.data[3]))
			}
			if _, rst1 := c.resets[1]; rst1 {
				t.Errorf("stream 1, a whole response, was reset")
			}
		})
	}
}
