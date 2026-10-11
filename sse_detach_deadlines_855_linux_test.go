//go:build linux

package celeris_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

// Tests for celeris#855. On the std engine an SSE stream ended at the server's
// WriteTimeout, while epoll, io_uring and adaptive kept it open: net/http arms
// a write deadline for each response and keeps it armed while the handler
// runs, so the first Send after it fails (over h2c the HTTP/2 server resets
// the stream with INTERNAL_ERROR at the same moment). A detached stream
// (Context.Detach, which the SSE middleware calls) is the connection's, as it
// is on the native engines, which apply neither timeout to it; Detach now
// clears the write deadline on std, and the read deadline with it.
//
// ReadTimeout is not a cut on std (go1.27): net/http clears the read deadline
// when it starts watching the connection for the client going away, before
// the handler runs, and over h2c ReadTimeout only closes the request body,
// which the bridge has read whole. The read arms therefore pass on main, and
// they stay as the guard that they keep passing; the issue's first path (the
// read deadline ending the request's context) is not what cut the bench's
// streams. Clearing the read deadline in Detach is defensive.
//
// The server timeouts are 1 s and a stream is held for 4.5 s, so a stream
// that is cut at either is cut four times over.

const (
	// timeout855 is the ReadTimeout and/or WriteTimeout of the server under
	// test.
	timeout855 = time.Second
	// hold855 is how long a stream must keep delivering events after the
	// first one: more than four timeouts.
	hold855 = 4500 * time.Millisecond
	// tick855 is the interval of the stream's events.
	tick855 = 200 * time.Millisecond
)

// big855 is the size of the response a stalled client does not read: far over
// what the sockets on both ends buffer. BIG855 (bytes) to change it.
var big855 = func() int {
	if n, err := strconv.Atoi(os.Getenv("BIG855")); err == nil && n > 0 {
		return n
	}
	return 48 << 20
}()

// arm855 is a server with a ReadTimeout, a WriteTimeout or both set. -1 is
// the documented "off": 0 would be the default, 30 s and 60 s.
type arm855 struct {
	name        string
	read, write time.Duration
}

var (
	armRead855  = arm855{"read", timeout855, -1}
	armWrite855 = arm855{"write", -1, timeout855}
	armBoth855  = arm855{"both", timeout855, timeout855}
)

func (a arm855) config(eng celeris.EngineType, async bool) celeris.Config {
	return celeris.Config{
		Engine:        eng,
		AsyncHandlers: async,
		ReadTimeout:   a.read,
		WriteTimeout:  a.write,
		// ReadHeaderTimeout falls back to ReadTimeout on std; the request
		// headers are in long before either.
	}
}

// events855 registers /events: an SSE stream that sends an event every
// tick855 for as long as the client is there (events == 0), or n events and
// then ends. HeartbeatInterval is off, as the bench adapter's /events has no
// heartbeat: the stream ends when its context ends or a Send fails.
func events855(s *celeris.Server, n int) {
	s.GET("/events", sse.New(sse.Config{
		HeartbeatInterval: -1,
		Handler: func(client *sse.Client) {
			tk := time.NewTicker(tick855)
			defer tk.Stop()
			ctx := client.Context()
			for i := 1; n == 0 || i <= n; i++ {
				select {
				case <-ctx.Done():
					return
				case <-tk.C:
				}
				if err := client.Send(sse.Event{Data: fmt.Sprint(i)}); err != nil {
					return
				}
			}
		},
	}))
}

// big855Route registers /big: a body no socket buffers.
func big855Route(s *celeris.Server) {
	body := make([]byte, big855)
	for i := range body {
		body[i] = byte(i)
	}
	s.GET("/big", func(c *celeris.Context) error {
		return c.Blob(http.StatusOK, "application/octet-stream", body)
	})
}

// readEvents855 reads data: lines from an SSE body until one arrives hold
// after the first, and returns how many it saw. The error says how the stream
// ended if it did before that.
func readEvents855(r io.Reader, hold time.Duration) (events int, span time.Duration, err error) {
	br := bufio.NewReader(r)
	var first time.Time
	for {
		line, rerr := br.ReadString('\n')
		if strings.HasPrefix(line, "data:") {
			events++
			if first.IsZero() {
				first = time.Now()
			}
			span = time.Since(first)
			if span >= hold {
				return events, span, nil
			}
		}
		if rerr != nil {
			if !first.IsZero() {
				span = time.Since(first)
			}
			return events, span, rerr
		}
	}
}

