//go:build linux

package celeris_test

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
)

// The per-stream request deadline of the native engines (celeris#889, L19 D1).
//
// ReadTimeout bounds each HTTP/2 request, as net/http's does: a stream whose
// request is not whole ReadTimeout after its HEADERS is answered 408 and reset
// with RST_STREAM(NO_ERROR) (RFC 9113 section 8.1), the connection and its other
// streams go on. The deadline is absolute from the HEADERS (std's, and the
// documented meaning of Config.ReadTimeout, "the max duration for reading the
// entire request"): a DATA frame does not move it, and neither does a PING. A
// request that is whole at its HEADERS (every GET) has no deadline.
//
// Server: ReadTimeout 1 s, IdleTimeout off, WriteTimeout 2 s. The handler of a
// POST runs only once the request is whole (the native path buffers the body),
// so an expired request has no handler to race.

// rdServer889 is the server of these cells: /upload889 answers the length of
// the body it got, /hold889 is an SSE-like stream, /ping answers ok, /big889 a
// body of ?n bytes.
func rdServer889(t *testing.T, eng celeris.EngineType, mutate func(*celeris.Config)) string {
	t.Helper()
	var ended atomic.Bool
	cfg := celeris.Config{Engine: eng, ReadTimeout: to889, IdleTimeout: -1, WriteTimeout: 2 * time.Second}
	if mutate != nil {
		mutate(&cfg)
	}
	return startServerConfig761(t, cfg, func(s *celeris.Server) {
		holdRoute889(s, 30*time.Second, &ended)
		s.POST("/upload889", func(c *celeris.Context) error {
			return c.String(200, "got %d", len(c.Body()))
		})
		s.GET("/big889", func(c *celeris.Context) error {
			n, _ := strconv.Atoi(c.Query("n"))
			return c.Blob(200, "application/octet-stream", make([]byte, n))
		}).Async()
	})
}

// rdEvent889 is what a stream (and its connection) did, as the client saw it.
type rdEvent889 struct {
	status  string        // :status of the first HEADERS on the stream, "" if none
	reset   bool          // RST_STREAM on the stream
	rst     http2.ErrCode // its code
	ended   bool          // END_STREAM on a HEADERS or DATA of the stream
	data    int           // DATA bytes on the stream
	at      time.Duration // since c.t0, when the wait ended
	connEnd string        // "" while the conn is open; "EOF" or "GOAWAY <code>"
	timeout bool          // the wait ran out
}

func (e rdEvent889) String() string {
	return fmt.Sprintf("status=%q reset=%v rst=%v ended=%v data=%d at=%v connEnd=%q timeout=%v",
		e.status, e.reset, e.rst, e.ended, e.data, e.at.Round(time.Millisecond), e.connEnd, e.timeout)
}

// await889 reads frames until the stream id is reset or ended, the conn ends, or
// d has passed since c.t0.
func (c *h2raw889) await889(id uint32, d time.Duration) rdEvent889 {
	c.t.Helper()
	var ev rdEvent889
	dec := hpack.NewDecoder(4096, nil)
	_ = c.conn.SetReadDeadline(c.t0.Add(d))
	for {
		f, err := c.fr.ReadFrame()
		ev.at = time.Since(c.t0)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				ev.timeout = true
				return ev
			}
			ev.connEnd = "EOF"
			return ev
		}
		switch f := f.(type) {
		case *http2.HeadersFrame:
			if f.StreamID == id {
				if fields, derr := dec.DecodeFull(f.HeaderBlockFragment()); derr == nil && ev.status == "" {
					for _, h := range fields {
						if h.Name == ":status" {
							ev.status = h.Value
						}
					}
				}
				if f.StreamEnded() {
					ev.ended = true
				}
			}
		case *http2.DataFrame:
			if f.StreamID == id {
				ev.data += len(f.Data())
				if f.StreamEnded() {
					ev.ended = true
					return ev
				}
			}
		case *http2.RSTStreamFrame:
			if f.StreamID == id {
				ev.reset, ev.rst = true, f.ErrCode
				return ev
			}
		case *http2.GoAwayFrame:
			ev.connEnd = "GOAWAY " + f.ErrCode.String()
			return ev
		}
	}
}

