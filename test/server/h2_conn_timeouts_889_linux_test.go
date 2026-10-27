//go:build linux

package celeris_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
)

// Tests for celeris#889 (L19): ReadTimeout and IdleTimeout on an HTTP/2
// connection that has a stream open.
//
// The native engines' timeout sweep measured both from the last READ on the
// connection, whatever streams the connection had: an h2c SSE stream (or any
// long response written as a slow trickle) whose client sent no frame for
// ReadTimeout (default 60 s) or IdleTimeout (default 600 s) was closed, with
// "unexpected EOF" at the client, 1.004 to 1.025 s after the request with
// ReadTimeout 1 s (probe 889.34 of PR 1131 and its review). std, the reference,
// keeps the stream: net/http's HTTP/2 server arms its idle timer only while no
// stream is open, and arms ReadTimeout per stream for the request body.
//
// The client here is a raw HTTP/2 framer, not http.Transport: after the
// request it sends nothing (a SETTINGS ack at the very start), so every
// millisecond of a cut is the server's own clock. The routes are async: an
// inline HTTP/2 handler occupies its event loop, whose sweep then does not
// run at all.

const (
	// to889 is the ReadTimeout or IdleTimeout of the server under test.
	to889 = time.Second
	// hold889 is how long a stream must stay open, three timeouts.
	hold889 = 3 * time.Second
	// reap889 is how long after the timeout a connection with no stream may
	// take to be closed: the sweeps are 25 ms (epoll) to 100 ms (io_uring)
	// apart, the rest is slack for a loaded host.
	reap889 = 3 * time.Second
	// tick889 is the interval of the held stream's chunks.
	tick889 = 200 * time.Millisecond
)

// timeoutArm889 is the timeout under test; the others are off (-1), so the arm
// is the only thing that can close the connection.
type timeoutArm889 struct {
	name string
	cfg  func(d time.Duration, c *celeris.Config)
}

var (
	armRead889 = timeoutArm889{"read", func(d time.Duration, c *celeris.Config) {
		c.ReadTimeout, c.IdleTimeout, c.WriteTimeout = d, -1, -1
	}}
	armIdle889 = timeoutArm889{"idle", func(d time.Duration, c *celeris.Config) {
		c.ReadTimeout, c.IdleTimeout, c.WriteTimeout = -1, d, -1
	}}
	// armWrite889 is a guard for the steady stream on the native engines:
	// WriteTimeout alone never cut it on main, on epoll, adaptive or io_uring.
	// Not for the quiet stream: on io_uring a send caught in flight by a sweep
	// is measured from the last read or completed send, so a stream whose
	// chunks are further apart than WriteTimeout is cut at a chunk, sooner or
	// later (a second defect, measured, in the PR's findings; it failed this
	// test once in CI). Not for std either, whose WriteTimeout is an absolute
	// deadline on a response that did not Detach (it resets the stream at 1 s,
	// RST_STREAM INTERNAL_ERROR, measured; celeris#855).
	armWrite889 = timeoutArm889{"write", func(d time.Duration, c *celeris.Config) {
		c.ReadTimeout, c.IdleTimeout, c.WriteTimeout = -1, -1, d
	}}
)

func (a timeoutArm889) config(e celeris.EngineType, d time.Duration) celeris.Config {
	c := celeris.Config{Engine: e}
	a.cfg(d, &c)
	return c
}

// h2raw889 is a raw h2c (prior knowledge) client. It sends the preface and a
// SETTINGS frame (acking the server's), and then only what the test says.
type h2raw889 struct {
	t    *testing.T
	conn net.Conn
	fr   *http2.Framer
	addr string
	t0   time.Time  // when the last frame the client sent went out
	wmu  sync.Mutex // the framer is not safe for concurrent writes (the ping goroutine, the ack)
}

