//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestIOUringShutdownGateConns884 pins what the io_uring engine does with the
// HTTP/1 conns it stops reading once its shutdown has passed the gate
// (celeris#884 item 73, the io_uring twin of
// celeris#885 item 49 / PR #1116 on epoll). The gate is the first 250 ms of
// the wait for HTTP/2 pool handlers ("pool-wait") or of the send drain
// ("send-drain"); it is held open for the whole 20 s budget by clients that
// take nothing, so every assertion below is about the gate, not about the end
// of the wait (which closes everything). Per arm:
//
//   - an idle keep-alive conn, a conn that was accepted and sent nothing (a
//     browser's preconnect), and, on a server with an Async route, an idle conn
//     that route promoted to a dispatch goroutine each get a FIN (EOF, not a
//     reset, not silence) at once, as std's Shutdown closes its idle conns;
//   - a detached stream (Server-Sent Events) the gate ended receives what was
//     queued for it, whole, and then a FIN, on a sync route and on an Async one
//     (a promoted conn: epoll's r3 M1);
//   - a request in progress when the shutdown begins (a 2 MiB POST of which
//     64 KiB has arrived, the same on an Async route, a GET whose headers are
//     in pieces) is read on and answered, as std's Shutdown answers the request
//     of an active conn, with Connection: close, and its conn is then closed;
//   - a 16 MiB download queued for a client that reads slowly arrives whole and
//     its conn is then closed.
//
// On main the gate reads nothing past it and closes nothing: every FIN and every
// answer above is missing until the end of the wait.
func TestIOUringShutdownGateConns884(t *testing.T) {
	type arm struct {
		name  string
		eng   celeris.EngineType
		setup func(*testing.T)
		gate  string // "pool-wait" or "send-drain"
		async bool   // a server with an Async route, whose conns can be promoted
	}
	noEnv := func(*testing.T) {}
	var arms []arm
	for _, gate := range []string{"pool-wait", "send-drain"} {
		for _, async := range []bool{false, true} {
			name := gate + "/sync"
			if async {
				name = gate + "/async-route"
			}
			arms = append(arms, arm{name, celeris.IOUring, noEnv, gate, async})
		}
	}
	arms = append(arms, arm{"adaptive-iouring/pool-wait/sync", celeris.Adaptive,
		func(t *testing.T) { t.Setenv("CELERIS_ADAPTIVE_START", "iouring") }, "pool-wait", false})

	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			a.setup(t)
			runIOUringGateArm884(t, a.eng, a.gate, a.async)
		})
	}
}

