//go:build linux

package celeris_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris"
)

// The contract of celeris#884 / celeris#885 for every engine: once Shutdown has
// begun, EVERY HTTP/1 response carries exactly one Connection: close and its
// conn is closed after it, as net/http's response written after Shutdown began
// does (its Shutdown turns keep-alives off). Before, only a response the gate
// (the first 250 ms past the start of the wait, celeris#885 item 49) had
// marked did: a conn busy when Shutdown began, or an idle one that a request
// reached in those 250 ms, answered keep-alive and stayed open.
//
// Two kinds of response are checked, on the wire, per shape (variants884):
//
//   - in flight: the request reached its handler before Shutdown began, and the
//     handler is released only once the shutdown has been seen from outside (an
//     HTTP/2 holder conn read its GOAWAY, which every engine sends at the start
//     of its drain, after Engine.Shutdown has run). The response is written
//     after the start of the shutdown. All engines, std included: this is the
//     one contract of the three.
//   - in the window: an idle keep-alive conn that is sent a request right after
//     that GOAWAY, inside the 250 ms before the gate. Native engines only (std
//     closes its idle conns at once); the wait is held open for the window by
//     HTTP/2 conns whose responses nobody gives window to ("pool-wait"). In
//     "direct" mode the holders are idle h2 conns: a loop with nothing to wait
//     for shuts down at once, and what is checked is the in-flight kind alone.
//
// On main every response of both kinds that the gate had not marked answers
// keep-alive and its conn stays open.

type ccVariant884 struct {
	name string
	req  string // %s: the path
	head bool   // a HEAD request: the response has no body
	// h is the handler's reply (after any gate).
	h func(c *celeris.Context) error
	// http10: the request is HTTP/1.0. net/http ends its conn after the
	// response, as for any request, but sends no Connection header to an
	// HTTP/1.0 client (it adds one only when the request is HTTP/1.1 or later);
	// the native engines say close to every request.
	http10 bool
	// wantBody > 0: the body must arrive whole (this many bytes, no read error)
	// and the conn then end with an EOF, not a reset: close(2) with unread bytes
	// in the receive queue sends an RST that destroys the response still in
	// flight, so a closing conn that a late request reached must have those
	// bytes read and dropped before it closes.
	wantBody int
}

const ccGet884 = "GET %s HTTP/1.1\r\nHost: x\r\n\r\n"

func variants884() []ccVariant884 {
	ok := func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") }
	return []ccVariant884{
		{name: "get", req: ccGet884, h: ok},
		{name: "head", req: "HEAD %s HTTP/1.1\r\nHost: x\r\n\r\n", head: true, h: ok},
		{name: "no-content-204", req: ccGet884, h: func(c *celeris.Context) error { return c.NoContent(http.StatusNoContent) }},
		{name: "not-modified-304", req: ccGet884, h: func(c *celeris.Context) error { return c.NoContent(http.StatusNotModified) }},
		{name: "handler-set-close", req: ccGet884, h: func(c *celeris.Context) error {
			c.SetHeader("connection", "close")
			return c.String(http.StatusOK, "ok")
		}},
		{name: "handler-set-keep-alive", req: ccGet884, h: func(c *celeris.Context) error {
			c.SetHeader("connection", "keep-alive")
			return c.String(http.StatusOK, "ok")
		}},
		{name: "request-keep-alive", req: "GET %s HTTP/1.1\r\nHost: x\r\nConnection: keep-alive\r\n\r\n", h: ok},
		{name: "http10", req: "GET %s HTTP/1.0\r\nHost: x\r\n\r\n", h: ok, http10: true},
		{name: "http10-keep-alive", req: "GET %s HTTP/1.0\r\nHost: x\r\nConnection: keep-alive\r\n\r\n", h: ok, http10: true},
		{name: "chunked-stream", req: ccGet884, h: func(c *celeris.Context) error {
			sw := c.StreamWriter()
			if sw == nil {
				return errors.New("no stream writer")
			}
			if err := sw.WriteHeader(http.StatusOK, [][2]string{{"content-type", "text/plain"}}); err != nil {
				return err
			}
			if _, err := sw.Write([]byte("ok")); err != nil {
				return err
			}
			return sw.Close()
		}},
		{name: "pipelined", req: "GET %s HTTP/1.1\r\nHost: x\r\n\r\nGET /v/second HTTP/1.1\r\nHost: x\r\n\r\n", h: ok},
	}
}

