//go:build linux

package celeris_test

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
)

// celeris#949: on the std engine an h2c request's c.Context() was
// context.Background(), so a client that reset its stream or went away never
// told the handler. The native engines (epoll, io_uring, adaptive) cancel the
// stream's context on RST_STREAM and on connection close. These tests pin
// that on every engine, over a real connection, with a raw HTTP/2 client so
// the departure is exactly the frame (or the close) the test names.

// ctxBound949 is how long a handler gets to see its context end after the
// client left. The native engines cancel within milliseconds; the bound is
// generous so a loaded CI host does not flake, and tight enough that "never"
// (the std engine before the fix: the handler gave up after ctxHold949) fails.
const (
	ctxBound949 = 5 * time.Second
	ctxHold949  = 8 * time.Second
	// h1Hold949 is how long the HTTP/1 handler waits for a cancellation that
	// is expected not to come; a departure reaches a handler in milliseconds.
	h1Hold949 = 1500 * time.Millisecond
)

type ctxOutcome949 struct {
	ended   bool          // c.Context().Done() closed
	err     error         // c.Context().Err() when it did
	at      time.Time     // when the handler saw it (or gave up)
	started time.Time     // when the handler started
	waited  time.Duration // at - started
}

// startWaitServer949 starts a server on engine e whose GET /wait handler
// signals started, then waits on c.Context() for ctxHold949 and reports what
// it saw. The route is Async so that, on the native engines, waiting does not
// hold an event loop.
func startWaitServer949(t *testing.T, e celeris.EngineType, hold time.Duration) (addr string, started chan struct{}, outcome chan ctxOutcome949) {
	t.Helper()
	started = make(chan struct{}, 16)
	outcome = make(chan ctxOutcome949, 16)
	addr = startServer761(t, e, false, func(s *celeris.Server) {
		s.GET("/wait", func(c *celeris.Context) error {
			o := ctxOutcome949{started: time.Now()}
			started <- struct{}{}
			ctx := c.Context()
			select {
			case <-ctx.Done():
				o.ended, o.err = true, ctx.Err()
			case <-time.After(hold):
			}
			o.at = time.Now()
			o.waited = o.at.Sub(o.started)
			outcome <- o
			return c.String(200, "done")
		}).Async()
	})
	return addr, started, outcome
}

// rawH2Conn949 opens a prior-knowledge h2c connection and sends GET /wait as
// stream 1. A goroutine drains the server's frames (and acknowledges its
// SETTINGS) until the connection closes.
func rawH2Conn949(t *testing.T, addr string) (net.Conn, *http2.Framer) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatalf("preface: %v", err)
	}
	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(); err != nil {
		t.Fatalf("settings: %v", err)
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", addr}, {":path", "/wait"}} {
		if err := enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		t.Fatalf("headers: %v", err)
	}
	go func() {
		rfr := http2.NewFramer(io.Discard, conn)
		for {
			f, err := rfr.ReadFrame()
			if err != nil {
				return
			}
			if sf, ok := f.(*http2.SettingsFrame); ok && !sf.IsAck() {
				_ = fr.WriteSettingsAck()
			}
		}
	}()
	return conn, fr
}

// rawUpgradeConn949 sends GET /wait as an RFC 7540 3.2 upgrade request and
// reads the 101; the request is then HTTP/2 stream 1.
func rawUpgradeConn949(t *testing.T, addr string) (net.Conn, *http2.Framer) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	req := "GET /wait HTTP/1.1\r\nHost: " + addr + "\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n" +
		"HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("upgrade request: %v", err)
	}
	// The server answers 101, then speaks HTTP/2; read the 101 head byte by
	// byte so no HTTP/2 byte is consumed.
	var head []byte
	one := make([]byte, 1)
	for !bytes.HasSuffix(head, []byte("\r\n\r\n")) {
		if _, err := conn.Read(one); err != nil {
			t.Fatalf("reading the 101: %v (read %q)", err, head)
		}
		head = append(head, one[0])
	}
	if !strings.HasPrefix(string(head), "HTTP/1.1 101") {
		t.Fatalf("upgrade answered %q, want 101", head)
	}
	_ = conn.SetDeadline(time.Time{})
	fr := http2.NewFramer(conn, conn)
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatalf("preface: %v", err)
	}
	if err := fr.WriteSettings(); err != nil {
		t.Fatalf("settings: %v", err)
	}
	go func() {
		rfr := http2.NewFramer(io.Discard, conn)
		for {
			f, err := rfr.ReadFrame()
			if err != nil {
				return
			}
			if sf, ok := f.(*http2.SettingsFrame); ok && !sf.IsAck() {
				_ = fr.WriteSettingsAck()
			}
		}
	}()
	return conn, fr
}