// sseOverH1855 opens /events on a raw connection and holds it.
func sseOverH1855(t *testing.T, addr string) (events int, span time.Duration, err error) {
	t.Helper()
	conn, derr := net.DialTimeout("tcp", addr, 2*time.Second)
	if derr != nil {
		t.Fatal(derr)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(hold855 + 10*time.Second))
	req := "GET /events HTTP/1.1\r\nHost: " + addr + "\r\nAccept: text/event-stream\r\n\r\n"
	if _, werr := conn.Write([]byte(req)); werr != nil {
		t.Fatal(werr)
	}
	return readEvents855(conn, hold855)
}

// sseOverH2C855 opens /events over h2c (prior knowledge) and holds it.
func sseOverH2C855(t *testing.T, addr string) (events int, span time.Duration, err error) {
	t.Helper()
	cl := h2cClient836(1)
	cl.Timeout = hold855 + 10*time.Second
	defer cl.CloseIdleConnections()
	req, rerr := http.NewRequest(http.MethodGet, "http://"+addr+"/events", nil)
	if rerr != nil {
		t.Fatal(rerr)
	}
	resp, derr := cl.Do(req)
	if derr != nil {
		t.Fatalf("GET /events over h2c: %v", derr)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 2 {
		t.Fatalf("GET /events came back as %s, not HTTP/2", resp.Proto)
	}
	return readEvents855(resp.Body, hold855)
}

type sseCell855 struct {
	eng   string
	async bool
	h2c   bool
	arm   arm855
}

func (c sseCell855) name() string {
	proto := "h1"
	if c.h2c {
		proto = "h2c"
	}
	mode := "sync"
	if c.async {
		mode = "async"
	}
	return fmt.Sprintf("%s/%s/%s/%s", c.eng, proto, mode, c.arm.name)
}

// TestSSEStaysOpenPastServerTimeouts855: with ReadTimeout and/or WriteTimeout
// at 1 s, an SSE stream keeps delivering events for 4.5 s on every engine,
// sync and async, over HTTP/1.1, and on std over h2c as well. std has all
// three arms; the others, which never cut a stream, have "both".
//
// h2c is std's only: on epoll, io_uring and adaptive an HTTP/2 stream's
// response goes out when its handler returns (Flush is no delivery guarantee
// on HTTP/2, see StreamWriter.Flush), so an SSE stream over h2c shows no
// headers until it ends (a polish item on #889). That is a defect of its own,
// not this one.
func TestSSEStaysOpenPastServerTimeouts855(t *testing.T) {
	var cells []sseCell855
	for _, e := range engines761 {
		for _, async := range []bool{false, true} {
			for _, h2c := range []bool{false, true} {
				if h2c && e.name != "std" {
					continue
				}
				arms := []arm855{armBoth855}
				if e.name == "std" {
					arms = []arm855{armRead855, armWrite855, armBoth855}
				}
				for _, a := range arms {
					cells = append(cells, sseCell855{e.name, async, h2c, a})
				}
			}
		}
	}
	engOf := map[string]celeris.EngineType{}
	for _, e := range engines761 {
		engOf[e.name] = e.eng
	}
	for _, cell := range cells {
		t.Run(cell.name(), func(t *testing.T) {
			t.Parallel()
			cfg := cell.arm.config(engOf[cell.eng], cell.async)
			addr := startServerConfig761(t, cfg, func(s *celeris.Server) { events855(s, 0) })
			var (
				events int
				span   time.Duration
				err    error
			)
			if cell.h2c {
				events, span, err = sseOverH2C855(t, addr)
			} else {
				events, span, err = sseOverH1855(t, addr)
			}
			t.Logf("SSE855 cell=%s events=%d span=%v err=%v", cell.name(), events, span.Round(time.Millisecond), err)
			if err != nil {
				t.Fatalf("the stream ended after %d events and %v (server ReadTimeout %v, WriteTimeout %v), want it open for %v: %v",
					events, span.Round(time.Millisecond), cell.arm.read, cell.arm.write, hold855, err)
			}
		})
	}
}

// stall855 is how long the client does not read, STALL855 (a duration) to
// change it.
func stall855() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("STALL855")); err == nil {
		return d
	}
	return 3 * timeout855
}