// ccHolder884 is an HTTP/2 conn that tells the test the shutdown has begun.
type ccHolder884 struct {
	c  net.Conn
	fr *http2.Framer
}

// awaitGoAway884 returns once one of the holders has read a GOAWAY, the proof
// that Engine.Shutdown has run (it precedes the cancel that starts every
// engine's drain, and the GOAWAY is sent by the drain). The reads go on in the
// background until the conns are closed.
//
// With orAfter > 0 the wait is also over after that long, GOAWAY or not: epoll's
// loop that has nothing to wait for shuts down at once, and its shutdown joins
// the async handlers (phase 2) before it flushes the GOAWAY, so while those
// are gated the GOAWAY cannot be the barrier. Engine.Shutdown has long run by
// then: it is the first thing Server.Shutdown does.
func awaitGoAway884(t *testing.T, holders []ccHolder884, limit, orAfter time.Duration) {
	t.Helper()
	got := make(chan struct{}, len(holders))
	for _, h := range holders {
		go func() {
			for {
				_ = h.c.SetReadDeadline(time.Now().Add(limit))
				f, err := h.fr.ReadFrame()
				if err != nil {
					return
				}
				if _, ok := f.(*http2.GoAwayFrame); ok {
					got <- struct{}{}
					return
				}
			}
		}()
	}
	var soft <-chan time.Time
	if orAfter > 0 {
		soft = time.After(orAfter)
	}
	select {
	case <-got:
	case <-soft:
	case <-time.After(limit):
		t.Fatalf("no GOAWAY on any of %d HTTP/2 conns within %v of the shutdown: the drain did not begin", len(holders), limit)
	}
}

// readHead884 reads the response head off br, up to and including the blank
// line, as sent: http.ReadResponse removes a "Connection: close" line from
// resp.Header (it reports it as resp.Close), so the lines are counted here.
func readHead884(br *bufio.Reader) (string, error) {
	var head strings.Builder
	for {
		line, err := br.ReadString('\n')
		head.WriteString(line)
		if err != nil {
			return head.String(), err
		}
		if line == "\r\n" {
			return head.String(), nil
		}
	}
}

