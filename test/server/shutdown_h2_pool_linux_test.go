//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
)

// TestShutdownSendsH2GoAwayThenFinishesStreams pins the HTTP/2 half of the
// native engines' graceful shutdown (celeris#759), on the wire: with a stream
// on an async route held in its handler, which runs on the shared HTTP/2
// worker pool, the shutdown must first send GOAWAY (NO_ERROR, naming the
// held stream or a later one), so the client opens no new stream, and then
// let the held stream finish: its response arrives after the GOAWAY, and only
// then does the connection close. Before, the shutdown cancelled the stream
// and closed the connection under the handler, with no GOAWAY.
//
// The order is forced: the handler is released only once the client has read
// the GOAWAY, or after releaseAfter if none comes, so a shutdown that closes
// at once, or sends no GOAWAY, fails every time. "Shutdown-background" shuts
// down with context.Background(), a ctx with no deadline, and releases the
// handler holdNoDeadline after the GOAWAY: past the wait's 250 ms floor, which
// is all a ctx without a deadline got.
func TestShutdownSendsH2GoAwayThenFinishesStreams(t *testing.T) {
	const releaseAfter = 2 * time.Second
	const holdNoDeadline = 600 * time.Millisecond
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}} {
		for _, mode := range []string{"Shutdown", "Shutdown-background", "cancel"} {
			t.Run(e.name+"/"+mode, func(t *testing.T) {
				entered := make(chan struct{})
				release := make(chan struct{})
				srv := startH2PoolServer759(t, e.eng, 30*time.Second, func(s *celeris.Server) {
					s.GET("/held", func(c *celeris.Context) error {
						close(entered)
						<-release
						return c.String(http.StatusOK, "done")
					}).Async()
				})
				c, fr := dialH2759(t, srv.addr)
				writeH2Get759(t, fr, 1, "/held")
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("the held handler did not start within 5s")
				}
				srv.beginShutdown(mode, 30*time.Second)

				// Read until GOAWAY, releasing the handler only then.
				released := false
				releaseTimer := time.AfterFunc(releaseAfter, func() { close(release) })
				var sawGoAway bool
				var goAwayCode http2.ErrCode
				var goAwayLast uint32
				var status string
				var body bytes.Buffer
				var events []string
				dec := hpack.NewDecoder(4096, nil)
				for {
					_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
					f, err := fr.ReadFrame()
					if err != nil {
						events = append(events, "read: "+describeH2ReadEnd759(err))
						break
					}
					switch f := f.(type) {
					case *http2.GoAwayFrame:
						sawGoAway, goAwayCode, goAwayLast = true, f.ErrCode, f.LastStreamID
						events = append(events, "GOAWAY")
						if !released && releaseTimer.Stop() {
							released = true
							if mode == "Shutdown-background" {
								time.AfterFunc(holdNoDeadline, func() { close(release) })
							} else {
								close(release)
							}
						}
					case *http2.HeadersFrame:
						if f.StreamID == 1 {
							hf, _ := dec.DecodeFull(f.HeaderBlockFragment())
							for _, h := range hf {
								if h.Name == ":status" {
									status = h.Value
								}
							}
							events = append(events, "HEADERS")
						}
					case *http2.DataFrame:
						if f.StreamID == 1 {
							body.Write(f.Data())
							if f.StreamEnded() {
								events = append(events, "DATA(end)")
							}
						}
					case *http2.RSTStreamFrame:
						events = append(events, "RST_STREAM "+f.ErrCode.String())
					case *http2.SettingsFrame:
						if !f.IsAck() {
							_ = fr.WriteSettingsAck()
						}
					case *http2.PingFrame:
						if !f.IsAck() {
							_ = fr.WritePing(true, f.Data)
						}
					}
				}
				if !released && releaseTimer.Stop() {
					close(release) // no GOAWAY came: let the handler go
				}
				srv.waitStart(t, 20*time.Second)
				trace := strings.Join(events, ", ")
				if !sawGoAway || goAwayCode != http2.ErrCodeNo || goAwayLast < 1 {
					t.Fatalf("%s/%s: GOAWAY seen=%v code=%v last=%d, want NO_ERROR naming stream 1 or later; frames: %s", e.name, mode, sawGoAway, goAwayCode, goAwayLast, trace)
				}
				if status != "200" || body.String() != "done" {
					t.Fatalf("%s/%s: stream 1 got status %q body %q; frames: %s", e.name, mode, status, body.String(), trace)
				}
				if i, j := strings.Index(trace, "GOAWAY"), strings.Index(trace, "HEADERS"); i < 0 || j < i {
					t.Fatalf("%s/%s: the response came before the GOAWAY; frames: %s", e.name, mode, trace)
				}
			})
		}
	}
}