type h2dial889 struct {
	initialWindow uint32 // SETTINGS_INITIAL_WINDOW_SIZE, 0 = leave the default
	prefaceOnly   bool   // the 24 bytes of the preface and no SETTINGS
}

func dialH2raw889(t *testing.T, addr string, o h2dial889) *h2raw889 {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := &h2raw889{t: t, conn: conn, fr: http2.NewFramer(conn, conn), addr: addr}
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	if !o.prefaceOnly {
		var s []http2.Setting
		if o.initialWindow != 0 {
			s = append(s, http2.Setting{ID: http2.SettingInitialWindowSize, Val: o.initialWindow})
		}
		if err := c.fr.WriteSettings(s...); err != nil {
			t.Fatal(err)
		}
		// Take the server's SETTINGS and ack them now, so that no frame of the
		// client's lands between a HEADERS and its CONTINUATION later.
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			f, err := c.fr.ReadFrame()
			if err != nil {
				t.Fatalf("no SETTINGS from the server: %v", err)
			}
			if sf, ok := f.(*http2.SettingsFrame); ok && !sf.IsAck() {
				if err := c.fr.WriteSettingsAck(); err != nil {
					t.Fatal(err)
				}
				break
			}
		}
	}
	c.t0 = time.Now()
	return c
}

func (c *h2raw889) block(method, path string) []byte {
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", method}, {":scheme", "http"}, {":authority", c.addr}, {":path", path}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	return hb.Bytes()
}

// request sends a HEADERS frame for stream id. endStream false leaves the
// request open (a body the client has not sent); endHeaders false leaves the
// header block open for a CONTINUATION that never comes.
func (c *h2raw889) request(id uint32, method, path string, endStream, endHeaders bool) {
	c.t.Helper()
	blk := c.block(method, path)
	if err := c.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: blk, EndStream: endStream, EndHeaders: endHeaders}); err != nil {
		c.t.Fatal(err)
	}
	c.t0 = time.Now()
}

// outcome889 is how a watch ended.
type outcome889 struct {
	open     bool          // still open (no EOF, GOAWAY or reset of the watched stream) at the end
	at       time.Duration // since the last frame the client sent: the whole watch if open, else the cut
	how      string
	frames   int // DATA frames of the watched stream
	lastData time.Duration
	resetErr http2.ErrCode
}

func (o outcome889) String() string {
	if o.open {
		return fmt.Sprintf("open after %v (%d DATA frames, the last at %v)", o.at.Round(time.Millisecond), o.frames, o.lastData.Round(time.Millisecond))
	}
	return fmt.Sprintf("cut after %v: %s (%d DATA frames, the last at %v)", o.at.Round(time.Millisecond), o.how, o.frames, o.lastData.Round(time.Millisecond))
}

// watch reads frames for up to d after the client's last frame and reports how
// the connection ended (EOF, GOAWAY), or stream id, when nonzero, was reset. It
// sends only the ack of the server's SETTINGS if dial did not take it.
func (c *h2raw889) watch(id uint32, d time.Duration) outcome889 {
	c.t.Helper()
	end := c.t0.Add(d)
	_ = c.conn.SetReadDeadline(end)
	var o outcome889
	acked := false
	for {
		f, err := c.fr.ReadFrame()
		o.at = time.Since(c.t0)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				o.open, o.at = true, d
				return o
			}
			o.how = err.Error()
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				o.how = "EOF"
			}
			return o
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() && !acked {
				acked = true
				// The ack is a frame the client sends; it goes out at once, so
				// the clock below still starts at the request.
				c.wmu.Lock()
				_ = c.fr.WriteSettingsAck()
				c.wmu.Unlock()
			}
		case *http2.DataFrame:
			if id == 0 || f.StreamID == id {
				o.frames++
				o.lastData = o.at
			}
		case *http2.RSTStreamFrame:
			if id != 0 && f.StreamID == id {
				o.how, o.resetErr = "RST_STREAM "+f.ErrCode.String(), f.ErrCode
				return o
			}
		case *http2.GoAwayFrame:
			o.how = "GOAWAY " + f.ErrCode.String()
			return o
		}
	}
}

