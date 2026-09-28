//go:build linux

package celeris_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
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
// at once, or sends no GOAWAY, fails every time.
func TestShutdownSendsH2GoAwayThenFinishesStreams(t *testing.T) {
	const releaseAfter = 2 * time.Second
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}} {
		for _, mode := range []string{"Shutdown", "cancel"} {
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
							close(release)
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
	const bound = 3 * time.Second
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
				t.Logf("%s/%s: Start returned %v after the shutdown began, with the handler still running", e.name, mode, time.Since(start).Round(time.Millisecond))
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

func (srv *h2PoolServer759) beginShutdown(mode string, budget time.Duration) {
	if mode == "cancel" {
		srv.cancel()
		return
	}
	srv.shutErr = make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
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