// TestShutdownH2PoolWaitIsBounded: a handler on the HTTP/2 worker pool that
// does not return must not hold the native engines' shutdown past its budget
// (celeris#759). The wait for pool handlers ends at the budget's deadline,
// and the Start call returns within bound of it, with the handler still
// running.
func TestShutdownH2PoolWaitIsBounded(t *testing.T) {
	const budget = 500 * time.Millisecond
	// bound is the most Start may return after the budget (was 3 s). Measured in
	// a linux/arm64 container: Start returned 500-512 ms after the shutdown began
	// over 120 runs without -race and 501-579 ms over 120 runs with -race (the
	// budget is 500 ms), so 1 s is about 12 times the largest excess seen.
	const bound = time.Second
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}} {
		for _, mode := range []string{"Shutdown", "cancel"} {
			t.Run(e.name+"/"+mode, func(t *testing.T) {
				entered := make(chan struct{})
				release := make(chan struct{})
				defer close(release)
				srv := startH2PoolServer759(t, e.eng, budget, func(s *celeris.Server) {
					s.GET("/stuck", func(c *celeris.Context) error {
						close(entered)
						<-release
						return c.String(http.StatusOK, "late")
					}).Async()
				})
				_, fr := dialH2759(t, srv.addr)
				writeH2Get759(t, fr, 1, "/stuck")
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("the stuck handler did not start within 5s")
				}
				start := time.Now()
				srv.beginShutdown(mode, budget)
				srv.waitStart(t, budget+bound)
				elapsed := time.Since(start)
				t.Logf("%s/%s: Start returned %v after the shutdown began, with the handler still running", e.name, mode, elapsed.Round(time.Millisecond))
				// The handler is held until the test's defer, so the wait can only
				// end at the budget: Start cannot return before it.
				if elapsed < budget {
					t.Errorf("%s/%s: Start returned %v after the shutdown began, before the %v budget, with the handler still running", e.name, mode, elapsed, budget)
				}
			})
		}
	}
}

type h2PoolServer759 struct {
	s         *celeris.Server
	addr      string
	cancel    context.CancelFunc
	startDone chan error
	shutErr   chan error
}

func startH2PoolServer759(t *testing.T, eng celeris.EngineType, budget time.Duration, routes func(*celeris.Server)) *h2PoolServer759 {
	t.Helper()
	retryUntil := time.Now().Add(30 * time.Second)
	for tries := 1; ; tries++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		s := celeris.New(celeris.Config{Engine: eng, Addr: addr, ShutdownTimeout: budget})
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		routes(s)
		ctx, cancel := context.WithCancel(context.Background())
		srv := &h2PoolServer759{s: s, addr: addr, cancel: cancel, startDone: make(chan error, 1)}
		go func() { srv.startDone <- s.StartWithContext(ctx) }()
		err = waitDrainOrderReady(addr, srv.startDone)
		if err == nil {
			if tries > 1 {
				t.Logf("server start retried on ring ENOMEM: %d tries", tries)
			}
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-srv.startDone:
					srv.startDone <- err
				case <-time.After(30 * time.Second):
					t.Errorf("StartWithContext did not return within 30s of the cleanup cancel")
				}
			})
			return srv
		}
		cancel()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(retryUntil) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start: %v", err)
	}
}

