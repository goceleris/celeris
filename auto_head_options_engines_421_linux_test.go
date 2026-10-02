//go:build linux

package celeris_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/cors"
)

// TestAutoHeadOptionsOnEveryEngine421 drives celeris#421 over real connections
// on every engine, HTTP/1.1 and HTTP/2 (h2c), all requests of a run on ONE
// connection, so a response whose framing is wrong breaks the next one:
//
//   - HEAD to a GET route is answered like the GET with no body, whichever way
//     the GET writes it: a Blob, a StreamWriter (chunked on HTTP/1.1, where the
//     chunks and the terminating chunk must not be sent either), and a file
//     (epoll's sendfile path past 16 KiB);
//   - OPTIONS to a path with no OPTIONS route gets 200, the Allow list and
//     Content-Length: 0, and a CORS preflight installed with Server.Use still
//     gets the cors middleware's 204;
//   - explicit HEAD and OPTIONS routes still answer, and a method the path does
//     not answer still gets 405 with the Allow list.
//
// The /stream-head route is an EXPLICIT HEAD route that streams: on main it
// already sent the chunked body on HTTP/1.1 (the bytes then corrupted the next
// response) and DATA frames on HTTP/2; auto-HEAD makes every streaming GET
// route reachable that way.
//
// Every route runs once inline and once as an async route (Route.Async): a
// HEAD answered by an async GET route is dispatched the way the GET is
// (router.routeAsync resolves HEAD to the GET route too).
func TestAutoHeadOptionsOnEveryEngine421(t *testing.T) {
	const fileSize = 64 << 10
	fileBody := bytes.Repeat([]byte("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ!?"), fileSize/64)
	path := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(path, fileBody, 0o600); err != nil {
		t.Fatal(err)
	}
	stream := func(c *celeris.Context) error {
		sw := c.StreamWriter()
		if sw == nil {
			return errors.New("no StreamWriter")
		}
		if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/plain"}}); err != nil {
			return err
		}
		for _, chunk := range []string{"chunk-one|", "chunk-two|"} {
			if _, err := sw.Write([]byte(chunk)); err != nil {
				return err
			}
		}
		return sw.Close()
	}
	const origin = "https://a.example"

	type req421 struct {
		name, method, path string
		hdrs               map[string]string
		status             int
		body               string
		check              func(resp *http.Response) error
	}
	hello := req421{name: "get-hello", method: "GET", path: "/hello", status: 200, body: "hello world"}
	contentLength := func(want int64) func(*http.Response) error {
		return func(resp *http.Response) error {
			if resp.ContentLength != want {
				return fmt.Errorf("content-length %d, want %d", resp.ContentLength, want)
			}
			return nil
		}
	}
	header := func(k, want string) func(*http.Response) error {
		return func(resp *http.Response) error {
			if got := strings.Join(resp.Header.Values(k), "|"); got != want {
				return fmt.Errorf("%s %q, want %q", k, got, want)
			}
			return nil
		}
	}
	reqs := []req421{
		{name: "head-blob", method: "HEAD", path: "/hello", status: 200, check: contentLength(11)},
		hello,
		{name: "head-stream", method: "HEAD", path: "/stream", status: 200},
		hello,
		{name: "head-stream-explicit-route", method: "HEAD", path: "/stream-head", status: 200},
		hello,
		{name: "head-file", method: "HEAD", path: "/file", status: 200, check: contentLength(fileSize)},
		hello,
		{name: "get-stream", method: "GET", path: "/stream", status: 200, body: "chunk-one|chunk-two|"},
		{name: "options-auto", method: "OPTIONS", path: "/hello", status: 200, check: func(resp *http.Response) error {
			if err := header("Allow", "GET, HEAD, OPTIONS")(resp); err != nil {
				return err
			}
			return contentLength(0)(resp)
		}},
		{name: "options-cors-preflight", method: "OPTIONS", path: "/hello",
			hdrs:   map[string]string{"Origin": origin, "Access-Control-Request-Method": "GET"},
			status: 204, check: header("Access-Control-Allow-Origin", origin)},
		{name: "head-explicit", method: "HEAD", path: "/eh", status: 200, check: header("X-Explicit", "head")},
		{name: "options-explicit", method: "OPTIONS", path: "/eo", status: 204, check: header("X-Explicit", "options")},
		{name: "delete-is-405", method: "DELETE", path: "/hello", status: 405, body: "405 Method Not Allowed",
			check: header("Allow", "GET, HEAD, OPTIONS")},
		hello,
	}

	for _, e := range engines761 {
		for _, route := range []string{"sync", "async-route"} {
			t.Run(e.name+"/"+route, func(t *testing.T) {
				addr := startServer421(t, e.eng, func(s *celeris.Server) {
					s.Use(cors.New(cors.Config{AllowOrigins: []string{origin}}))
					routes := []*celeris.Route{
						s.GET("/hello", func(c *celeris.Context) error { return c.String(200, "hello world") }),
						s.GET("/stream", stream),
						s.HEAD("/stream-head", stream),
						s.GET("/file", func(c *celeris.Context) error { return c.File(path) }),
						s.GET("/eh", func(c *celeris.Context) error { return c.String(200, "get eh") }),
						s.HEAD("/eh", func(c *celeris.Context) error {
							c.SetHeader("x-explicit", "head")
							return c.String(200, "head eh")
						}),
						s.GET("/eo", func(c *celeris.Context) error { return c.String(200, "get eo") }),
						s.OPTIONS("/eo", func(c *celeris.Context) error {
							c.SetHeader("x-explicit", "options")
							return c.NoContent(204)
						}),
					}
					if route == "async-route" {
						for _, r := range routes {
							r.Async()
						}
					}
				})
				for _, proto := range []string{"h1", "h2"} {
					t.Run(proto, func(t *testing.T) {
						tr := &http.Transport{MaxConnsPerHost: 1, DisableCompression: true}
						if proto == "h2" {
							p := new(http.Protocols)
							p.SetUnencryptedHTTP2(true)
							tr.Protocols = p
						}
						var dials atomic.Int32
						tr.DialContext = func(ctx context.Context, network, a string) (net.Conn, error) {
							dials.Add(1)
							var d net.Dialer
							return d.DialContext(ctx, network, a)
						}
						cl := &http.Client{Timeout: 10 * time.Second, Transport: tr}
						defer cl.CloseIdleConnections()
						wantMajor := map[string]int{"h1": 1, "h2": 2}[proto]
						for i, rq := range reqs {
							desc := fmt.Sprintf("%s/%s/%s #%d %s %s %s", e.name, route, proto, i, rq.name, rq.method, rq.path)
							hr, err := http.NewRequest(rq.method, "http://"+addr+rq.path, nil)
							if err != nil {
								t.Fatal(err)
							}
							for k, v := range rq.hdrs {
								hr.Header.Set(k, v)
							}
							resp, err := cl.Do(hr)
							if err != nil {
								t.Errorf("%s: %v", desc, err)
								continue
							}
							got, err := io.ReadAll(resp.Body)
							_ = resp.Body.Close()
							switch {
							case err != nil:
								t.Errorf("%s: reading the body: %v", desc, err)
							case resp.ProtoMajor != wantMajor:
								t.Errorf("%s: answered over %s", desc, resp.Proto)
							case resp.StatusCode != rq.status || string(got) != rq.body:
								t.Errorf("%s: %d %q, want %d %q", desc, resp.StatusCode, got, rq.status, rq.body)
							case rq.check != nil:
								if err := rq.check(resp); err != nil {
									t.Errorf("%s: %v", desc, err)
								}
							}
						}
						if n := dials.Load(); n != 1 {
							t.Errorf("%s/%s/%s: %d connections for %d requests, want 1 (a response's framing broke the connection)",
								e.name, route, proto, n, len(reqs))
						}
					})
				}
				// net/http's HTTP/2 client drops DATA on a HEAD stream without
				// an error, so the frames are counted on a raw connection.
				t.Run("h2-raw-head-sends-no-data", func(t *testing.T) {
					for _, p := range []string{"/hello", "/stream", "/stream-head", "/file"} {
						status, data, err := h2HeadDataBytes421(addr, p)
						if err != nil || status != "200" || data != 0 {
							t.Errorf("%s/%s: raw h2c HEAD %s: :status %q, %d DATA payload bytes, err %v; want 200, 0, nil",
								e.name, route, p, status, data, err)
						}
					}
				})
			})
		}
	}
}