// pingEvery889 sends a PING every d until the returned stop is called.
func (c *h2raw889) pingEvery889(d time.Duration) (stop func()) {
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(d)
		defer tk.Stop()
		for {
			select {
			case <-quit:
				return
			case <-tk.C:
				c.wmu.Lock()
				_ = c.fr.WritePing(false, [8]byte{'8', '8', '9'})
				c.wmu.Unlock()
			}
		}
	}()
	return func() { close(quit); <-done }
}

// trickle889 sends one byte of DATA on stream id every d, as a slow uploader
// does, until the returned stop is called. A client that sends nothing at all
// for ReadTimeout is silent, and a connection with no stream open that has been
// silent for ReadTimeout is closed whatever happened to its streams; an uploader
// that is slow but sending is not.
func (c *h2raw889) trickle889(id uint32, d time.Duration) (stop func()) {
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(d)
		defer tk.Stop()
		for {
			select {
			case <-quit:
				return
			case <-tk.C:
				c.wmu.Lock()
				_ = c.fr.WriteData(id, false, []byte{'x'})
				c.wmu.Unlock()
			}
		}
	}()
	return func() { close(quit); <-done }
}

func (c *h2raw889) sendData889(id uint32, end bool, b []byte) {
	c.t.Helper()
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.fr.WriteData(id, end, b); err != nil {
		c.t.Fatalf("DATA on stream %d: %v", id, err)
	}
}

// wantReset889 checks the expiry of a request: 408, RST_STREAM(NO_ERROR), at about
// the deadline, and the conn open.
func wantReset889(t *testing.T, ev rdEvent889, lo, hi time.Duration) {
	t.Helper()
	if ev.connEnd != "" || ev.timeout || !ev.reset {
		t.Fatalf("want the stream answered 408 and reset (RST_STREAM NO_ERROR) in [%v, %v], conn open; got %s", lo, hi, ev)
	}
	if ev.status != "408" || ev.rst != http2.ErrCodeNo {
		t.Fatalf("want 408 and RST_STREAM(NO_ERROR), got %s", ev)
	}
	if ev.at < lo || ev.at > hi {
		t.Fatalf("the stream was reset at %v, want it in [%v, %v]", ev.at.Round(time.Millisecond), lo, hi)
	}
}

// serves889 sends GET /ping on stream id and wants a 200: the conn is alive and
// serving.
func (c *h2raw889) serves889(id uint32) {
	c.t.Helper()
	c.request(id, "GET", "/ping", true, true)
	ev := c.await889(id, time.Since(c.t0)+3*time.Second)
	if ev.status != "200" || ev.connEnd != "" {
		c.t.Fatalf("GET /ping on stream %d after the reset: %s, want 200 on an open conn", id, ev)
	}
}

// TestH2PingingClientRequestIsAnsweredAndReset889 is the pinned ping cell of
// the previous work FLIPPED: a client that never completes its POST and sends a
// PING every 500 ms is no longer held: the request is answered 408 and reset at
// ReadTimeout, the conn stays and serves the next request. (The pinned cells for
// an unread RESPONSE stay pinned until the per-stream write deadline; see
// TestH2PingingClientHoldsAnUnfinishedStream889.)
func TestH2PingingClientRequestIsAnsweredAndReset889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, rdServer889(t, e.eng, nil), h2dial889{})
			c.request(1, "POST", "/upload889", false, true)
			stop := c.pingEvery889(500 * time.Millisecond)
			defer stop()
			ev := c.await889(1, 5*time.Second)
			t.Logf("RD889 ping engine=%s: %s", e.name, ev)
			wantReset889(t, ev, 900*time.Millisecond, 2500*time.Millisecond)
			c.serves889(3)
		})
	}
}

// TestH2RequestDeadlineIsAbsolute889: a client that sends one byte of DATA
// every 300 ms is reset at ReadTimeout from the HEADERS, as net/http does; the
// deadline is not moved by DATA.
func TestH2RequestDeadlineIsAbsolute889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, rdServer889(t, e.eng, nil), h2dial889{})
			c.request(1, "POST", "/upload889", false, true)
			quit := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				tk := time.NewTicker(300 * time.Millisecond)
				defer tk.Stop()
				for {
					select {
					case <-quit:
						return
					case <-tk.C:
						c.wmu.Lock()
						_ = c.fr.WriteData(1, false, []byte{'x'})
						c.wmu.Unlock()
					}
				}
			}()
			ev := c.await889(1, 5*time.Second)
			close(quit)
			<-done
			t.Logf("RD889 absolute engine=%s: %s", e.name, ev)
			wantReset889(t, ev, 900*time.Millisecond, 2500*time.Millisecond)
		})
	}
}

