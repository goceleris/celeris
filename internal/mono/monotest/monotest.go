// Package monotest is the stepped-clock scenarios of celeris#1178 that both
// native engines run against themselves. It is imported by the engines' tests
// only.
//
// The clock is internal/mono's, with a stair on its wall reading: it steps by
// 75 s every 50 ms, forward or backward, cumulatively, while its monotonic
// reading runs as it does. A timeout that is read from the wall clock is crossed
// within the first 50 ms (forward) or put out of reach (backward); one that is
// read from the clock of internal/mono, once it is monotonic, does not notice. So
// every scenario asserts BOTH bounds of what it waits for: not cut early, and not
// held past its time. "Not cut early" alone is passed by a timeout that never
// fires, which is what a clock on the wrong scale does.
package monotest

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/mono"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// Start starts an engine of the package under test with cfg (Addr is empty: the
// engine picks its own port) and h, waits until it listens and returns its
// address. The engine is stopped by a cleanup the Start registered.
type Start func(t *testing.T, cfg resource.Config, h stream.Handler) (addr string)

const (
	step  = 75 * time.Second
	every = 50 * time.Millisecond
)

// bigBody is the size of the response of /big: more than the kernel buffers of a
// loopback connection hold (the sending arm of the sweep has something to judge).
const bigBody = 32 << 20

type handler struct{ big []byte }

func (h handler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := []byte("ok")
	if s.Path == "/big" {
		body = h.big
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "application/octet-stream"}, {"content-length", strconv.Itoa(len(body))}}, body)
}

// Config is the config every scenario starts from.
func Config() resource.Config {
	return resource.Config{
		Protocol:  engine.Auto,
		Resources: resource.Resources{Workers: 2, SocketSend: 16 << 10},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// Run runs the scenarios against the engine start starts, once with the wall
// reading of the clock stepping forward and once backward.
func Run(t *testing.T, start Start) {
	t.Helper()
	big := make([]byte, bigBody)
	for _, dir := range []struct {
		name string
		step time.Duration
	}{{"forward", step}, {"backward", -step}} {
		t.Run(dir.name, func(t *testing.T) {
			run := func(name string, f func(t *testing.T)) {
				t.Run(name, func(t *testing.T) {
					defer mono.SetStair(dir.step, every)()
					f(t)
				})
			}
			run("IdleKeepAliveIsReapedOnTime", func(t *testing.T) { idleKeepAlive(t, start) })
			run("StalledBodyIsCutAtReadTimeout", func(t *testing.T) { stalledBody(t, start) })
			run("SlowHeaderIsCutAtReadHeaderTimeout", func(t *testing.T) { slowHeader(t, start) })
			run("SilentConnIsCutAtReadHeaderTimeout", func(t *testing.T) { silentConn(t, start) })
			run("H2RequestIsCutAtReadTimeout", func(t *testing.T) { h2Request(t, start) })
			run("SlowReaderGetsTheWholeResponse", func(t *testing.T) { slowReader(t, start, big) })
			run("NonReadingPeerIsCutAtWriteTimeout", func(t *testing.T) { nonReading(t, start, big) })
			if dir.step > 0 {
				run("SlowUploadIsNotCut", func(t *testing.T) { slowUpload(t, start) })
			}
		})
	}
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// closedAfter waits for the server to close c, up to max, and returns how long it
// took and whether it did. Whatever the server sends meanwhile is read and dropped.
func closedAfter(c net.Conn, t0 time.Time, max time.Duration) (time.Duration, bool) {
	buf := make([]byte, 4096)
	_ = c.SetReadDeadline(t0.Add(max))
	for {
		if _, err := c.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return time.Since(t0), false
			}
			return time.Since(t0), true
		}
	}
}

// expect asserts the server closed c within [lo, hi] of t0.
func expect(t *testing.T, what string, c net.Conn, t0 time.Time, lo, hi time.Duration) {
	t.Helper()
	d, closed := closedAfter(c, t0, hi+time.Second)
	switch {
	case !closed:
		t.Errorf("%s: still open %v after the start, want it closed by %v (a timeout held past its time)", what, d.Round(time.Millisecond), hi)
	case d < lo:
		t.Errorf("%s: closed after %v, want no sooner than %v (a timeout that fired early)", what, d.Round(time.Millisecond), lo)
	case d > hi:
		t.Errorf("%s: closed after %v, want by %v (a timeout held past its time)", what, d.Round(time.Millisecond), hi)
	}
}

// An idle keep-alive connection is reaped at IdleTimeout (1 s), not at the first
// sweep after a forward step, nor never after a backward one.
func idleKeepAlive(t *testing.T, start Start) {
	cfg := Config()
	cfg.IdleTimeout = time.Second
	addr := start(t, cfg, handler{})
	c := dial(t, addr)
	br := bufio.NewReader(c)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := readResponse(br, 2); err != nil {
		t.Fatalf("first response: %v", err)
	}
	expect(t, "idle keep-alive conn (IdleTimeout 1 s)", c, time.Now(), 600*time.Millisecond, 7*time.Second)
}

// readResponse reads one response with a body of n bytes, all of it.
func readResponse(br *bufio.Reader, n int) error {
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.ContentLength != int64(n) {
		return fmt.Errorf("Content-Length %d, want %d", resp.ContentLength, n)
	}
	got, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return fmt.Errorf("read %d of %d bytes: %w", got, n, err)
	}
	return nil
}