func runIOUringGateArm884(t *testing.T, eng celeris.EngineType, gate string, async bool) {
	const (
		nBulk, chunk = 4, 128 << 10
		upSize       = 2 << 20
		upFirst      = 64 << 10
		holdSize     = 4 << 20
		nHold        = 48
	)
	dlBody := strings.Repeat("d", 16<<20)
	holdBody := strings.Repeat("h", holdSize)
	var okBytes [nBulk]atomic.Int64
	srv := startH2PoolServer759(t, eng, 20*time.Second, func(s *celeris.Server) {
		s.GET("/big", func(c *celeris.Context) error { return c.String(http.StatusOK, "%s", holdBody) })
		s.GET("/dl", func(c *celeris.Context) error { return c.String(http.StatusOK, "%s", dlBody) })
		s.POST("/up", func(c *celeris.Context) error { return c.String(http.StatusOK, "got %d", len(c.Body())) })
		bulk := s.GET("/bulk/:i", func(c *celeris.Context) error {
			i, convErr := strconv.Atoi(c.Param("i"))
			done := c.Detach()
			sw := c.StreamWriter()
			if sw == nil || convErr != nil || i < 0 || i >= nBulk {
				done()
				return errors.New("no stream writer")
			}
			if err := sw.WriteHeader(http.StatusOK, [][2]string{{"content-type", "application/octet-stream"}}); err != nil {
				done()
				return err
			}
			go func() {
				defer done()
				buf := bytes.Repeat([]byte{'x'}, chunk)
				for {
					if _, err := sw.Write(buf); err != nil {
						return
					}
					okBytes[i].Add(chunk)
					time.Sleep(10 * time.Millisecond)
				}
			}()
			return nil
		})
		if async {
			bulk.Async()
			s.GET("/aping", func(c *celeris.Context) error { return c.String(http.StatusOK, "a") }).Async()
			s.POST("/aup", func(c *celeris.Context) error { return c.String(http.StatusOK, "got %d", len(c.Body())) }).Async()
		}
	})

	// What holds the gate open: HTTP/2 responses nobody gives window to (the
	// pool wait), or HTTP/1 responses queued for clients that read nothing (the
	// send drain). Enough of them that every worker has some.
	var holders []net.Conn
	switch gate {
	case "pool-wait":
		for range nHold {
			c, fr := dialH2759(t, srv.addr)
			holders = append(holders, c)
			writeH2Get759(t, fr, 1, "/big")
		}
	case "send-drain":
		for range nHold {
			c := dialSmallRcvbuf884(t, srv.addr)
			holders = append(holders, c)
			if _, err := io.WriteString(c, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
		}
	}
	time.Sleep(300 * time.Millisecond)

	keepAlive := func(path string) (net.Conn, *bufio.Reader) {
		c, err := net.Dial("tcp", srv.addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		br := bufio.NewReader(c)
		if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("%s before the shutdown: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return c, br
	}
	idle := make([]net.Conn, 6)
	idleBr := make([]*bufio.Reader, 6)
	for i := range idle {
		idle[i], idleBr[i] = keepAlive("/ping")
	}
	silent := make([]net.Conn, 2)
	for i := range silent {
		c, err := net.Dial("tcp", srv.addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		silent[i] = c
	}
	var promoted net.Conn
	var promotedBr *bufio.Reader
	if async {
		promoted, promotedBr = keepAlive("/aping")
	}

	// Detached streams to clients that read nothing until later.
	bulk := make([]net.Conn, nBulk)
	for i := range bulk {
		c, err := net.Dial("tcp", srv.addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		bulk[i] = c
		if _, err := io.WriteString(c, fmt.Sprintf("GET /bulk/%d HTTP/1.1\r\nHost: x\r\n\r\n", i)); err != nil {
			t.Fatal(err)
		}
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		ready := true
		for i := range okBytes {
			ready = ready && okBytes[i].Load() >= 4*chunk
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the producers wrote under 1 MiB each within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A download queued in the engine for a slow reader.
	dl := dialSmallRcvbuf884(t, srv.addr)
	if _, err := io.WriteString(dl, "GET /dl HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	// Requests in progress: the head and the first 64 KiB of a 2 MiB body, on a
	// sync route and (when the server has an Async route) on an Async one, and a
	// GET whose headers are in pieces.
	type progress struct {
		name string
		c    net.Conn
		rest []byte
		want string
	}
	var inProgress []*progress
	startPost := func(name, path string) {
		c, err := net.Dial("tcp", srv.addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if _, err := io.WriteString(c, "POST "+path+" HTTP/1.1\r\nHost: x\r\nContent-Length: "+strconv.Itoa(upSize)+"\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write(make([]byte, upFirst)); err != nil {
			t.Fatal(err)
		}
		inProgress = append(inProgress, &progress{name, c, make([]byte, upSize-upFirst), "got " + strconv.Itoa(upSize)})
	}
	startPost("POST /up (sync route)", "/up")
	if async {
		startPost("POST /aup (Async route)", "/aup")
	}
	{
		c, err := net.Dial("tcp", srv.addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if _, err := io.WriteString(c, "GET /ping HTTP/1.1\r\nHost: x\r\n"); err != nil {
			t.Fatal(err)
		}
		inProgress = append(inProgress, &progress{"GET /ping with headers in pieces", c, []byte("\r\n"), "ok"})
	}

	// TCP_DEFER_ACCEPT hands the engine a conn with no byte after about a
	// second: the silent conns are in its table by then.
	time.Sleep(1300 * time.Millisecond)

	begin := time.Now()
	srv.beginShutdown("Shutdown", 20*time.Second)

	expectFIN := func(name string, c net.Conn, br *bufio.Reader) {
		t.Helper()
		_ = c.SetReadDeadline(begin.Add(2500 * time.Millisecond))
		var err error
		if br != nil {
			_, err = br.ReadByte()
		} else {
			_, err = c.Read(make([]byte, 1))
		}
		if err != io.EOF {
			t.Errorf("%s: read %v at %v, want a FIN (io.EOF) once the gate has passed: the gate left the conn open with no answer", name, err, time.Since(begin).Round(time.Millisecond))
		}
	}
	for i, c := range idle {
		expectFIN("idle keep-alive "+strconv.Itoa(i), c, idleBr[i])
	}
	for i, c := range silent {
		expectFIN("conn with no byte sent "+strconv.Itoa(i), c, nil)
	}
	if promoted != nil {
		expectFIN("idle conn on an Async route", promoted, promotedBr)
	}

	// The rest of every request in progress arrives after the gate.
	time.Sleep(time.Until(begin.Add(1200 * time.Millisecond)))
	// The download's client sends more after the gate, twice, which the engine
	// does not read (the conn is not idle, but the gate reads a request only
	// when one is in progress): bytes that stay unread in the kernel, so a close
	// of that conn while any of the response is still unacknowledged in the
	// kernel's send queue would be a reset that throws the tail of it away.
	for range 2 {
		if _, err := io.WriteString(dl, "GET /ping HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			t.Errorf("a request written on the download's conn after the gate: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	var pwg sync.WaitGroup
	for _, p := range inProgress {
		pwg.Add(1)
		go func() {
			defer pwg.Done()
			_ = p.c.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if _, err := p.c.Write(p.rest); err != nil {
				t.Errorf("%s: the rest of the request, sent %v into the shutdown: %v: a request in progress was cut", p.name, time.Since(begin).Round(time.Millisecond), err)
				return
			}
			_ = p.c.SetReadDeadline(time.Now().Add(4 * time.Second))
			br := bufio.NewReader(p.c)
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Errorf("%s: the response: %v: a request in progress when the gate came was not answered", p.name, err)
				return
			}
			b, _ := io.ReadAll(resp.Body)
			if string(b) != p.want {
				t.Errorf("%s: the response body %q, want %q", p.name, b, p.want)
			}
			if !resp.Close {
				t.Errorf("%s: the response, written after the gate, has no Connection: close", p.name)
			}
			_ = p.c.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, err := br.ReadByte(); err != io.EOF {
				t.Errorf("%s: after the response the conn read %v, want a FIN (io.EOF)", p.name, err)
			}
		}()
	}

	// What was queued for the detached streams is flushed, and then the conns
	// are closed: every stream read at once.
	type streamResult struct {
		got, ok int64
		err     error
		end     error
	}
	results := make([]streamResult, nBulk)
	var rwg sync.WaitGroup
	for i, c := range bulk {
		ok := okBytes[i].Load()
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				results[i] = streamResult{ok: ok, err: err}
				return
			}
			var got int64
			buf := make([]byte, 64<<10)
			// okBytes was read before the producers were ended: what reached
			// the engine after it may or may not go out, so the body is read
			// to its end below, and only what had been written is required.
			for got < ok {
				n, err := resp.Body.Read(buf)
				got += int64(n)
				if err != nil {
					results[i] = streamResult{got: got, ok: ok, err: err}
					return
				}
			}
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			var endErr error
			for endErr == nil {
				_, endErr = resp.Body.Read(buf)
			}
			results[i] = streamResult{got: got, ok: ok, end: endErr}
		}()
	}
	rwg.Wait()
	for i, r := range results {
		t.Logf("celeris884 %s stream %d: %d of %d bytes written before the gate were received (%v)", gate, i, r.got, r.ok, r.err)
		if r.err != nil || r.got < r.ok-chunk {
			t.Errorf("stream %d: the client received %d of the %d bytes whose writes had succeeded (%v): what was queued was not flushed after the gate", i, r.got, r.ok, r.err)
		} else if !errors.Is(r.end, io.EOF) && !errors.Is(r.end, io.ErrUnexpectedEOF) {
			t.Errorf("stream %d: after the flushed bytes the client read %v, 3 s on, want a FIN (EOF, not a reset or silence): the gate ended the stream but did not close its conn", i, r.end)
		}
	}
	pwg.Wait()

	// The queued download arrives whole, and then its conn is closed.
	_ = dl.SetReadDeadline(time.Now().Add(30 * time.Second))
	dbr := bufio.NewReader(dl)
	resp, err := http.ReadResponse(dbr, nil)
	if err != nil {
		t.Fatalf("the download's head: %v", err)
	}
	// Read slowly (about 20 MiB/s), so that the kernel's send queue (a few MiB)
	// is still draining when the engine's own queue has emptied.
	var dlN int64
	var dlErr error
	for chunk := make([]byte, 64<<10); dlErr == nil; {
		var n int
		n, dlErr = resp.Body.Read(chunk)
		dlN += int64(n)
		time.Sleep(3 * time.Millisecond)
	}
	if dlN != int64(len(dlBody)) {
		t.Errorf("the queued download delivered %d of %d bytes (%v): the gate cut a response that was still going out", dlN, len(dlBody), dlErr)
	}
	// Closed once drained: a FIN, or, because the client's two requests after
	// the gate were never read and close(2) with unread bytes resets, an RST that
	// comes after the whole response has been received (the length check above).
	// Silence is what the gate must not leave.
	_ = dl.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := dbr.ReadByte(); err != io.EOF && !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("after the download the conn read %v, want a close (FIN, or a reset after the whole response): a drained conn was left open", err)
	}

	for _, c := range holders { // the holders go: the wait ends
		_ = c.Close()
	}
	srv.waitStart(t, 25*time.Second)
}

// dialSmallRcvbuf884 opens an HTTP/1 client whose receive buffer is small, so
// a response queues in the engine instead of the kernel.
func dialSmallRcvbuf884(t *testing.T, addr string) net.Conn {
	t.Helper()
	d := net.Dialer{Control: func(_, _ string, rc syscall.RawConn) error {
		return rc.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 32<<10)
		})
	}}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