// beginShutdown starts the shutdown: a cancel of StartWithContext's context
// ("cancel", whose budget is the server's ShutdownTimeout), a Shutdown with
// context.Background(), which has no deadline ("Shutdown-background"), or a
// Shutdown with a budget of its own.
func (srv *h2PoolServer759) beginShutdown(mode string, budget time.Duration) {
	if mode == "cancel" {
		srv.cancel()
		return
	}
	srv.shutErr = make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		if mode == "Shutdown-background" {
			ctx = context.Background()
		}
		defer cancel()
		srv.shutErr <- srv.s.Shutdown(ctx)
	}()
}

func (srv *h2PoolServer759) waitStart(t *testing.T, limit time.Duration) {
	t.Helper()
	deadline := time.After(limit)
	if srv.shutErr != nil {
		select {
		case <-srv.shutErr:
		case <-deadline:
			t.Fatalf("Shutdown had not returned %v after it began", limit)
		}
	}
	select {
	case err := <-srv.startDone:
		srv.startDone <- err // for the cleanup
	case <-deadline:
		t.Fatalf("StartWithContext had not returned %v after the shutdown began", limit)
	}
}

// dialH2759 opens a raw h2c (prior knowledge) connection: the client preface
// and an empty SETTINGS frame.
func dialH2759(t *testing.T, addr string) (net.Conn, *http2.Framer) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := io.WriteString(c, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(c, c)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	return c, fr
}

func writeH2Get759(t *testing.T, fr *http2.Framer, id uint32, path string) {
	t.Helper()
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range []hpack.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: "x"}, {Name: ":path", Value: path},
	} {
		_ = enc.WriteField(f)
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
}

func describeH2ReadEnd759(err error) string {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return "no frame for 10s, connection still open"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "EOF"
	}
	return err.Error()
}

// frames759 reads frames from fr until the connection ends, idle passes
// without one, or on returns true, answering SETTINGS and PING and handing
// every frame to on; it returns how the read ended. Its writes take wmu, which
// a caller writing on fr from another goroutine holds too (a Framer is not
// safe for concurrent writes).
func frames759(c net.Conn, fr *http2.Framer, wmu *sync.Mutex, idle time.Duration, on func(http2.Frame) bool) string {
	for {
		_ = c.SetReadDeadline(time.Now().Add(idle))
		f, err := fr.ReadFrame()
		if err != nil {
			return describeH2ReadEnd759(err)
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				wmu.Lock()
				_ = fr.WriteSettingsAck()
				wmu.Unlock()
			}
		case *http2.PingFrame:
			if !f.IsAck() {
				wmu.Lock()
				_ = fr.WritePing(true, f.Data)
				wmu.Unlock()
			}
		}
		if on(f) {
			return "stopped"
		}
	}
}