// holdRoute889 registers /hold889: an async route that sends the head and a
// chunk every tick889 for up to maxLife (or until its context ends), then closes
// the response. ended is set when the handler returns.
func holdRoute889(s *celeris.Server, maxLife time.Duration, ended *atomic.Bool) {
	tickRoute889(s, "/hold889", tick889, maxLife, ended)
}

// tickRoute889 is holdRoute889 on a path with its own interval between chunks.
func tickRoute889(s *celeris.Server, path string, tick, maxLife time.Duration, ended *atomic.Bool) {
	s.GET(path, func(c *celeris.Context) error {
		defer ended.Store(true)
		sw := c.StreamWriter()
		if sw == nil {
			return errors.New("no StreamWriter")
		}
		if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/event-stream"}}); err != nil {
			return err
		}
		done := c.Context().Done()
		tk := time.NewTicker(tick)
		defer tk.Stop()
		stop := time.After(maxLife)
		for {
			if _, err := sw.Write([]byte("data: tick\n\n")); err != nil {
				return nil
			}
			if err := sw.Flush(); err != nil {
				return nil
			}
			select {
			case <-done:
				return nil
			case <-stop:
				return sw.Close()
			case <-tk.C:
			}
		}
	}).Async()
}

// TestH2OpenStreamIsNotCutByReadOrIdleTimeout889: an h2c client that has an
// SSE-like stream open and sends no frame is not cut by ReadTimeout or by
// IdleTimeout (each 1 s, the other off) for 3 s, on every engine; nor by a
// WriteTimeout of 1 s alone (the guard arm, green on main). Before the
// fix epoll and adaptive (and io_uring when a sweep finds the conn quiet)
// closed the connection about 1 s after the request.
func TestH2OpenStreamIsNotCutByReadOrIdleTimeout889(t *testing.T) {
	for _, e := range engines761 {
		for _, arm := range []timeoutArm889{armRead889, armIdle889, armWrite889} {
			if e.name == "std" && arm.name == "write" {
				continue
			}
			t.Run(e.name+"/"+arm.name, func(t *testing.T) {
				t.Parallel()
				var ended atomic.Bool
				addr := startServerConfig761(t, arm.config(e.eng, to889), func(s *celeris.Server) { holdRoute889(s, 6*time.Second, &ended) })
				c := dialH2raw889(t, addr, h2dial889{})
				c.request(1, "GET", "/hold889", true, true)
				o := c.watch(1, hold889)
				t.Logf("H2OPEN889 engine=%s arm=%s timeout=%v: %s", e.name, arm.name, to889, o)
				if !o.open {
					t.Fatalf("the stream was closed %v after the client's request (server %s timeout %v): %s; want it open for %v",
						o.at.Round(time.Millisecond), arm.name, to889, o.how, hold889)
				}
				if o.frames < 5 {
					t.Fatalf("only %d DATA frames in %v: the control did not stream", o.frames, hold889)
				}
			})
		}
	}
}