// stalledResponse855 asks for /big and does not read for three timeouts, then
// reads what is left: a server that bounds the response by its WriteTimeout
// has closed the connection by then (or reset the stream), so the client gets
// less than the body.
func stalledResponse855(t *testing.T, addr string, h2c bool) (got int, err error) {
	t.Helper()
	if h2c {
		cl := h2cClient836(1)
		cl.Timeout = 30 * time.Second
		defer cl.CloseIdleConnections()
		resp, derr := cl.Get("http://" + addr + "/big")
		if derr != nil {
			return 0, derr
		}
		defer func() { _ = resp.Body.Close() }()
		time.Sleep(3 * timeout855)
		cr := &countingReader761{r: resp.Body}
		_, err = io.Copy(io.Discard, cr)
		return cr.n, err
	}
	d := net.Dialer{Timeout: 2 * time.Second, Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			// A small receive buffer, so the stall reaches the server's
			// send side quickly.
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
		})
	}}
	conn, derr := d.Dial("tcp", addr)
	if derr != nil {
		t.Fatal(derr)
	}
	defer func() { _ = conn.Close() }()
	if _, werr := conn.Write([]byte("GET /big HTTP/1.1\r\nHost: " + addr + "\r\n\r\n")); werr != nil {
		t.Fatal(werr)
	}
	time.Sleep(stall855())
	cr := &countingReader761{r: idleConn761{conn}}
	_, err = io.Copy(io.Discard, cr)
	return cr.n, err
}

// TestStalledResponseStillBoundedByWriteTimeoutStd855 is the negative of the
// test above on std, the engine the fix is in: a plain response to a client
// that does not read is still cut at WriteTimeout, over HTTP/1.1 (the
// connection is closed) and over h2c (the stream is reset), so Detach is the
// only thing that lifts the timeout.
//
// std only: epoll, io_uring and adaptive deliver a body of 8 MiB or more whole
// to a client that read nothing for five WriteTimeouts (measured on main
// 3cfee4b), so there is no bound to keep there; see the polish items on #884
// and #885. The request side of the negative runs on every engine in
// TestStalledRequestStillBoundedByReadTimeout855.
func TestStalledResponseStillBoundedByWriteTimeoutStd855(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, h2c := range []bool{false, true} {
			proto := "h1"
			if h2c {
				proto = "h2c"
			}
			mode := "sync"
			if async {
				mode = "async"
			}
			t.Run(proto+"/"+mode, func(t *testing.T) {
				t.Parallel()
				addr := startServerConfig761(t, armWrite855.config(celeris.Std, async), big855Route)
				got, err := stalledResponse855(t, addr, h2c)
				t.Logf("SSE855NEG proto=%s mode=%s got=%d of %d bytes err=%v", proto, mode, got, big855, err)
				if got >= big855 {
					t.Fatalf("a client that did not read for %v was sent the whole %d-byte body: WriteTimeout %v did not bound the response",
						stall855(), big855, timeout855)
				}
			})
		}
	}
}

// TestStalledRequestStillBoundedByReadTimeout855: a request whose body stops
// coming is still cut at ReadTimeout on every engine, with the fix in: the
// read deadline Detach clears is the one of a request whose handler has
// detached, not of the requests that never get to a handler.
func TestStalledRequestStillBoundedByReadTimeout855(t *testing.T) {
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			addr := startServerConfig761(t, armRead855.config(e.eng, false), func(s *celeris.Server) {
				s.POST("/echo", func(c *celeris.Context) error { return c.Blob(http.StatusOK, "text/plain", c.Body()) })
			})
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			// 10 of the 100 promised body bytes, then nothing.
			if _, err := conn.Write([]byte("POST /echo HTTP/1.1\r\nHost: " + addr + "\r\nContent-Length: 100\r\n\r\n0123456789")); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			_ = conn.SetReadDeadline(start.Add(10 * time.Second))
			_, rerr := io.Copy(io.Discard, conn) // returns when the server closes the connection
			ended := time.Since(start)
			t.Logf("SSE855NEGREAD engine=%s ended=%v err=%v", e.name, ended.Round(10*time.Millisecond), rerr)
			if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
				t.Fatalf("the server did not end a request stalled in its body within 10 s (ReadTimeout %v)", timeout855)
			}
			if ended > 5*time.Second {
				t.Fatalf("the stalled request was ended after %v, want about ReadTimeout %v", ended, timeout855)
			}
		})
	}
}