// TestH2SlowUploaderAtTheDeadline889: an upload that completes before the
// deadline is served; one that completes after it was answered 408 and reset at
// the deadline, and its remaining DATA (and END_STREAM) arrive on a stream the
// server reset: ignored, the conn goes on.
func TestH2SlowUploaderAtTheDeadline889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name+"/before", func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, rdServer889(t, e.eng, nil), h2dial889{})
			c.request(1, "POST", "/upload889", false, true)
			time.Sleep(300 * time.Millisecond)
			c.sendData889(1, false, []byte("hello "))
			time.Sleep(300 * time.Millisecond) // 0.6 s, before the 1 s deadline
			c.sendData889(1, true, []byte("world"))
			ev := c.await889(1, 4*time.Second)
			t.Logf("RD889 before engine=%s: %s", e.name, ev)
			if ev.status != "200" || ev.reset || !ev.ended || ev.connEnd != "" {
				t.Fatalf("an upload that completed at 0.6 s (ReadTimeout 1 s) must be served: %s", ev)
			}
		})
		t.Run(e.name+"/after", func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, rdServer889(t, e.eng, nil), h2dial889{})
			c.request(1, "POST", "/upload889", false, true)
			stop := c.trickle889(1, 250*time.Millisecond)
			ev := c.await889(1, 3*time.Second)
			stop()
			t.Logf("RD889 after engine=%s: %s", e.name, ev)
			wantReset889(t, ev, 900*time.Millisecond, 2500*time.Millisecond)
			// The uploader had not finished: its DATA and END_STREAM are in flight.
			c.sendData889(1, false, []byte("world"))
			c.sendData889(1, true, []byte("!"))
			c.serves889(3)
		})
	}
}

// TestH2FastUploadIsNotKilled889: a 1 MiB upload, flow-controlled by the
// client, completes well inside the deadline and is served in full.
func TestH2FastUploadIsNotKilled889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			addr := rdServer889(t, e.eng, nil)
			c := dialH2raw889(t, addr, h2dial889{})
			c.request(1, "POST", "/upload889", false, true)
			const total = 1 << 20
			chunk := make([]byte, 16384)
			sent, connWin, strWin := 0, 65535, 65535
			var ev rdEvent889
			dec := hpack.NewDecoder(4096, nil)
			_ = c.conn.SetReadDeadline(c.t0.Add(8 * time.Second))
			for ev.status == "" {
				for sent < total && connWin >= len(chunk) && strWin >= len(chunk) {
					end := sent+len(chunk) == total
					c.sendData889(1, end, chunk)
					sent += len(chunk)
					connWin -= len(chunk)
					strWin -= len(chunk)
				}
				f, err := c.fr.ReadFrame()
				if err != nil {
					t.Fatalf("after %d of %d bytes: %v", sent, total, err)
				}
				switch f := f.(type) {
				case *http2.WindowUpdateFrame:
					if f.StreamID == 0 {
						connWin += int(f.Increment)
					} else if f.StreamID == 1 {
						strWin += int(f.Increment)
					}
				case *http2.HeadersFrame:
					if f.StreamID == 1 {
						fields, _ := dec.DecodeFull(f.HeaderBlockFragment())
						for _, h := range fields {
							if h.Name == ":status" {
								ev.status = h.Value
							}
						}
					}
				case *http2.RSTStreamFrame:
					t.Fatalf("the upload was reset after %d of %d bytes (%v)", sent, total, f.ErrCode)
				}
			}
			t.Logf("RD889 fast engine=%s: %d bytes in %v, status %s", e.name, sent, time.Since(c.t0).Round(time.Millisecond), ev.status)
			if ev.status != "200" || sent != total {
				t.Fatalf("status %q after %d of %d bytes, want 200 after all", ev.status, sent, total)
			}
		})
	}
}