// TestH2QuietStreamIsNotCutByReadOrIdleTimeout889: the same with a stream that
// is quiet for longer than the timeout between its chunks (2 s apart, 1 s
// timeouts). A server write restamps the connection's clock on io_uring (a
// completed send) but not on epoll, so this is the shape that reaches io_uring:
// the gap between two events, not the stream's age, is what the timeout meets.
func TestH2QuietStreamIsNotCutByReadOrIdleTimeout889(t *testing.T) {
	for _, e := range engines761 {
		for _, arm := range []timeoutArm889{armRead889, armIdle889} {
			t.Run(e.name+"/"+arm.name, func(t *testing.T) {
				t.Parallel()
				var ended atomic.Bool
				addr := startServerConfig761(t, arm.config(e.eng, to889), func(s *celeris.Server) {
					tickRoute889(s, "/quiet889", 2*time.Second, 9*time.Second, &ended)
				})
				c := dialH2raw889(t, addr, h2dial889{})
				c.request(1, "GET", "/quiet889", true, true)
				o := c.watch(1, 5*time.Second)
				t.Logf("H2QUIET889 engine=%s arm=%s timeout=%v: %s", e.name, arm.name, to889, o)
				if !o.open {
					t.Fatalf("the stream was closed %v after the client's request (server %s timeout %v, a chunk every 2s): %s; want it open for 5s",
						o.at.Round(time.Millisecond), arm.name, to889, o.how)
				}
				if o.frames < 3 {
					t.Fatalf("only %d DATA frames in 5s: the control did not stream", o.frames)
				}
			})
		}
	}
}

// TestH2ConnWithoutAStreamIsStillReaped889: the exemption above is for a
// connection with a stream open. One without (a client that completed the
// preface and sent nothing, one that sent a part of a frame, one whose streams
// all ended) is closed by ReadTimeout and by IdleTimeout, each alone, on every
// engine. A mutant that skips both checks for every h2 connection fails these.
func TestH2ConnWithoutAStreamIsStillReaped889(t *testing.T) {
	type scenario struct {
		name string
		run  func(t *testing.T, addr string) *h2raw889
	}
	scenarios := []scenario{
		{"preface-only", func(t *testing.T, addr string) *h2raw889 {
			return dialH2raw889(t, addr, h2dial889{prefaceOnly: true})
		}},
		{"preface-and-settings", func(t *testing.T, addr string) *h2raw889 {
			return dialH2raw889(t, addr, h2dial889{})
		}},
		{"partial-frame", func(t *testing.T, addr string) *h2raw889 {
			c := dialH2raw889(t, addr, h2dial889{})
			// The first 5 bytes of a HEADERS frame header: slowloris on the
			// frame layer. No stream exists yet.
			if _, err := c.conn.Write([]byte{0, 0, 40, 1, 5}); err != nil {
				t.Fatal(err)
			}
			c.t0 = time.Now()
			return c
		}},
		{"partial-frame-payload", func(t *testing.T, addr string) *h2raw889 {
			c := dialH2raw889(t, addr, h2dial889{})
			// A whole frame header declaring 40 bytes of HEADERS payload and 3
			// bytes of it: the frame never completes, so no stream is made.
			if _, err := c.conn.Write([]byte{0, 0, 40, 1, 5, 0, 0, 0, 1, 0x82, 0x86, 0x84}); err != nil {
				t.Fatal(err)
			}
			c.t0 = time.Now()
			return c
		}},
		{"stream-finished", func(t *testing.T, addr string) *h2raw889 {
			c := dialH2raw889(t, addr, h2dial889{})
			c.request(1, "GET", "/ping", true, true)
			// Read the whole response, then stay silent: the stream is closed.
			end := time.Now().Add(2 * time.Second)
			_ = c.conn.SetReadDeadline(end)
			for {
				f, err := c.fr.ReadFrame()
				if err != nil {
					t.Fatalf("no response to /ping: %v", err)
				}
				if sf, ok := f.(*http2.SettingsFrame); ok && !sf.IsAck() {
					_ = c.fr.WriteSettingsAck()
				}
				if df, ok := f.(*http2.DataFrame); ok && df.StreamID == 1 && df.StreamEnded() {
					break
				}
			}
			c.t0 = time.Now()
			return c
		}},
		{"header-block-open", func(t *testing.T, addr string) *h2raw889 {
			// HEADERS without END_HEADERS and no CONTINUATION: no stream is
			// open until the block is whole.
			c := dialH2raw889(t, addr, h2dial889{})
			c.request(1, "GET", "/hold889", true, false)
			return c
		}},
		{"streams-refused-then-reset", func(t *testing.T, addr string) *h2raw889 {
			// 101 streams against MAX_CONCURRENT_STREAMS 100: the last is
			// refused. The client then resets all of them, so a count that a
			// refusal or a reset left behind would exempt the conn for good.
			c := dialH2raw889(t, addr, h2dial889{})
			const n = 101
			for i := range n {
				c.request(uint32(2*i+1), "GET", "/hold889", true, true)
			}
			time.Sleep(300 * time.Millisecond)
			for i := range n {
				if err := c.fr.WriteRSTStream(uint32(2*i+1), http2.ErrCodeCancel); err != nil {
					t.Fatal(err)
				}
			}
			c.t0 = time.Now()
			return c
		}},
		{"stream-reset-by-client", func(t *testing.T, addr string) *h2raw889 {
			c := dialH2raw889(t, addr, h2dial889{})
			c.request(1, "GET", "/hold889", true, true)
			time.Sleep(300 * time.Millisecond) // the handler is running and sending
			if err := c.fr.WriteRSTStream(1, http2.ErrCodeCancel); err != nil {
				t.Fatal(err)
			}
			c.t0 = time.Now()
			return c
		}},
	}
	for _, e := range engines761 {
		for _, arm := range []timeoutArm889{armRead889, armIdle889} {
			if e.name == "std" && arm.name == "read" {
				// Reference behaviour, measured: net/http's HTTP/2 server does
				// not close a connection without a stream at ReadTimeout (that
				// timeout is armed per stream, for its request); IdleTimeout
				// does. No cell: a skip would trip the CI allow-list.
				continue
			}
			for _, sc := range scenarios {
				t.Run(e.name+"/"+arm.name+"/"+sc.name, func(t *testing.T) {
					t.Parallel()
					var ended atomic.Bool
					addr := startServerConfig761(t, arm.config(e.eng, to889), func(s *celeris.Server) { holdRoute889(s, 30*time.Second, &ended) })
					c := sc.run(t, addr)
					o := c.watch(0, to889+reap889)
					t.Logf("H2REAP889 engine=%s arm=%s scenario=%s timeout=%v: %s", e.name, arm.name, sc.name, to889, o)
					if o.open {
						t.Fatalf("the connection was still open %v after the client's last frame (server %s timeout %v): want it closed within %v",
							o.at.Round(time.Millisecond), arm.name, to889, to889+reap889)
					}
				})
			}
		}
	}
}