// engines759 are every engine, std included.
var engines759 = []struct {
	name string
	eng  celeris.EngineType
}{{"std", celeris.Std}, {"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}}

// TestShutdownAcceptsNoNewConnection: while a graceful shutdown waits for an
// HTTP/2 stream on the worker pool (celeris#759), the native engines kept
// their listeners and accepted and served new connections, HTTP/1.1 and h2c,
// for as long as the wait lasted (up to the whole budget), then cut them at
// its end. net/http's Shutdown closes its listeners first, and so must they:
// a connection attempted 300 ms into the shutdown is not served.
func TestShutdownAcceptsNoNewConnection(t *testing.T) {
	for _, e := range engines759 {
		t.Run(e.name, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			srv := startH2PoolServer759(t, e.eng, 5*time.Second, func(s *celeris.Server) {
				s.GET("/held", func(c *celeris.Context) error {
					close(entered)
					<-release
					return c.String(http.StatusOK, "done")
				}).Async()
			})
			c, fr := dialH2759(t, srv.addr)
			writeH2Get759(t, fr, 1, "/held")
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the held handler did not start within 5s")
			}
			heldDone := make(chan string, 1)
			go func() {
				heldDone <- frames759(c, fr, new(sync.Mutex), 10*time.Second, func(http2.Frame) bool { return false })
			}()
			srv.beginShutdown("Shutdown", 5*time.Second)
			time.Sleep(300 * time.Millisecond)
			h1 := tryH1Ping759(srv.addr)
			h2c := tryH2cPing759(srv.addr)
			close(release)
			srv.waitStart(t, 10*time.Second)
			_ = c.Close() // std leaves an h2c connection open past the shutdown
			<-heldDone
			if h1 == "served" || h2c == "served" {
				t.Fatalf("%s: 300 ms into the shutdown a new connection was served: HTTP/1.1 %s, h2c %s", e.name, h1, h2c)
			}
			t.Logf("%s: new HTTP/1.1 connection: %s; new h2c connection: %s", e.name, h1, h2c)
		})
	}
}

// tryH1Ping759 dials addr and sends GET /ping over HTTP/1.1; it returns
// "served" if a response came, else how the attempt ended.
func tryH1Ping759(addr string) string {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return "dial: " + err.Error()
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(c, "GET /ping HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		return "write: " + err.Error()
	}
	if _, err := http.ReadResponse(bufio.NewReader(c), nil); err != nil {
		return "read: " + describeH2ReadEnd759(err)
	}
	return "served"
}

// tryH2cPing759 dials addr and asks for /ping on h2c stream 1; it returns
// "served" if response HEADERS came for it, else how the attempt ended.
func tryH2cPing759(addr string) string {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return "dial: " + err.Error()
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, http2.ClientPreface); err != nil {
		return "write: " + err.Error()
	}
	fr := http2.NewFramer(c, c)
	_ = fr.WriteSettings()
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/ping"}} {
		_ = enc.WriteField(f)
	}
	_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
	res := ""
	end := frames759(c, fr, new(sync.Mutex), time.Second, func(f http2.Frame) bool {
		switch f := f.(type) {
		case *http2.HeadersFrame:
			if f.StreamID == 1 {
				res = "served"
			}
		case *http2.RSTStreamFrame:
			res = "RST_STREAM " + f.ErrCode.String()
		}
		return res != ""
	})
	if res != "" {
		return res
	}
	return "read: " + end
}