// TestH2ExpiredRequestDataAfterTheResetWindow889 is the M3 scenario of the
// design review. The server remembers the streams it reset in a window of 2048
// odd stream identifiers; a stream whose deadline expires after more than that
// many newer streams, and a newer reset, would be forgotten, and the uploader's
// DATA in flight would then be DATA on an idle stream: GOAWAY(PROTOCOL_ERROR),
// every stream of the conn gone. net/http never ends a conn for DATA on a stream
// it closed. Here: stream 1 is a POST that stays open, 2100 GETs follow, a
// malformed request after them is reset (the window moves past stream 1), then
// the deadline of stream 1 expires; its DATA and its trailers arrive late.
func TestH2ExpiredRequestDataAfterTheResetWindow889(t *testing.T) {
	for _, e := range nativeEngines889() {
		for _, late := range []string{"data", "trailers"} {
			t.Run(e.name+"/"+late, func(t *testing.T) {
				t.Parallel()
				c := dialH2raw889(t, rdServer889(t, e.eng, nil), h2dial889{})
				c.request(1, "POST", "/upload889", false, true)
				id := uint32(3)
				for range 2100 {
					c.request(id, "GET", "/ping", true, true)
					id += 2
				}
				// A request without :path: a stream error, RST_STREAM, remembered.
				c.wmu.Lock()
				noPath := []byte{0x82, 0x86, 0x41, 0x01, 'x'} // :method GET, :scheme http, :authority x; no :path
				err := c.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: noPath, EndStream: true, EndHeaders: true})
				c.wmu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				bad := id
				id += 2
				stop := c.trickle889(1, 250*time.Millisecond)
				ev := c.await889(1, 4*time.Second)
				stop()
				t.Logf("RD889 m3 engine=%s late=%s: %s (the malformed stream was %d)", e.name, late, ev, bad)
				wantReset889(t, ev, 900*time.Millisecond, 3*time.Second)
				switch late {
				case "data":
					c.sendData889(1, false, []byte("late"))
					c.sendData889(1, true, []byte("!"))
				default:
					c.sendData889(1, false, []byte("late"))
					c.wmu.Lock()
					err := c.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: []byte{0x40, 0x01, 'x', 0x01, 'y'}, EndStream: true, EndHeaders: true})
					c.wmu.Unlock()
					if err != nil {
						t.Fatal(err)
					}
				}
				c.serves889(id)
			})
		}
	}
}

// TestH2RequestDeadlineResetsOneStreamNotTheConn889 is the M2 scenario: a quiet
// SSE stream S, a request R that is never completed, and a client that sends
// nothing. R is answered 408 and reset at ReadTimeout; S goes on streaming and
// the conn stays. (Before the per-stream deadline the conn clock closed the
// whole conn, S included, as soon as R made it a conn the client owed.)
func TestH2RequestDeadlineResetsOneStreamNotTheConn889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name+"/quiet-sse", func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, rdServer889(t, e.eng, nil), h2dial889{})
			c.request(1, "GET", "/hold889", true, true)
			c.request(3, "POST", "/upload889", false, true)
			t0 := c.t0
			ev := c.await889(3, 4*time.Second)
			t.Logf("RD889 mixed engine=%s: R %s", e.name, ev)
			wantReset889(t, ev, 900*time.Millisecond, 2500*time.Millisecond)
			// S is still streaming: a DATA frame of stream 1 arrives after the reset.
			c.t0 = t0
			s := c.await889(1, 3500*time.Millisecond)
			if s.connEnd != "" || s.reset || s.ended {
				t.Fatalf("the SSE stream did not survive the reset of the unfinished request: %s", s)
			}
		})
		// IdleTimeout below ReadTimeout: net/http applies IdleTimeout only while
		// no stream is open. The open request is bounded by ReadTimeout (3 s),
		// the conn is not closed at IdleTimeout (1 s).
		t.Run(e.name+"/idle-below-read", func(t *testing.T) {
			t.Parallel()
			addr := rdServer889(t, e.eng, func(c *celeris.Config) { c.ReadTimeout, c.IdleTimeout = 3*time.Second, time.Second })
			c := dialH2raw889(t, addr, h2dial889{})
			c.request(1, "POST", "/upload889", false, true)
			ev := c.await889(1, 6*time.Second)
			t.Logf("RD889 idle<read engine=%s: %s", e.name, ev)
			wantReset889(t, ev, 2700*time.Millisecond, 4500*time.Millisecond)
			// The conn's own clock (IdleTimeout 1 s) was due all along and the open
			// request exempted the conn from it; with the request reset, nothing is
			// left to keep the silent conn for: it goes with the reset, not another
			// IdleTimeout later (the stamp of the last owed stream is cleared at an
			// expiry). io_uring's clock is restamped by the 408's completed send.
			at, closed := c.closedAfter889(7 * time.Second)
			bound := 3600 * time.Millisecond
			if e.name == "io_uring" {
				bound = 5500 * time.Millisecond
			}
			if !closed || at > bound {
				t.Fatalf("the silent conn was closed=%v at %v, want it closed by %v (the reset at 3 s)", closed, at.Round(time.Millisecond), bound)
			}
		})
	}
}

