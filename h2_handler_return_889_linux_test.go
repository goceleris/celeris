//go:build linux

package celeris_test

import (
	"context"
	"crypto/tls"
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
// the native engines are what the processor now does the same on.
func h2ReturnArms889() []arm848 {
	return append([]arm848{{"std", celeris.Std, nil, ""}}, arms848()[:2]...)
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
	s.GET("/ok", func(c *celeris.Context) error { return c.String(200, "ok") })
}

func startH2Return889(t *testing.T, eng celeris.EngineType) *h2Return889Rig {
	t.Helper()
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

// get fetches path over h2c with Go's own HTTP/2 client and reads the body.
func (r *h2Return889Rig) get(path string) (body string, err error) {
	tr := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
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

func TestH2HandlerReturnWithoutClosingEndsTheStream889(t *testing.T) {
	for _, a := range h2ReturnArms889() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH2Return889(t, a.eng)
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
	for _, a := range arms848()[:2] {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH2Return889(t, a.eng)
			for _, path := range []string{"/short", "/ashort"} {
				body, err := r.get(path)
				var se http2.StreamError
				if !errors.As(err, &se) || se.Code != http2.ErrCodeInternal {
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
			r := startH2Return889(t, a.eng)
			for range 3 {
				_, err := r.get("/aboom")
				var se http2.StreamError
				if !errors.As(err, &se) || se.Code != http2.ErrCodeInternal {
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
	for _, a := range arms848()[:2] {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH2Return889(t, a.eng)
			for _, path := range []string{"/panicstream", "/errstream", "/apanicstream", "/aerrstream"} {
				body, err := r.get(path)
				var se http2.StreamError
				if !errors.As(err, &se) || se.Code != http2.ErrCodeInternal {
					t.Errorf("GET %s: body %q err %v, want a stream error INTERNAL_ERROR: a response broken off after its headers must not end with END_STREAM", path, body, err)
				}
			}
			if body, err := r.get("/ok"); err != nil || body != "ok" {
				t.Errorf("GET /ok: %q %v", body, err)
			}
		})
	}
}