func awaitStarted949(t *testing.T, started chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never started")
	}
}

// expectEnded949 waits for the handler's report and fails unless its context
// ended within ctxBound949 of the departure.
func expectEnded949(t *testing.T, engine, what string, outcome chan ctxOutcome949, departed time.Time) {
	t.Helper()
	select {
	case o := <-outcome:
		lat := o.at.Sub(departed)
		t.Logf("MEASURE engine=%s departure=%s ended=%v err=%v latency=%s", engine, what, o.ended, o.err, lat.Round(time.Millisecond))
		if !o.ended {
			t.Fatalf("%s: the client left (%s) and the handler's c.Context() was still not done after %s", engine, what, o.waited)
		}
		if o.err == nil || o.err.Error() != "context canceled" {
			t.Fatalf("%s: c.Context().Err() = %v, want context canceled", engine, o.err)
		}
		if lat > ctxBound949 {
			t.Fatalf("%s: the handler saw the departure (%s) after %s, want within %s", engine, what, lat, ctxBound949)
		}
	case <-time.After(ctxHold949 + 5*time.Second):
		t.Fatalf("%s: the handler never reported", engine)
	}
}

// TestH2DepartureCancelsRequestContext949: an h2c client resets its stream
// (RST_STREAM CANCEL) or drops the connection while the handler waits: the
// handler's c.Context() must be cancelled, within a bound, on every engine.
// The request is stream 1 of a prior-knowledge connection, or the request of
// an RFC 7540 3.2 upgrade.
func TestH2DepartureCancelsRequestContext949(t *testing.T) {
	type open func(*testing.T, string) (net.Conn, *http2.Framer)
	for _, e := range engines761 {
		for _, how := range []struct {
			name string
			open open
		}{{"prior-knowledge", rawH2Conn949}, {"upgrade", rawUpgradeConn949}} {
			for _, depart := range []string{"rst-stream", "conn-close"} {
				t.Run(e.name+"/"+how.name+"/"+depart, func(t *testing.T) {
					addr, started, outcome := startWaitServer949(t, e.eng, ctxHold949)
					conn, fr := how.open(t, addr)
					awaitStarted949(t, started)
					at := time.Now()
					switch depart {
					case "rst-stream":
						if err := fr.WriteRSTStream(1, http2.ErrCodeCancel); err != nil {
							t.Fatalf("RST_STREAM: %v", err)
						}
					case "conn-close":
						_ = conn.Close()
					}
					expectEnded949(t, e.name, how.name+"/"+depart, outcome, at)
				})
			}
		}
	}
}

// TestH1DepartureLeavesRequestContextAlone949 pins the HTTP/1 half of the
// parity that #949 restores. On every engine, native and std, an HTTP/1
// stream's context is context.Background(): a client that drops its HTTP/1
// connection while the handler waits does not end c.Context() (the engines
// tell SSE and WebSocket handlers through their detach hook instead). The fix
// for #949 moves the h2c path of the std engine to the native behaviour and
// leaves HTTP/1 where all four engines have it; if the native engines ever
// cancel HTTP/1 contexts, this test fails on them and std has to follow.
func TestH1DepartureLeavesRequestContextAlone949(t *testing.T) {
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr, started, outcome := startWaitServer949(t, e.eng, h1Hold949)
			conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := io.WriteString(conn, fmt.Sprintf("GET /wait HTTP/1.1\r\nHost: %s\r\n\r\n", addr)); err != nil {
				t.Fatal(err)
			}
			awaitStarted949(t, started)
			at := time.Now()
			_ = conn.Close()
			select {
			case o := <-outcome:
				t.Logf("MEASURE engine=%s departure=h1-conn-close ended=%v err=%v latency=%s", e.name, o.ended, o.err, o.at.Sub(at).Round(time.Millisecond))
				if o.ended {
					t.Fatalf("%s: an HTTP/1 client left and c.Context() ended (%v); the native engines leave it as context.Background()", e.name, o.err)
				}
			case <-time.After(h1Hold949 + 10*time.Second):
				t.Fatalf("%s: the handler never reported", e.name)
			}
		})
	}
}