// TestH2FlowingDownloadIsNotResetByTheRequestDeadline889: a large response taken
// as fast as the client grants window, taking longer than ReadTimeout, is never
// reset: the request deadline is about requests.
func TestH2FlowingDownloadIsNotResetByTheRequestDeadline889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			addr := rdServer889(t, e.eng, nil)
			c := dialH2raw889(t, addr, h2dial889{})
			c.request(1, "GET", "/big889?n=8388608", true, true)
			var got int
			_ = c.conn.SetReadDeadline(c.t0.Add(20 * time.Second))
			var started time.Time
			for {
				f, err := c.fr.ReadFrame()
				if err != nil {
					t.Fatalf("after %d bytes: %v", got, err)
				}
				switch f := f.(type) {
				case *http2.DataFrame:
					if f.StreamID != 1 {
						continue
					}
					if started.IsZero() {
						started = time.Now()
					}
					got += len(f.Data())
					n := uint32(len(f.Data()))
					c.wmu.Lock()
					_ = c.fr.WriteWindowUpdate(0, n)
					_ = c.fr.WriteWindowUpdate(1, n)
					c.wmu.Unlock()
					time.Sleep(4 * time.Millisecond) // 512 frames: about 2 s, twice ReadTimeout
					if f.StreamEnded() {
						t.Logf("RD889 download engine=%s: %d bytes in %v", e.name, got, time.Since(started).Round(time.Millisecond))
						return
					}
				case *http2.RSTStreamFrame:
					t.Fatalf("the download was reset after %d bytes (%v)", got, f.ErrCode)
				}
			}
		})
	}
}

// TestH2SilentUnfinishedRequestEndsTheConnWithTheReset889: a client that opens a
// POST, sends nothing more and is silent for ReadTimeout (1 s) has the request
// answered 408 and reset at 1 s, and then, having nothing left open and having
// been silent for ReadTimeout, loses the connection at once: the idle period of
// the conn is not kept another timeout for a stream the deadline reset (the
// stamp of the last stream the server owed is cleared at an expiry).
func TestH2SilentUnfinishedRequestEndsTheConnWithTheReset889(t *testing.T) {
	for _, e := range nativeEngines889() {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			c := dialH2raw889(t, rdServer889(t, e.eng, nil), h2dial889{})
			c.request(1, "POST", "/upload889", false, true)
			ev := c.await889(1, 4*time.Second)
			wantReset889(t, ev, 900*time.Millisecond, 2500*time.Millisecond)
			at, closed := c.closedAfter889(4 * time.Second)
			t.Logf("RD889 silent engine=%s: reset at %v, conn closed=%v at %v", e.name, ev.at.Round(time.Millisecond), closed, at.Round(time.Millisecond))
			// io_uring restamps the conn's clock at the completion of a send, the
			// 408 included (its clock is "the last read or completed send"), so its
			// silent conn goes ReadTimeout after the reset; epoll and adaptive have
			// the client's last frame as the clock and close it with the reset.
			bound := 1700 * time.Millisecond
			if e.name == "io_uring" {
				bound = 2800 * time.Millisecond
			}
			if !closed || at > bound {
				t.Fatalf("the conn of a silent client was closed=%v at %v, want it closed by %v (the reset at about 1 s, not another timeout kept for the stream)", closed, at.Round(time.Millisecond), bound)
			}
		})
	}
}