// wantClosed884 checks one response and the conn after it: exactly one
// Connection header line, close, as net/http's response written after Shutdown
// began has (all three engines; std: but for an HTTP/1.0 request, see
// ccVariant884), and then nothing more to read from the conn:
// it is closed, and nothing pipelined behind the request is answered.
func wantClosed884(t *testing.T, name string, c net.Conn, br *bufio.Reader, v ccVariant884, std bool) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	head, err := readHead884(br)
	if err != nil {
		t.Errorf("%s: the response head: %v after %q (a request in flight at the start of the shutdown must be answered)", name, err, head)
		return
	}
	var lines []string
	for _, l := range strings.Split(head, "\r\n") {
		if len(l) >= 11 && strings.EqualFold(l[:11], "connection:") {
			lines = append(lines, l)
		}
	}
	if std && v.http10 {
		if len(lines) > 1 {
			t.Errorf("%s: Connection lines %q in the head", name, lines)
		}
	} else if len(lines) != 1 || !strings.EqualFold(strings.TrimSpace(lines[0][len("connection:"):]), "close") {
		t.Errorf("%s: Connection lines %q in the head, want exactly [connection: close]: a response written after the start of the shutdown must say it ends the conn", name, lines)
	}
	// br may have buffered past the head: parse from the head and what follows.
	rd := bufio.NewReader(io.MultiReader(strings.NewReader(head), br))
	req := &http.Request{Method: "GET"}
	if v.head {
		req.Method = "HEAD"
	}
	resp, err := http.ReadResponse(rd, req)
	if err != nil {
		t.Errorf("%s: the response: %v", name, err)
		return
	}
	if !resp.Close {
		t.Errorf("%s: the response does not say it closes the conn", name)
	}
	body, rerr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if v.wantBody > 0 && (rerr != nil || len(body) != v.wantBody) {
		t.Errorf("%s: the body is %d of %d bytes (%v): the close cut the response", name, len(body), v.wantBody, rerr)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := rd.Read(make([]byte, 1))
	if v.wantBody > 0 && !errors.Is(err, io.EOF) {
		t.Errorf("%s: after the whole body the conn read %v, want io.EOF (a reset is the close of a conn with unread bytes)", name, err)
	}
	if n != 0 || err == nil {
		t.Errorf("%s: read %d byte(s), %v after the response, want the conn closed with nothing more to read", name, n, err)
	} else if !errors.Is(err, io.EOF) && !isReset884(err) {
		t.Errorf("%s: after the response the conn read %v, want a close (EOF)", name, err)
	}
}

func isReset884(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection reset")
}

type ccArm884 struct {
	name   string
	eng    celeris.EngineType
	setup  func(*testing.T)
	mode   string // Shutdown mode of h2PoolServer759
	async  bool   // the server has Async routes (conns can be promoted)
	direct bool   // idle h2 holders: a loop shuts down at once (no window)
	std    bool   // net/http: the in-flight kind only (it closes its idle conns at once)
}

func TestShutdownConnCloseContract884(t *testing.T) {
	noEnv := func(*testing.T) {}
	adaptiveStart := func(start string) func(*testing.T) {
		return func(t *testing.T) { t.Setenv("CELERIS_ADAPTIVE_START", start) }
	}
	var arms []ccArm884
	arms = append(arms, ccArm884{name: "std/async-route", eng: celeris.Std, setup: noEnv, mode: "Shutdown", async: true, std: true})
	arms = append(arms, ccArm884{name: "std/sync", eng: celeris.Std, setup: noEnv, mode: "Shutdown", std: true})
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
		set  func(*testing.T)
		mode string
	}{
		{"epoll", celeris.Epoll, noEnv, "Shutdown"},
		{"io_uring", celeris.IOUring, noEnv, "Shutdown"},
		{"adaptive-epoll", celeris.Adaptive, adaptiveStart("epoll"), "cancel"},
		{"adaptive-iouring", celeris.Adaptive, adaptiveStart("iouring"), "cancel"},
	} {
		for _, async := range []bool{false, true} {
			kind := "sync"
			if async {
				kind = "async-route"
			}
			arms = append(arms, ccArm884{name: e.name + "/pool-wait/" + kind, eng: e.eng, setup: e.set, mode: e.mode, async: async})
		}
		if e.eng == celeris.Epoll || e.eng == celeris.IOUring {
			for _, async := range []bool{false, true} {
				kind := "sync"
				if async {
					kind = "async-route"
				}
				arms = append(arms, ccArm884{name: e.name + "/direct/" + kind, eng: e.eng, setup: e.set, mode: e.mode, async: async, direct: true})
			}
		}
	}
	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			a.setup(t)
			runConnCloseArm884(t, a)
		})
	}
}

