//go:build linux

package celeris_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
)

// The write clock of an h2 stream on io_uring (celeris#889, L19). The sweep
// measures WriteTimeout, for a conn that has a send queued or in flight, from
// lastActivity: the last read or completed send. A stream that is quiet for
// longer than WriteTimeout between two chunks has a clock as old as the gap, so
// when a sweep (every 50 to 100 ms) finds the next chunk's SEND in flight, the
// conn is closed, mid-stream, with the chunk: measured with WriteTimeout 100 ms
// and a chunk every 250 ms, one chunk in 16 to 44 (3.75, 6.26 and 10.75 s in
// three runs), and once in CI with WriteTimeout 1 s and a gap of 2 s. epoll and
// adaptive have their own write clock (writeSince) and stay open.

const (
	wtq889Timeout = 100 * time.Millisecond
	wtq889Tick    = 250 * time.Millisecond
	wtq889Hold    = 6 * time.Second
	// wtq889Streams are the conns of one server: each chunk of each is a
	// chance for a sweep to find its send in flight, so that the defect does
	// not depend on one conn's luck (one stream is cut within 6 s in about
	// half of the runs on main). Their ticks differ by 13 ms each, so that the
	// chunks do not all fall on the same phase of the sweep.
	wtq889Streams = 16
)

// wtqDial is a raw h2c client (prior knowledge) with one GET open.
func wtqDial(addr, path string, window uint32) (net.Conn, *http2.Framer, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, nil, err
	}
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	fr := http2.NewFramer(conn, conn)
	var settings []http2.Setting
	if window != 0 {
		settings = append(settings, http2.Setting{ID: http2.SettingInitialWindowSize, Val: window})
	}
	if err := fr.WriteSettings(settings...); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("no SETTINGS from the server: %w", err)
		}
		if sf, ok := f.(*http2.SettingsFrame); ok && !sf.IsAck() {
			if err := fr.WriteSettingsAck(); err != nil {
				_ = conn.Close()
				return nil, nil, err
			}
			break
		}
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", addr}, {":path", path}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, fr, nil
}

// TestQuietH2StreamIsNotCutByWriteTimeout889: wtq889Streams h2c streams, each
// with a chunk every 250 ms and a client that reads them and sends nothing,
// WriteTimeout 100 ms (ReadTimeout and IdleTimeout off): every stream is
// still open after 6 s on every native engine. Red on main on io_uring.
func TestQuietH2StreamIsNotCutByWriteTimeout889(t *testing.T) {
	for _, e := range engines761 {
		if e.name == "std" {
			continue // std's WriteTimeout is an absolute deadline per response (celeris#855)
		}
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			cfg := celeris.Config{Engine: e.eng, ReadTimeout: -1, IdleTimeout: -1, WriteTimeout: wtq889Timeout}
			addr := startServerConfig761(t, cfg, func(s *celeris.Server) {
				s.GET("/tick889", func(c *celeris.Context) error {
					sw := c.StreamWriter()
					if sw == nil {
						return errors.New("no StreamWriter")
					}
					if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/event-stream"}}); err != nil {
						return err
					}
					tick := wtq889Tick
					if ms, err := strconv.Atoi(c.Query("ms")); err == nil && ms > 0 {
						tick = time.Duration(ms) * time.Millisecond
					}
					tk := time.NewTicker(tick)
					defer tk.Stop()
					stop := time.After(2 * wtq889Hold)
					done := c.Context().Done()
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
			})
			var (
				wg     sync.WaitGroup
				cut    atomic.Int32
				mu     sync.Mutex
				first  string
				chunks atomic.Int64
			)
			for i := 0; i < wtq889Streams; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					conn, fr, err := wtqDial(addr, "/tick889?ms="+strconv.Itoa(int(wtq889Tick/time.Millisecond)+13*i), 0)
					if err != nil {
						cut.Add(1)
						mu.Lock()
						if first == "" {
							first = err.Error()
						}
						mu.Unlock()
						return
					}
					defer func() { _ = conn.Close() }()
					t0 := time.Now()
					_ = conn.SetReadDeadline(t0.Add(wtq889Hold))
					for {
						f, err := fr.ReadFrame()
						if err != nil {
							var ne net.Error
							if errors.As(err, &ne) && ne.Timeout() {
								return // still open at the end of the hold
							}
							cut.Add(1)
							mu.Lock()
							if first == "" {
								first = fmt.Sprintf("%v after %v", err, time.Since(t0).Round(time.Millisecond))
								if errors.Is(err, io.EOF) {
									first = fmt.Sprintf("EOF after %v", time.Since(t0).Round(time.Millisecond))
								}
							}
							mu.Unlock()
							return
						}
						if df, ok := f.(*http2.DataFrame); ok && df.StreamID == 1 {
							chunks.Add(1)
						}
					}
				}()
			}
			wg.Wait()
			t.Logf("WTQ889 engine=%s: %d of %d streams cut within %v (WriteTimeout %v, a chunk every %v; %d chunks seen)%s",
				e.name, cut.Load(), wtq889Streams, wtq889Hold, wtq889Timeout, wtq889Tick, chunks.Load(), map[bool]string{true: ", first: " + first}[first != ""])
			if n := cut.Load(); n > 0 {
				t.Fatalf("%d of %d streams were cut within %v by WriteTimeout %v with a chunk every %v (first: %s)", n, wtq889Streams, wtq889Hold, wtq889Timeout, wtq889Tick, first)
			}
		})
	}
}