// TestSSEEndedStreamKeepsConnectionTimeouts855 (std): the deadlines Detach
// cleared are net/http's for that one request, which it arms again for the
// next on a kept-alive connection. After a stream ended by the server, a
// stalled response on the same connection is cut at WriteTimeout again.
func TestSSEEndedStreamKeepsConnectionTimeouts855(t *testing.T) {
	cfg := armBoth855.config(celeris.Std, false)
	addr := startServerConfig761(t, cfg, func(s *celeris.Server) {
		events855(s, 8) // 8 x 200 ms: past both timeouts, then the server ends it
		big855Route(s)
	})
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := conn.Write([]byte("GET /events HTTP/1.1\r\nHost: " + addr + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("reading the ended stream: %v (%d bytes)", err, len(body))
	}
	if n := strings.Count(string(body), "data:"); n != 8 {
		t.Fatalf("the stream delivered %d of 8 events: %q", n, body)
	}
	if _, err := conn.Write([]byte("GET /big HTTP/1.1\r\nHost: " + addr + "\r\n\r\n")); err != nil {
		t.Fatalf("the connection did not survive the stream: %v", err)
	}
	time.Sleep(3 * timeout855)
	cr := &countingReader761{r: br}
	_, err = io.Copy(io.Discard, cr)
	t.Logf("SSE855REARM got=%d of %d bytes err=%v", cr.n, big855, err)
	if cr.n >= big855 {
		t.Fatalf("the next response on the connection was not bounded by WriteTimeout %v", timeout855)
	}
}

// TestUndetachedStreamStillCutAtWriteTimeoutStd855 (std): a streaming body
// that is not a detached stream is still a response with a WriteTimeout, as
// the docs say of long-lived responses other than SSE: it is cut at 1 s, not
// kept for the 4.5 s the handler would stream.
func TestUndetachedStreamStillCutAtWriteTimeoutStd855(t *testing.T) {
	addr := startServerConfig761(t, armWrite855.config(celeris.Std, false), func(s *celeris.Server) {
		s.GET("/chunks", func(c *celeris.Context) error {
			sw := c.StreamWriter()
			if sw == nil {
				return errors.New("no StreamWriter")
			}
			if err := sw.WriteHeader(http.StatusOK, [][2]string{{"content-type", "text/plain"}}); err != nil {
				return err
			}
			tk := time.NewTicker(tick855)
			defer tk.Stop()
			begin := time.Now()
			for time.Since(begin) < 2*hold855 { // outlasts what the client waits for
				<-tk.C
				if _, err := sw.Write([]byte("data: x\n\n")); err != nil {
					return nil
				}
				_ = sw.Flush()
			}
			return sw.Close()
		})
	})
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(hold855 + 10*time.Second))
	if _, err := conn.Write([]byte("GET /chunks HTTP/1.1\r\nHost: " + addr + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	events, _, rerr := readEvents855(conn, hold855)
	ended := time.Since(start)
	t.Logf("SSE855STREAM events=%d ended=%v err=%v", events, ended.Round(time.Millisecond), rerr)
	if rerr == nil {
		t.Fatalf("a streaming body that is not detached ran %v past a WriteTimeout of %v", ended.Round(time.Millisecond), timeout855)
	}
}

// TestHijackedConnNotCutByServerTimeoutsStd855 (std): net/http clears the
// deadlines of a connection it hands over, so a hijacked connection (the
// WebSocket path) outlives ReadTimeout and WriteTimeout. Pinned so the SSE
// change is not mistaken for the thing that keeps it open.
func TestHijackedConnNotCutByServerTimeoutsStd855(t *testing.T) {
	addr := startServerConfig761(t, armBoth855.config(celeris.Std, false), func(s *celeris.Server) {
		s.GET("/hijack", func(c *celeris.Context) error {
			conn, err := c.Hijack()
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n"))
			time.Sleep(3 * timeout855)
			_, err = conn.Write([]byte("late"))
			return err
		})
	})
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := conn.Write([]byte("GET /hijack HTTP/1.1\r\nHost: " + addr + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	all, err := io.ReadAll(conn)
	if err != nil || !strings.HasSuffix(string(all), "late") {
		t.Fatalf("the hijacked connection was cut before its write at %v: got %q, err %v", 3*timeout855, all, err)
	}
}