func runConnCloseArm884(t *testing.T, a ccArm884) {
	var second, entered atomic.Int64
	release := make(chan struct{})
	vs := variants884()
	bigBody := strings.Repeat("h", 4<<20)

	srv := startH2PoolServer759(t, a.eng, 20*time.Second, func(s *celeris.Server) {
		s.GET("/big", func(c *celeris.Context) error { return c.String(http.StatusOK, "%s", bigBody) })
		s.GET("/v/second", func(c *celeris.Context) error { second.Add(1); return c.String(http.StatusOK, "second") })
		// /hold is a gated SYNC route: on the native engines its handler runs on
		// the loop's own thread, so the loop is busy when Shutdown begins.
		s.GET("/hold", func(c *celeris.Context) error {
			entered.Add(1)
			<-release
			return c.String(http.StatusOK, "ok")
		})
		// The late-pipelined shape (the review of celeris#1169): a response large
		// enough to be still flushing when the engine's gate makes its first
		// pass, whose conn then has a second request in the kernel's receive
		// queue that reached it after the first was read.
		bigResp := strings.Repeat("L", 16<<20)
		s.GET("/holdbig", func(c *celeris.Context) error {
			entered.Add(1)
			<-release
			return c.String(http.StatusOK, "%s", bigResp)
		})
		if a.async {
			s.GET("/g/bighold", func(c *celeris.Context) error {
				entered.Add(1)
				<-release
				return c.String(http.StatusOK, "%s", bigResp)
			}).Async()
		}
		for _, v := range vs {
			h := v.h
			register := func(path string, async, gated bool) {
				fn := func(c *celeris.Context) error {
					if gated {
						entered.Add(1)
						<-release
					}
					return h(c)
				}
				method := "GET"
				if v.head {
					method = "HEAD"
				}
				r := s.Handle(method, path, fn)
				if async {
					r.Async()
				}
			}
			register("/v/"+v.name, false, false)
			if a.async {
				register("/av/"+v.name, true, false)
				register("/g/"+v.name, true, true)
			}
		}
	})

	// The holders: what keeps each loop's wait open (4 MiB of HTTP/2 response
	// nobody gives window to), or, in direct mode, idle h2 conns that only
	// report the shutdown.
	nHold := 48
	if a.direct || a.std {
		nHold = 6
	}
	var holders []ccHolder884
	for range nHold {
		c, fr := dialH2759(t, srv.addr)
		holders = append(holders, ccHolder884{c, fr})
		if !a.direct && !a.std {
			writeH2Get759(t, fr, 1, "/big")
		}
	}
	time.Sleep(300 * time.Millisecond)

	dial := func() (net.Conn, *bufio.Reader) {
		c, err := net.Dial("tcp", srv.addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c, bufio.NewReader(c)
	}
	send := func(c net.Conn, raw string) {
		t.Helper()
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(c, raw); err != nil {
			t.Fatalf("write %q: %v", strings.SplitN(raw, "\r\n", 2)[0], err)
		}
	}

	// Idle keep-alive conns for the window: one request each before the
	// shutdown, which must be answered keep-alive (the control).
	type idle struct {
		v    ccVariant884
		path string
		c    net.Conn
		br   *bufio.Reader
	}
	var idles []idle
	if !a.direct && !a.std {
		for _, v := range vs {
			paths := []string{"/v/" + v.name}
			if a.async {
				paths = append(paths, "/av/"+v.name)
			}
			for _, p := range paths {
				c, br := dial()
				send(c, "GET /ping HTTP/1.1\r\nHost: x\r\n\r\n")
				_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatalf("/ping before the shutdown: %v", err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				if resp.Close || len(resp.Header.Values("Connection")) != 0 {
					t.Fatalf("control: a response before the shutdown says Connection %q", resp.Header.Values("Connection"))
				}
				idles = append(idles, idle{v, p, c, br})
			}
		}
	}

	// In flight: every variant's request is in its handler (async routes), and
	// a sync handler holds its loop's thread.
	type flight struct {
		name string
		v    ccVariant884
		c    net.Conn
		br   *bufio.Reader
	}
	var flights []flight
	want := int64(0)
	if a.async {
		for _, v := range vs {
			if a.eng == celeris.Epoll && a.direct && v.name == "chunked-stream" {
				// epoll's shutdown with nothing to wait for ends a stream
				// writer on an Async route in its phase 1 (detachClosed: its
				// writes are no-ops from there), so a stream still in the
				// handler when that loop shuts down has no response to close.
				// Not this contract: the stream is ended by the shutdown itself.
				continue
			}
			c, br := dial()
			path := "/g/" + v.name
			send(c, fmt.Sprintf(v.req, path))
			flights = append(flights, flight{"in-flight " + path, v, c, br})
			want++
		}
	}
	// The late-pipelined flights: a large response, read slowly (a small receive
	// buffer, read after the gate), and a second request written once the first
	// is in its handler. On a server with no Async route the one gated sync
	// handler is this flight (a second sync handler could land on the loop the
	// first holds).
	var lates []flight
	late := func(path string) {
		c := dialSmallRcvbuf884(t, srv.addr)
		send(c, "GET "+path+" HTTP/1.1\r\nHost: x\r\n\r\n")
		// Whole-body strictness where main delivers the whole response: a loop
		// that waits (the gate's first pass finds the closing conn with the late
		// request in its queue). A loop that shuts down at once, and net/http,
		// reset such a conn on close, as main does (not this change).
		wb := 0
		if !a.direct && !a.std {
			wb = 16 << 20
		}
		lates = append(lates, flight{"late-pipelined " + path, ccVariant884{name: "get", wantBody: wb}, c, bufio.NewReader(c)})
		want++
	}
	if a.async {
		// Before the sync /hold: a request sent after it could land behind it on
		// the loop it holds.
		late("/g/bighold")
		c, br := dial()
		send(c, "GET /hold HTTP/1.1\r\nHost: x\r\n\r\n")
		flights = append(flights, flight{"in-flight sync /hold", ccVariant884{name: "hold", req: ccGet884}, c, br})
		want++
	} else {
		late("/holdbig")
	}
	for deadline := time.Now().Add(10 * time.Second); entered.Load() < want; {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d gated handlers entered within 10s", entered.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The second request of each late flight reaches the kernel now, after the
	// first was read and while its handler runs.
	for _, f := range lates {
		send(f.c, "GET /v/second HTTP/1.1\r\nHost: x\r\n\r\n")
	}

	srv.beginShutdown(a.mode, 20*time.Second)
	// The soft barrier: in direct mode (see awaitGoAway884) and whenever a sync
	// handler holds a loop's thread, since a ring that is the only worker the
	// memlock limit allowed (CI's coverage job) is then blocked and cannot send
	// the GOAWAY before the handler is released.
	orAfter := time.Second
	if a.direct {
		orAfter = 500 * time.Millisecond
	}
	awaitGoAway884(t, holders, 10*time.Second, orAfter)

	// The window (native, with the wait held): requests on idle conns, sent at
	// once; and the busy handlers released.
	var wg sync.WaitGroup
	for _, in := range idles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = in.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(in.c, fmt.Sprintf(in.v.req, in.path)); err != nil {
				t.Errorf("window %s: write: %v", in.path, err)
				return
			}
			wantClosed884(t, "window "+in.path, in.c, in.br, in.v, false)
		}()
	}
	close(release)
	for _, f := range flights {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wantClosed884(t, f.name, f.c, f.br, f.v, a.std)
		}()
	}
	for _, f := range lates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Read only after the gate's first pass (250 ms into the wait): the
			// response is then still flushing, to a client that has taken 32 KiB.
			time.Sleep(700 * time.Millisecond)
			wantClosed884(t, f.name, f.c, f.br, f.v, a.std)
		}()
	}
	wg.Wait()
	if n := second.Load(); n != 0 {
		t.Errorf("/v/second was served %d time(s): a pipelined request was answered after the response that closes the conn", n)
	}

	for _, h := range holders {
		_ = h.c.Close()
	}
	srv.waitStart(t, 25*time.Second)
}