// TestShutdownRefusesStreamsAfterGoAway: once the graceful shutdown has sent
// GOAWAY naming stream 1, a stream the client opens anyway (3, on an async
// route; 5, on a sync one) must not be served (celeris#759). Its client
// counts it as not processed and may retry it on another connection (RFC
// 9113 §6.8), so serving it could run a request twice. It is refused with
// RST_STREAM(REFUSED_STREAM), and stream 1 still finishes. std does not send
// h2c connections GOAWAY at all; that is not asserted here.
func TestShutdownRefusesStreamsAfterGoAway(t *testing.T) {
	for _, e := range engines759[1:] {
		t.Run(e.name, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			var served atomic.Int32
			srv := startH2PoolServer759(t, e.eng, 5*time.Second, func(s *celeris.Server) {
				s.GET("/held", func(c *celeris.Context) error {
					close(entered)
					<-release
					return c.String(http.StatusOK, "done")
				}).Async()
				s.GET("/quick", func(c *celeris.Context) error {
					served.Add(1)
					return c.String(http.StatusOK, "quick")
				}).Async()
				s.GET("/quicksync", func(c *celeris.Context) error {
					served.Add(1)
					return c.String(http.StatusOK, "quick")
				})
			})
			c, fr := dialH2759(t, srv.addr)
			writeH2Get759(t, fr, 1, "/held")
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the held handler did not start within 5s")
			}
			srv.beginShutdown("Shutdown", 5*time.Second)
			var events []string
			rst := map[uint32]http2.ErrCode{}
			sent := false
			var releaseOnce sync.Once
			end := frames759(c, fr, new(sync.Mutex), 5*time.Second, func(f http2.Frame) bool {
				switch f := f.(type) {
				case *http2.GoAwayFrame:
					events = append(events, "GOAWAY(last="+strconv.Itoa(int(f.LastStreamID))+")")
					if !sent {
						sent = true
						writeH2Get759(t, fr, 3, "/quick")
						writeH2Get759(t, fr, 5, "/quicksync")
						// Stream 1 goes on once the refusals had time to come.
						time.AfterFunc(500*time.Millisecond, func() { releaseOnce.Do(func() { close(release) }) })
					}
				case *http2.HeadersFrame:
					events = append(events, "HEADERS(s"+strconv.Itoa(int(f.StreamID))+")")
				case *http2.DataFrame:
					events = append(events, "DATA(s"+strconv.Itoa(int(f.StreamID))+")")
				case *http2.RSTStreamFrame:
					rst[f.StreamID] = f.ErrCode
					events = append(events, "RST_STREAM(s"+strconv.Itoa(int(f.StreamID))+","+f.ErrCode.String()+")")
				}
				return false
			})
			releaseOnce.Do(func() { close(release) })
			srv.waitStart(t, 10*time.Second)
			trace := strings.Join(events, ", ") + ", read: " + end
			if !sent {
				t.Fatalf("%s: no GOAWAY; frames: %s", e.name, trace)
			}
			if n := served.Load(); n != 0 || strings.Contains(trace, "HEADERS(s3)") || strings.Contains(trace, "HEADERS(s5)") {
				t.Fatalf("%s: %d stream(s) opened after the GOAWAY were served; frames: %s", e.name, n, trace)
			}
			if rst[3] != http2.ErrCodeRefusedStream || rst[5] != http2.ErrCodeRefusedStream {
				t.Fatalf("%s: streams 3 and 5, opened after the GOAWAY, were not refused (REFUSED_STREAM); frames: %s", e.name, trace)
			}
			if !strings.Contains(trace, "DATA(s1)") {
				t.Fatalf("%s: stream 1 did not finish; frames: %s", e.name, trace)
			}
		})
	}
}

// TestShutdownWaitsForFlowControlledH2Response: a response whose DATA waits
// for the client's WINDOW_UPDATE when the shutdown begins must still go out
// whole (celeris#759). The client keeps the default 65535-byte windows and
// opens them 400 ms into the shutdown, past the wait's 250 ms floor; the
// native engines took a handler that had returned for a settled stream and
// closed the connection with the rest of the body unsent.
func TestShutdownWaitsForFlowControlledH2Response(t *testing.T) {
	const size = 1 << 20
	body := bytes.Repeat([]byte("z"), size)
	for _, e := range engines759 {
		for _, route := range []string{"sync", "async-route"} {
			t.Run(e.name+"/"+route, func(t *testing.T) {
				entered := make(chan struct{})
				release := make(chan struct{})
				srv := startH2PoolServer759(t, e.eng, 5*time.Second, func(s *celeris.Server) {
					r := s.GET("/big", func(c *celeris.Context) error {
						if route == "async-route" {
							close(entered)
							<-release
						}
						return c.Blob(http.StatusOK, "application/octet-stream", body)
					})
					if route == "async-route" {
						r.Async()
					}
				})
				c, fr := dialH2759(t, srv.addr)
				writeH2Get759(t, fr, 1, "/big")
				n, ended := 0, false
				firstData := make(chan struct{})
				var sawData bool
				var events []string
				var wmu sync.Mutex
				done := make(chan string, 1)
				go func() {
					done <- frames759(c, fr, &wmu, 5*time.Second, func(f http2.Frame) bool {
						switch f := f.(type) {
						case *http2.GoAwayFrame:
							events = append(events, "GOAWAY")
						case *http2.DataFrame:
							if f.StreamID == 1 {
								if !sawData {
									sawData = true
									close(firstData)
								}
								n += len(f.Data())
								ended = ended || f.StreamEnded()
							}
						case *http2.RSTStreamFrame:
							events = append(events, "RST_STREAM "+f.ErrCode.String())
						}
						return ended
					})
				}()
				if route == "async-route" {
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("the handler did not start within 5s")
					}
				} else {
					// The first DATA frame of stream 1 means the handler has run and the
					// response is waiting for the window; the shutdown begins then (was a
					// 300 ms sleep, which a slow runner could end before the handler ran).
					select {
					case <-firstData:
					case <-time.After(5 * time.Second):
						t.Fatal("no DATA frame for stream 1 within 5s")
					}
				}
				start := time.Now()
				srv.beginShutdown("Shutdown", 5*time.Second)
				if route == "async-route" {
					time.Sleep(100 * time.Millisecond)
					close(release)
				}
				time.Sleep(time.Until(start.Add(400 * time.Millisecond)))
				wmu.Lock()
				_ = fr.WriteWindowUpdate(0, size)
				_ = fr.WriteWindowUpdate(1, size)
				wmu.Unlock()
				end := <-done
				srv.waitStart(t, 10*time.Second)
				if n != size || !ended {
					t.Fatalf("%s/%s: stream 1 got %d of %d body bytes, ended=%v, then %s; frames: %s", e.name, route, n, size, ended, end, strings.Join(events, ", "))
				}
			})
		}
	}
}

