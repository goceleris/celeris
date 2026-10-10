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
// the departure is exactly the frame (or the close) the test names. They also
// pin the other two halves of the parity: a client that stays does not cancel
// its handler's context, and an HTTP/1 client that leaves does not either (on
// any engine).

const (
	// ctxBound949 is how long a handler gets to see its context end after the
	// client left. The native engines take milliseconds; the bound is
	// generous so a loaded CI host does not flake, and short of ctxHold949,
	// the time a handler waits for a cancellation that never comes (the std
	// engine before the fix).
	ctxBound949 = 5 * time.Second
	ctxHold949  = 8 * time.Second
	// stayHold949 is how long a handler whose client stays waits on its
	// context before answering; it must not end in that time.
	stayHold949 = 700 * time.Millisecond
	// h1Hold949 is how long an HTTP/1 handler waits for a cancellation that
	// is expected not to come; a departure reaches a handler in milliseconds.
	h1Hold949 = 1500 * time.Millisecond
)

type ctxOutcome949 struct {
	ended   bool      // c.Context().Done() closed
	err     error     // c.Context().Err() when it did
	at      time.Time // when the handler saw it (or gave up)
	started time.Time // when the handler started
}

// startWaitServer949 starts a server on engine e whose GET /wait handler
// signals started, then waits on c.Context() for hold and reports what it
// saw. async routes it with .Async(): on the native engines a sync handler
// runs on the event loop, which cannot then see its client leave, so the
// departure arms use async routes there (and both on std, where the two
// are the same goroutine-per-request path).
func startWaitServer949(t *testing.T, e celeris.EngineType, hold time.Duration, async bool) (addr string, started chan struct{}, outcome chan ctxOutcome949) {
	t.Helper()
	started = make(chan struct{}, 16)
	outcome = make(chan ctxOutcome949, 16)
	addr = startServer761(t, e, false, func(s *celeris.Server) {
		r := s.GET("/wait", func(c *celeris.Context) error {
			o := ctxOutcome949{started: time.Now()}
			started <- struct{}{}
			ctx := c.Context()
			select {
			case <-ctx.Done():
				o.ended, o.err = true, ctx.Err()
			case <-time.After(hold):
			}
			o.at = time.Now()
			outcome <- o
			return c.String(200, "done")
		})
		if async {
			r.Async()
		}
	})
	return addr, started, outcome
}

// rawH2c949 is a raw HTTP/2 client over a connection that already speaks
// HTTP/2 and has sent GET /wait as stream 1. A goroutine reads the server's
// frames, acknowledges its SETTINGS and reports the :status of stream 1's
// response on status.
type rawH2c949 struct {
	conn   net.Conn
	fr     *http2.Framer
	status chan string
}

func (r *rawH2c949) readLoop() {
	rfr := http2.NewFramer(io.Discard, r.conn)
	// A Framer is not safe for concurrent use: the SETTINGS acks are written
	// with a Framer of this goroutine's own, as the test writes with r.fr.
	afr := http2.NewFramer(r.conn, nil)
	dec := hpack.NewDecoder(4096, nil)
	for {
		f, err := rfr.ReadFrame()
		if err != nil {
			return
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				_ = afr.WriteSettingsAck()
			}
		case *http2.HeadersFrame:
			if f.StreamID != 1 {
				continue
			}
			fields, err := dec.DecodeFull(f.HeaderBlockFragment())
			if err != nil {
				return
			}
			for _, hf := range fields {
				if hf.Name == ":status" {
					select {
					case r.status <- hf.Value:
					default:
					}
				}
			}
		}
	}
}

func get949(t *testing.T, fr *http2.Framer, addr string) {
	t.Helper()
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
}

// priorKnowledge949 opens a prior-knowledge h2c connection and sends GET /wait
// as stream 1.
func priorKnowledge949(t *testing.T, addr string) *rawH2c949 {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatalf("preface: %v", err)
	}
	r := &rawH2c949{conn: conn, fr: http2.NewFramer(conn, conn), status: make(chan string, 1)}
	if err := r.fr.WriteSettings(); err != nil {
		t.Fatalf("settings: %v", err)
	}
	get949(t, r.fr, addr)
	go r.readLoop()
	return r
}