// h2HeadDataBytes421 sends one HEAD on a fresh h2c (prior knowledge)
// connection and returns the response's :status and the DATA payload bytes
// received on the stream until it ends. From the response's HEADERS on it
// sends a PING every 50 ms: an async route's frames can sit in the write
// queue until the event loop next wakes (celeris#837, a lost wakeup that GET
// streams hit too, and a single PING can land in the same window), and each
// PING wakes the loop, so any DATA the server queued still arrives and counts.
func h2HeadDataBytes421(addr, path string) (status string, data int, err error) {
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
	var wmu sync.Mutex // the Framer's writes share one buffer: the pinger and the reader both write
	stopPings := make(chan struct{})
	defer close(stopPings)
	pinging := false
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
				wmu.Lock()
				err := fr.WriteSettingsAck()
				wmu.Unlock()
				if err != nil {
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
			if !pinging {
				pinging = true
				go func() {
					tick := time.NewTicker(50 * time.Millisecond)
					defer tick.Stop()
					for {
						select {
						case <-stopPings:
							return
						case <-tick.C:
							wmu.Lock()
							err := fr.WritePing(false, [8]byte{4, 2, 1})
							wmu.Unlock()
							if err != nil {
								return
							}
						}
					}
				}()
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
			return status, data, fmt.Errorf("GOAWAY %v", f.ErrCode)
		}
	}
}

// startServer421 is startServer761 with setup run before any route exists,
// so it may call Server.Use.
func startServer421(t *testing.T, eng celeris.EngineType, setup func(*celeris.Server)) string {
	t.Helper()
	retryUntil := time.Now().Add(30 * time.Second)
	for tries := 1; ; tries++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		s := celeris.New(celeris.Config{Engine: eng, Addr: addr})
		setup(s)
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		startDone := make(chan error, 1)
		go func() { startDone <- s.Start() }()
		err = waitReady761(addr, startDone)
		if err == nil {
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = s.Shutdown(ctx)
				select {
				case <-startDone:
				case <-time.After(15 * time.Second):
					t.Errorf("Start did not return within 15s of Shutdown")
				}
			})
			return addr
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(retryUntil) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start (try %d): %v", tries, err)
	}
}