// TestShutdownPoolWaitStopsServingH1AndDetached884: while the shutdown waits
// for an HTTP/2 stream handler on the worker pool, the native engines serve
// no more HTTP/1 requests and let no detached stream (Server-Sent Events, a
// WebSocket) write (celeris#884 item 50). io_uring's pool wait stopped
// accepting but went on reading and answering the keep-alive requests of its
// HTTP/1 clients and writing a detached stream's events until the handler
// ended, up to the whole budget; only its later send drain had the gate
// (celeris#806: refuseRecv and stopDetachedProducers).
//
// The pool wait is per worker: a worker with no HTTP/2 connection settles at
// once and goes on to the send drain. So the test holds one stream on each of
// many HTTP/2 connections, enough that every worker owns some, and the HTTP/1
// and detached clients are then on a worker in the pool wait whichever it is.
//
// The gate comes shutdownSendDrainNanos (250 ms) into the wait, as the send
// drain's does, so a request in flight when the shutdown begins is still
// answered: the first assertion is that requests made as soon as every
// listener has closed are. A request whose bytes have not all arrived by the
// gate is another matter: the gate drops the bytes that come after it and the
// recv is not re-armed (a slow upload, headers in pieces), which the pull
// request's open questions record. The oracle looks 1.5 s in, well past the grace and the time a worker idle in its ring
// wait takes to notice the cancel.
//
// io_uring only: epoll's pool wait (Loop.run, h2PoolSettled) does not stop
// reading or writing either; its loops stop at their shutdown, after the pool
// settles (the same run asserts it: with epoll in the list this test fails on
// it, which the polish checklist of epoll, celeris#885, records; with epoll
// in the list the run measures 16 of 16 answered and 688 to 704 ticks in
// 900 ms). The adaptive engine is run on io_uring (CELERIS_ADAPTIVE_START), as
// the second arm: its gate is the io_uring worker's.
func TestShutdownPoolWaitStopsServingH1AndDetached884(t *testing.T) {
	const nH2 = 48
	for _, e := range []struct {
		name  string
		eng   celeris.EngineType
		setup func(*testing.T)
	}{
		{"io_uring", celeris.IOUring, func(*testing.T) {}},
		{"adaptive-iouring", celeris.Adaptive, func(t *testing.T) { t.Setenv("CELERIS_ADAPTIVE_START", "iouring") }},
	} {
		t.Run(e.name, func(t *testing.T) {
			e.setup(t)
			entered := make(chan struct{}, nH2)
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseHeld()
			srv := startH2PoolServer759(t, e.eng, 20*time.Second, func(s *celeris.Server) {
				s.GET("/held", func(c *celeris.Context) error {
					entered <- struct{}{}
					<-release
					return c.String(http.StatusOK, "done")
				}).Async()
				s.GET("/stream", func(c *celeris.Context) error {
					done := c.Detach()
					sw := c.StreamWriter()
					if sw == nil {
						done()
						return errors.New("no stream writer")
					}
					if err := sw.WriteHeader(http.StatusOK, [][2]string{{"content-type", "text/plain"}}); err != nil {
						done()
						return err
					}
					go func() {
						defer done()
						for {
							if _, err := sw.Write([]byte("tick\n")); err != nil {
								return
							}
							time.Sleep(20 * time.Millisecond)
						}
					}()
					return nil
				})
			})

			// The HTTP/2 streams the shutdown waits for.
			type h2Result struct {
				status string
				body   bytes.Buffer
				ended  bool
				end    string
			}
			results := make([]*h2Result, nH2)
			var h2wg sync.WaitGroup
			for i := range nH2 {
				c, fr := dialH2759(t, srv.addr)
				writeH2Get759(t, fr, 1, "/held")
				res := &h2Result{}
				results[i] = res
				h2wg.Add(1)
				go func() {
					defer h2wg.Done()
					dec := hpack.NewDecoder(4096, nil)
					res.end = frames759(c, fr, new(sync.Mutex), 15*time.Second, func(f http2.Frame) bool {
						switch f := f.(type) {
						case *http2.HeadersFrame:
							if f.StreamID == 1 {
								hf, _ := dec.DecodeFull(f.HeaderBlockFragment())
								for _, h := range hf {
									if h.Name == ":status" {
										res.status = h.Value
									}
								}
							}
						case *http2.DataFrame:
							if f.StreamID == 1 {
								res.body.Write(f.Data())
								res.ended = res.ended || f.StreamEnded()
							}
						}
						return res.ended
					})
				}()
			}
			for i := range nH2 {
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatalf("only %d of %d held handlers started within 10s", i, nH2)
				}
			}

			// HTTP/1 keep-alive clients, warm, one per HTTP/2 connection's
			// worth of luck: each lands on some worker, which owns HTTP/2
			// connections.
			type h1Client struct {
				c  net.Conn
				br *bufio.Reader
			}
			// pingAll writes a request on every client, then reads the
			// answers concurrently, so that the clients that get none cost
			// one wait between them, not one each: the windows below are as
			// long as they are logged as.
			pingAll := func(cls []*h1Client, wait time.Duration) []string {
				out := make([]string, len(cls))
				for i, cl := range cls {
					if _, err := io.WriteString(cl.c, "GET /ping HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
						out[i] = "write: " + err.Error()
					}
				}
				var wg sync.WaitGroup
				for i, cl := range cls {
					if out[i] != "" {
						continue
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						_ = cl.c.SetReadDeadline(time.Now().Add(wait))
						resp, err := http.ReadResponse(cl.br, nil)
						if err != nil {
							out[i] = "read: " + describeH2ReadEnd759(err)
							return
						}
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
						out[i] = "served"
					}()
				}
				wg.Wait()
				return out
			}
			var h1 []*h1Client
			for range 16 {
				c, err := net.Dial("tcp", srv.addr)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				cl := &h1Client{c: c, br: bufio.NewReader(c)}
				if got := pingAll([]*h1Client{cl}, 2*time.Second)[0]; got != "served" {
					t.Fatalf("a keep-alive request before the shutdown: %s", got)
				}
				h1 = append(h1, cl)
			}

			// Detached streams, ticking.
			var ticks atomic.Int64
			for range 16 {
				b, err := net.Dial("tcp", srv.addr)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = b.Close() }()
				if _, err := io.WriteString(b, "GET /stream HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				go func() {
					resp, err := http.ReadResponse(bufio.NewReader(b), nil)
					if err != nil {
						return
					}
					sc := bufio.NewScanner(resp.Body)
					for sc.Scan() {
						ticks.Add(1)
					}
				}()
			}
			for deadline := time.Now().Add(5 * time.Second); ticks.Load() < 32; {
				if time.Now().After(deadline) {
					t.Fatalf("the streams sent %d ticks in 5s before the shutdown", ticks.Load())
				}
				time.Sleep(10 * time.Millisecond)
			}

			begin := time.Now()
			srv.beginShutdown("Shutdown", 20*time.Second)
			// Inside the grace: once every listener is closed, the workers
			// have all reached the pool wait (stopAccepting is its first act),
			// and a request made then is answered. A worker may have noticed
			// the cancel a few ms before the rest, so the assertion is that
			// most of the clients are answered, not all.
			var refusedAt time.Duration
			for deadline := time.Now().Add(5 * time.Second); ; {
				c, err := net.DialTimeout("tcp", srv.addr, 100*time.Millisecond)
				if err != nil {
					refusedAt = time.Since(begin)
					break
				}
				_ = c.Close()
				if time.Now().After(deadline) {
					t.Fatal("the listeners were still open 5s into the shutdown")
				}
				time.Sleep(2 * time.Millisecond)
			}
			inGrace := 0
			for _, got := range pingAll(h1, 500*time.Millisecond) {
				if got == "served" {
					inGrace++
				}
			}
			t.Logf("celeris884 %s: listeners closed %v into the shutdown; %d of %d requests made right after were answered (the grace)",
				e.name, refusedAt.Round(time.Millisecond), inGrace, len(h1))
			if inGrace < len(h1)/2 {
				t.Errorf("%s: only %d of %d requests made right after the listeners closed (%v into the shutdown), inside the grace, were answered",
					e.name, inGrace, len(h1), refusedAt.Round(time.Millisecond))
			}
			time.Sleep(time.Until(begin.Add(1500 * time.Millisecond)))
			before, windowStart := ticks.Load(), time.Now()
			served := 0
			outcomes := pingAll(h1, 300*time.Millisecond)
			for _, got := range outcomes {
				if got == "served" {
					served++
				}
			}
			time.Sleep(time.Until(begin.Add(2400 * time.Millisecond)))
			after, window := ticks.Load(), time.Since(windowStart)
			t.Logf("celeris884 %s: 1.5 s into the shutdown %d of %d keep-alive requests were answered; the detached streams' ticks over the next %v: %d",
				e.name, served, len(h1), window.Round(10*time.Millisecond), after-before)
			if served > 0 {
				t.Errorf("%s: %d of %d keep-alive HTTP/1 requests made 1.5 s into the shutdown, while HTTP/2 handlers were still running, were answered (%v)",
					e.name, served, len(h1), outcomes)
			}
			if n := after - before; n > 4 {
				t.Errorf("%s: the detached streams wrote %d ticks in %v, from 1.5 s into the shutdown, while HTTP/2 handlers were still running", e.name, n, window.Round(10*time.Millisecond))
			}

			// The HTTP/2 streams are not cut by the gate: they finish.
			releaseHeld()
			h2wg.Wait()
			srv.waitStart(t, 25*time.Second)
			bad := 0
			for i, r := range results {
				if r.status != "200" || r.body.String() != "done" || !r.ended {
					bad++
					if bad <= 3 {
						t.Errorf("%s: held HTTP/2 stream %d got status %q body %q ended=%v (%s) after the gate", e.name, i, r.status, r.body.String(), r.ended, r.end)
					}
				}
			}
			if bad > 0 {
				t.Errorf("%s: %d of %d held HTTP/2 streams did not finish", e.name, bad, nH2)
			}
		})
	}
}
