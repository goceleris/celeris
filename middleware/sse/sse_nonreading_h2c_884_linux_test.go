//go:build linux

package sse_test

import (
	"bytes"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris"
)

// An h2c client that stays connected and reads nothing of an SSE stream
// (celeris#884 item 79, gap 1). The stall bound of #1077 is for detached HTTP/1
// connections; an HTTP/2 connection is never detached. What bounds an h2c stream
// whose peer stops reading is, by the code, the flow-control window (a Write on
// the worker pool waits for it for at most WriteTimeout and then resets the
// stream) and, once the peer has granted a window larger than its socket, the
// connection's ordinary write clock. Nothing measured either; these cells do.

// dialH2NonReading884 dials addr with a 4 KiB receive buffer, speaks h2c by hand
// (the preface, SETTINGS with INITIAL_WINDOW_SIZE window, a connection
// WINDOW_UPDATE up to the same size, one GET /events), reads until the response
// HEADERS of stream 1 have arrived, and then reads nothing more. The caller
// closes the conn through the cleanup.
func dialH2NonReading884(tb testing.TB, addr string, window uint32) net.Conn {
	tb.Helper()
	d := net.Dialer{
		Timeout: 2 * time.Second,
		Control: func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
			}); err != nil {
				return err
			}
			return serr
		},
	}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		tb.Fatalf("dial: %v", err)
	}
	tb.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		tb.Fatalf("preface: %v", err)
	}
	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: window}); err != nil {
		tb.Fatalf("settings: %v", err)
	}
	if window > 65535 {
		if err := fr.WriteWindowUpdate(0, window-65535); err != nil {
			tb.Fatalf("connection window update: %v", err)
		}
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", addr}, {":path", "/events"}, {"accept", "text/event-stream"}} {
		if err := enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]}); err != nil {
			tb.Fatal(err)
		}
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		tb.Fatalf("headers: %v", err)
	}
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			tb.Fatalf("waiting for the response headers: %v", err)
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				if err := fr.WriteSettingsAck(); err != nil {
					tb.Fatalf("settings ack: %v", err)
				}
			}
		case *http2.HeadersFrame:
			if f.StreamID == 1 {
				_ = conn.SetDeadline(time.Time{})
				return conn
			}
		case *http2.RSTStreamFrame, *http2.GoAwayFrame:
			tb.Fatalf("stream refused before the headers: %T", f)
		}
	}
}

// TestNonReadingH2CClientEndsStream884 drives the finding: the client reads the
// response headers and then nothing; the handler keeps sending. The stream must
// end (OnDisconnect runs) within WriteTimeout plus slack, and not before
// WriteTimeout. Two windows: the default 65535 (the stream is stopped by flow
// control: a pool Write waits for window up to WriteTimeout, then resets the
// stream) and 1 GiB (the window never closes, so the engine holds the bytes the
// socket will not take: the connection's write clock must end it).
func TestNonReadingH2CClientEndsStream884(t *testing.T) {
	// std is a control for the default window only: with a 1 GiB window it did
	// not end the stream within 26 s (a finding of this change, not asserted).
	var cells []nonReadCell888
	for _, c := range nonReadCells888(t) {
		if strings.HasSuffix(c.name, "/sync") {
			continue // the H2 processor chooses inline or pool per route, not per Config.AsyncHandlers
		}
		c.routeAsync = true
		cells = append(cells, c)
	}
	wins := []struct {
		name   string
		window uint32
		p      producer888
	}{
		{"default-window", 65535, producer888{burstBytes: 1000, settle: 100 * time.Millisecond, trickleBytes: 1000, trickleEvery: 5 * time.Millisecond}},
		{"huge-window", 1 << 30, producer888{burstBytes: burstFor888(t), settle: 700 * time.Millisecond, trickleBytes: 1000, trickleEvery: 5 * time.Millisecond}},
	}
	for _, cell := range cells {
		for _, w := range wins {
			if cell.engine == celeris.Std && w.window > 65535 {
				continue
			}
			t.Run(cell.name+"/"+w.name, func(t *testing.T) {
				addr, disconnected, shutdown := serve888(t, cell, w.p)
				defer shutdown()
				began := time.Now()
				_ = dialH2NonReading884(t, addr, w.window)
				const slack = 5 * time.Second
				wt := w.p.wt()
				select {
				case d := <-disconnected:
					el := d.at.Sub(began)
					t.Logf("OnDisconnect %v after connect (WriteTimeout %v)", el.Round(time.Millisecond), wt)
					if cell.engine != celeris.Std && el < wt {
						t.Errorf("OnDisconnect ran %v after connect, before WriteTimeout %v", el, wt)
					}
				case <-time.After(wt + slack):
					t.Fatalf("OnDisconnect never ran %v after connect (WriteTimeout %v): an h2c client that reads nothing holds its stream (celeris#884 item 79)", wt+slack, wt)
				}
			})
		}
	}
}