// upgrade949 sends GET /wait as an RFC 7540 3.2 upgrade request and reads the
// 101; the request is then HTTP/2 stream 1.
func upgrade949(t *testing.T, addr string) *rawH2c949 {
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
	// Read the 101 head byte by byte so no HTTP/2 byte is consumed.
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
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatalf("preface: %v", err)
	}
	r := &rawH2c949{conn: conn, fr: http2.NewFramer(conn, conn), status: make(chan string, 1)}
	if err := r.fr.WriteSettings(); err != nil {
		t.Fatalf("settings: %v", err)
	}
	go r.readLoop()
	return r
}

func awaitStarted949(t *testing.T, started chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never started")
	}
}

var shapes949 = []struct {
	name string
	open func(*testing.T, string) *rawH2c949
}{{"prior-knowledge", priorKnowledge949}, {"upgrade", upgrade949}}

func routeName949(async bool) string {
	if async {
		return "async"
	}
	return "sync"
}

// TestH2DepartureCancelsRequestContext949: an h2c client resets its stream
// (RST_STREAM CANCEL) or drops the connection while the handler waits: the
// handler's c.Context() must be cancelled, within a bound, on every engine.
// The request is stream 1 of a prior-knowledge connection, or the request of
// an RFC 7540 3.2 upgrade.
func TestH2DepartureCancelsRequestContext949(t *testing.T) {
	for _, e := range engines761 {
		for _, sh := range shapes949 {
			for _, depart := range []string{"rst-stream", "conn-close"} {
				for _, async := range []bool{true, false} {
					if !async && e.name != "std" {
						// A sync handler runs on the native event loop,
						// which does not see the departure until the
						// handler returns.
						continue
					}
					route := routeName949(async)
					t.Run(e.name+"/"+sh.name+"/"+depart+"/"+route, func(t *testing.T) {
						addr, started, outcome := startWaitServer949(t, e.eng, ctxHold949, async)
						r := sh.open(t, addr)
						awaitStarted949(t, started)
						at := time.Now()
						switch depart {
						case "rst-stream":
							if err := r.fr.WriteRSTStream(1, http2.ErrCodeCancel); err != nil {
								t.Fatalf("RST_STREAM: %v", err)
							}
						case "conn-close":
							_ = r.conn.Close()
						}
						select {
						case o := <-outcome:
							lat := o.at.Sub(at)
							t.Logf("MEASURE engine=%s shape=%s departure=%s route=%s ended=%v err=%v latency=%s",
								e.name, sh.name, depart, route, o.ended, o.err, lat.Round(time.Millisecond))
							if !o.ended {
								t.Fatalf("%s: the client left (%s) and the handler's c.Context() was still not done after %s", e.name, depart, o.at.Sub(o.started))
							}
							if o.err == nil || o.err.Error() != "context canceled" {
								t.Fatalf("%s: c.Context().Err() = %v, want context canceled", e.name, o.err)
							}
							if lat > ctxBound949 {
								t.Fatalf("%s: the handler saw the departure after %s, want within %s", e.name, lat, ctxBound949)
							}
						case <-time.After(ctxHold949 + 5*time.Second):
							t.Fatalf("%s: the handler never reported", e.name)
						}
					})
				}
			}
		}
	}
}

// TestH2StayingClientKeepsRequestContext949 is the other side of the test
// above, and the guard against a fix that cancels too much: while the h2c
// client stays connected and waits, the handler's c.Context() is not done,
// and the client then gets its 200.
func TestH2StayingClientKeepsRequestContext949(t *testing.T) {
	for _, e := range engines761 {
		for _, sh := range shapes949 {
			for _, async := range []bool{true, false} {
				t.Run(e.name+"/"+sh.name+"/"+routeName949(async), func(t *testing.T) {
					addr, started, outcome := startWaitServer949(t, e.eng, stayHold949, async)
					r := sh.open(t, addr)
					awaitStarted949(t, started)
					select {
					case st := <-r.status:
						if st != "200" {
							t.Fatalf("%s: answered :status %q, want 200", e.name, st)
						}
					case <-time.After(10 * time.Second):
						t.Fatalf("%s: the staying client got no response", e.name)
					}
					select {
					case o := <-outcome:
						if o.ended {
							t.Fatalf("%s: the client stayed and the handler's c.Context() ended (%v) after %s", e.name, o.err, o.at.Sub(o.started))
						}
						if d := o.at.Sub(o.started); d < stayHold949-50*time.Millisecond {
							t.Fatalf("%s: the handler waited %s of %s: the test did not hold the context open", e.name, d, stayHold949)
						}
					case <-time.After(10 * time.Second):
						t.Fatalf("%s: the handler never reported", e.name)
					}
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
			addr, started, outcome := startWaitServer949(t, e.eng, h1Hold949, true)
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
