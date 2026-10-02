//go:build linux

package sse_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

// TestSSEHeadGetsHeadersOnly421: since celeris#421 a HEAD request to an SSE
// GET route reaches the middleware. It must answer with the stream's headers
// and end the response there (no event, no detached stream that never ends),
// without running the Handler, so the connection serves its next request.
// OnConnect still gates it: a rejection gives HEAD the status GET gets (401
// here), and an accepted HEAD runs OnDisconnect once, as a stream that ends
// at once would. HTTP/1.1 keep-alive and HTTP/2 (h2c, raw frames) on every
// engine, with and without AsyncHandlers.
func TestSSEHeadGetsHeadersOnly421(t *testing.T) {
	engines := []struct {
		name string
		eng  celeris.EngineType
	}{{"std", celeris.Std}, {"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}}
	for _, e := range engines {
		for _, async := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/async=%v", e.name, async), func(t *testing.T) {
				var handlerRuns, connects, disconnects atomic.Int32
				addr, stop := startSSEHead421(t, e.eng, async, sse.New(sse.Config{
					HeartbeatInterval: -1,
					OnConnect: func(c *celeris.Context, _ *sse.Client) error {
						connects.Add(1)
						if c.Header("x-deny") != "" {
							return celeris.NewHTTPError(401, "denied")
						}
						return nil
					},
					OnDisconnect: func(_ *celeris.Context, _ *sse.Client) { disconnects.Add(1) },
					Handler: func(client *sse.Client) {
						handlerRuns.Add(1)
						tick := time.NewTicker(20 * time.Millisecond)
						defer tick.Stop()
						for {
							select {
							case <-client.Context().Done():
								return
							case <-tick.C:
								if err := client.Send(sse.Event{Event: "tick", Data: "x"}); err != nil {
									return
								}
							}
						}
					},
				}))
				defer stop()

				// HTTP/1.1: HEAD then GET /ping on the same connection.
				conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				br := bufio.NewReader(conn)
				if _, err := io.WriteString(conn, "HEAD /events HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				resp, err := http.ReadResponse(br, &http.Request{Method: "HEAD"})
				if err != nil {
					t.Fatalf("HEAD /events: %v", err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
					t.Fatalf("HEAD /events: %d content-type %q, want 200 text/event-stream", resp.StatusCode, resp.Header.Get("Content-Type"))
				}
				// OnConnect rejects: HEAD gets GET's 401, on the same connection.
				if _, err := io.WriteString(conn, "HEAD /events HTTP/1.1\r\nHost: x\r\nX-Deny: 1\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				resp, err = http.ReadResponse(br, &http.Request{Method: "HEAD"})
				if err != nil {
					t.Fatalf("HEAD /events (denied): %v", err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != 401 {
					t.Fatalf("HEAD /events with OnConnect rejecting: %d, want the 401 GET gets", resp.StatusCode)
				}
				if _, err := io.WriteString(conn, "GET /ping HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				resp, err = http.ReadResponse(br, &http.Request{Method: "GET"})
				if err != nil {
					t.Fatalf("GET /ping after HEAD /events on the same connection: %v", err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || resp.StatusCode != 200 || string(body) != "pong" {
					t.Fatalf("GET /ping after HEAD /events: %d %q %v", resp.StatusCode, body, err)
				}

				// HTTP/2: the HEAD stream ends with no DATA payload.
				status, data, err := h2HeadSSE421(addr, "/events")
				if err != nil || status != "200" || data != 0 {
					t.Fatalf("h2c HEAD /events: :status %q, %d DATA payload bytes, err %v; want 200, 0, nil", status, data, err)
				}
				if n := handlerRuns.Load(); n != 0 {
					t.Fatalf("the SSE Handler ran %d times for HEAD", n)
				}
				// Three HEADs reached OnConnect (h1, h1 denied, h2); the two it
				// accepted each ran OnDisconnect once. The h2 stream can end
				// on the wire before its handler has returned, so give the
				// last OnDisconnect a moment.
				for end := time.Now().Add(2 * time.Second); disconnects.Load() < 2 && time.Now().Before(end); {
					time.Sleep(5 * time.Millisecond)
				}
				if c, d := connects.Load(), disconnects.Load(); c != 3 || d != 2 {
					t.Fatalf("OnConnect ran %d times, OnDisconnect %d; want 3 and 2", c, d)
				}
			})
		}
	}
}

func startSSEHead421(t *testing.T, eng celeris.EngineType, async bool, h celeris.HandlerFunc) (string, func()) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		s := celeris.New(celeris.Config{Engine: eng, AsyncHandlers: async, ShutdownTimeout: 2 * time.Second})
		s.GET("/events", h)
		s.GET("/ping", func(c *celeris.Context) error { return c.String(200, "pong") })
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
		addr, err := c714WaitReady(s, done)
		if err == nil {
			// The native engines re-bind the port; let them settle the way
			// the other engine tests here do before the first request.
			if err := pingReady421(addr); err != nil {
				cancel()
				<-done
				t.Fatalf("server not serving: %v", err)
			}
			return addr, func() { cancel(); <-done }
		}
		cancel()
		_ = ln.Close()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start: %v", err)
	}
}

func pingReady421(addr string) error {
	cl := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
	var last error
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		resp, err := cl.Get("http://" + addr + "/ping")
		if err != nil {
			last = err
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == 200 {
			return nil
		}
		last = fmt.Errorf("status %d", resp.StatusCode)
	}
	return last
}

// h2HeadSSE421 sends one HEAD on a fresh h2c connection and returns the
// :status and the DATA payload bytes received before the stream ends.
func h2HeadSSE421(addr, path string) (status string, data int, err error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		return "", 0, err
	}
	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(); err != nil {
		return "", 0, err
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "HEAD"}, {":scheme", "http"}, {":authority", addr}, {":path", path}} {
		if err := enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]}); err != nil {
			return "", 0, err
		}
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		return "", 0, err
	}
	dec := hpack.NewDecoder(4096, nil)
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return status, data, err
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				if err := fr.WriteSettingsAck(); err != nil {
					return status, data, err
				}
			}
		case *http2.HeadersFrame:
			if f.StreamID != 1 {
				continue
			}
			fields, err := dec.DecodeFull(f.HeaderBlockFragment())
			if err != nil {
				return status, data, err
			}
			for _, hf := range fields {
				if hf.Name == ":status" {
					status = hf.Value
				}
			}
			if f.StreamEnded() {
				return status, data, nil
			}
		case *http2.DataFrame:
			if f.StreamID != 1 {
				continue
			}
			data += len(f.Data())
			if f.StreamEnded() {
				return status, data, nil
			}
		case *http2.RSTStreamFrame:
			return status, data, fmt.Errorf("RST_STREAM %v", f.ErrCode)
		case *http2.GoAwayFrame:
			return status, data, errors.New("GOAWAY " + f.ErrCode.String())
		}
	}
}