// A request body that stops arriving is cut at ReadTimeout (2 s) from the last
// byte, not at the first sweep after a step and not never.
func stalledBody(t *testing.T, start Start) {
	cfg := Config()
	cfg.ReadTimeout = 2 * time.Second
	addr := start(t, cfg, handler{})
	c := dial(t, addr)
	if _, err := fmt.Fprint(c, "POST /up HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n"+string(make([]byte, 100))); err != nil {
		t.Fatal(err)
	}
	expect(t, "stalled upload (ReadTimeout 2 s)", c, time.Now(), 1300*time.Millisecond, 9*time.Second)
}

// A body that keeps arriving for longer than a sweep is not cut: ReadTimeout is
// the time since the last byte.
func slowUpload(t *testing.T, start Start) {
	cfg := Config()
	cfg.ReadTimeout = 2 * time.Second
	addr := start(t, cfg, handler{})
	c := dial(t, addr)
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := fmt.Fprint(c, "POST /up HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := c.Write(make([]byte, 100)); err != nil {
			t.Fatalf("the server cut a body that was arriving: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := readResponse(bufio.NewReader(c), 2); err != nil {
		t.Fatalf("the response of a slow upload: %v", err)
	}
}

// A header that does not complete is cut at ReadHeaderTimeout (1 s) from the first
// byte: the slowloris defence.
func slowHeader(t *testing.T, start Start) {
	cfg := Config()
	cfg.ReadHeaderTimeout = time.Second
	addr := start(t, cfg, handler{})
	c := dial(t, addr)
	if _, err := fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\nX-Slow: "); err != nil {
		t.Fatal(err)
	}
	expect(t, "unfinished header (ReadHeaderTimeout 1 s)", c, time.Now(), 600*time.Millisecond, 7*time.Second)
}

// A connection that sends nothing (the protocol is not decided yet) is cut at the
// same deadline.
func silentConn(t *testing.T, start Start) {
	cfg := Config()
	cfg.ReadHeaderTimeout = time.Second
	addr := start(t, cfg, handler{})
	c := dial(t, addr)
	expect(t, "silent conn (ReadHeaderTimeout 1 s)", c, time.Now(), 600*time.Millisecond, 7*time.Second)
}

// An HTTP/2 request that is not whole (HEADERS without END_STREAM) is answered and
// reset at ReadTimeout (1 s): the stream deadline of the sweep.
func h2Request(t *testing.T, start Start) {
	cfg := Config()
	cfg.ReadTimeout = time.Second
	addr := start(t, cfg, handler{})
	c := dial(t, addr)
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(c, c)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "POST"}, {":scheme", "http"}, {":path", "/u"}, {":authority", "x"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	t0 := time.Now()
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
	for {
		_ = c.SetReadDeadline(t0.Add(8 * time.Second))
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("no answer to a request past ReadTimeout (1 s) after %v: %v (a deadline held past its time)", time.Since(t0).Round(time.Millisecond), err)
		}
		if f.Header().StreamID != 1 {
			continue
		}
		switch f.(type) {
		case *http2.HeadersFrame, *http2.RSTStreamFrame:
			if d := time.Since(t0); d < 600*time.Millisecond {
				t.Fatalf("the request was answered after %v, want no sooner than 600ms (a deadline that fired early)", d.Round(time.Millisecond))
			}
			return
		}
	}
}

// A peer that reads nothing for 3 s and then reads it all gets the whole response
// (WriteTimeout is 60 s): a step is not 60 s without progress.
func slowReader(t *testing.T, start Start, big []byte) {
	addr := start(t, Config(), handler{big: big})
	c := dial(t, addr)
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := fmt.Fprint(c, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	br := bufio.NewReaderSize(c, 1<<20)
	if err := readResponse(br, len(big)); err != nil {
		t.Fatalf("a response to a reader that waited 3 s: %v", err)
	}
}

// The positive control: with WriteTimeout 2 s a peer that reads nothing for 9 s is
// cut, so it does not get the whole response when it reads. A write clock held
// past its time (a backward step) delivers it all.
func nonReading(t *testing.T, start Start, big []byte) {
	cfg := Config()
	cfg.WriteTimeout = 2 * time.Second
	addr := start(t, cfg, handler{big: big})
	c := dial(t, addr)
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := fmt.Fprint(c, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(9 * time.Second)
	br := bufio.NewReaderSize(c, 1<<20)
	if err := readResponse(br, len(big)); err == nil {
		t.Fatal("a peer that read nothing for 9 s with WriteTimeout 2 s got the whole response: the write clock was held past its time")
	}
}