// TestH2ReadTimeoutCountsAnyClientFrame889: the guard for option B of the
// decision in the PR: on main every read restamps the connection's clock, so a
// client that sends a frame of any kind (a PING here) at least every ReadTimeout
// keeps a held stream open under ReadTimeout 1 s. This passes on main and must
// keep passing.
func TestH2ReadTimeoutCountsAnyClientFrame889(t *testing.T) {
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			var ended atomic.Bool
			addr := startServerConfig761(t, armRead889.config(e.eng, to889), func(s *celeris.Server) { holdRoute889(s, 6*time.Second, &ended) })
			c := dialH2raw889(t, addr, h2dial889{})
			c.request(1, "GET", "/hold889", true, true)
			stop := make(chan struct{})
			pinged := make(chan struct{})
			go func() {
				defer close(pinged)
				tk := time.NewTicker(300 * time.Millisecond)
				defer tk.Stop()
				for {
					select {
					case <-stop:
						return
					case <-tk.C:
						c.wmu.Lock()
						_ = c.fr.WritePing(false, [8]byte{'l', '1', '9'})
						c.wmu.Unlock()
					}
				}
			}()
			o := c.watch(1, hold889)
			close(stop)
			<-pinged
			t.Logf("H2PING889 engine=%s: %s", e.name, o)
			if !o.open {
				t.Fatalf("the stream was closed %v after the request although the client pinged every 300 ms: %s", o.at.Round(time.Millisecond), o.how)
			}
		})
	}
}