// TestNonReadingH2ClientIsStillCutByWriteTimeout889: the other side of it: a
// client that requests a response far over what the sockets buffer and reads
// none of it is cut on the native engines: WriteTimeout (1 s) ends the conn, on
// io_uring after the 5 s its close waits for the queued SEND, so the client has
// seen the end of the conn by 12 s. (After 6 s io_uring is still draining: the
// response is delivered whole if the client reads then.)
func TestNonReadingH2ClientIsStillCutByWriteTimeout889(t *testing.T) {
	for _, e := range engines761 {
		if e.name == "std" {
			continue
		}
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			cfg := celeris.Config{Engine: e.eng, ReadTimeout: -1, IdleTimeout: -1, WriteTimeout: time.Second}
			body := make([]byte, 48<<20)
			addr := startServerConfig761(t, cfg, func(s *celeris.Server) {
				s.GET("/big889", func(c *celeris.Context) error { return c.Blob(200, "application/octet-stream", body) }).Async()
			})
			conn, fr, err := wtqDial(addr, "/big889", 1<<31-1)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			// The stream window is 2^31-1 (SETTINGS) and so is the connection's
			// (WINDOW_UPDATE), so that the server writes all it has, and then
			// nothing is read: the socket fills, the SEND stalls, and the conn
			// is cut at WriteTimeout. Reading after 6 s finds the bytes the
			// kernel took and then the end of the conn; a conn that was not cut
			// has the client's read time out (or the whole body).
			if err := fr.WriteWindowUpdate(0, 1<<31-1-65535); err != nil {
				t.Fatal(err)
			}
			t0 := time.Now()
			time.Sleep(12 * time.Second)
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			var got int64
			for {
				f, err := fr.ReadFrame()
				if err != nil {
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						t.Fatalf("the conn was still open 16 s after the request, with %.1f MiB of the 48 MiB taken and the client reading nothing (WriteTimeout 1 s)", float64(got)/(1<<20))
					}
					t.Logf("WTSTALL889 engine=%s: the conn was cut (%v) after %.1f MiB of the 48 MiB were taken, the client having read nothing for 12 s", e.name, err, float64(got)/(1<<20))
					return
				}
				if df, ok := f.(*http2.DataFrame); ok {
					got += int64(len(df.Data()))
					if df.StreamEnded() {
						t.Fatalf("the whole 48 MiB response was delivered to a client that read nothing for 12 s (WriteTimeout 1 s): not cut (%v)", time.Since(t0))
					}
				}
			}
		})
	}
}
