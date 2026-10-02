//go:build linux

package sse_test

import (
	"bufio"
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

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

// startServer835 starts a server on eng with routes and stops it at cleanup.
// It never skips: an engine that cannot start fails the test (a skip would
// read as a pass). A ring ENOMEM at start (the kernel returns ring memory to
// RLIMIT_MEMLOCK asynchronously after the previous engine's rings close) is
// retried for 30 s.
func startServer835(t *testing.T, eng celeris.EngineType, routes func(*celeris.Server)) string {
	t.Helper()
	retryUntil := time.Now().Add(30 * time.Second)
	for {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		s := celeris.New(celeris.Config{Engine: eng, Addr: addr, ShutdownTimeout: 2 * time.Second})
		s.GET("/ping835", func(c *celeris.Context) error { return c.String(200, "ok") })
		routes(s)
		startDone := make(chan error, 1)
		go func() { startDone <- s.Start() }()
		err = waitReady835(addr, startDone)
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
		t.Fatalf("%v server did not start: %v", eng, err)
	}
}

func waitReady835(addr string, startDone <-chan error) error {
	deadline := time.Now().Add(15 * time.Second)
	cl := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		select {
		case err := <-startDone:
			if err == nil {
				err = errors.New("Start returned nil before the server was ready")
			}
			return err
		default:
		}
		resp, err := cl.Get("http://" + addr + "/ping835")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("server not ready within 15s")
}

// TestOnConnectRejectionReachesClient835 is celeris#835 on the wire. sse.New
// takes the StreamWriter before it runs OnConnect, and taking it marked the
// response written, so the error OnConnect returned to reject a client was
// dropped: epoll, io_uring and adaptive sent nothing on HTTP/1.1 (the client
// hung until its timeout), and std and HTTP/2 answered an empty 200. The
// config documents "Return a non-nil error to reject the connection".
//
// Every engine, HTTP/1.1 and h2c, GET and an explicit HEAD route: the client
// must get the 401 (with its body, except for HEAD). The HTTP/1.1 connection
// must then still serve /ping835. /events835-ok is the control on the same
// server: an OnConnect that accepts still gets its stream and first event.
func TestOnConnectRejectionReachesClient835(t *testing.T) {
	engines := []struct {
		name string
		eng  celeris.EngineType
	}{{"std", celeris.Std}, {"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}}
	for _, e := range engines {
		t.Run(e.name, func(t *testing.T) {
			var rejectedHandlerRuns atomic.Int32
			defer func() {
				if n := rejectedHandlerRuns.Load(); n != 0 {
					t.Errorf("%s: Handler ran %d times for a client OnConnect rejected", e.name, n)
				}
			}()
			addr := startServer835(t, e.eng, func(s *celeris.Server) {
				reject := sse.New(sse.Config{
					HeartbeatInterval: -1,
					OnConnect: func(*celeris.Context, *sse.Client) error {
						return celeris.NewHTTPError(401, "denied835")
					},
					Handler: func(*sse.Client) { rejectedHandlerRuns.Add(1) },
				})
				s.GET("/events835", reject).Async()
				s.HEAD("/events835", reject).Async()
				s.GET("/events835-ok", sse.New(sse.Config{
					HeartbeatInterval: -1,
					OnConnect:         func(*celeris.Context, *sse.Client) error { return nil },
					Handler: func(client *sse.Client) {
						_ = client.Send(sse.Event{Event: "hello", Data: "835"})
						<-client.Context().Done()
					},
				})).Async()
			})

			t.Run("h1", func(t *testing.T) {
				conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conn.Close() }()
				br := bufio.NewReader(conn)
				for _, method := range []string{"GET", "HEAD"} {
					wantBody := "denied835"
					if method == "HEAD" {
						wantBody = ""
					}
					desc := fmt.Sprintf("%s h1 %s /events835 (OnConnect rejects with 401)", e.name, method)
					status, body, err := h1Do835(conn, br, method, "/events835")
					if err != nil {
						t.Fatalf("%s: %v", desc, err)
					}
					if status != 401 || body != wantBody {
						t.Errorf("%s: %d %q, want 401 %q", desc, status, body, wantBody)
					}
					if status, body, err := h1Do835(conn, br, "GET", "/ping835"); err != nil || status != 200 || body != "ok" {
						t.Fatalf("%s: then /ping835 on the same connection: %d %q %v", desc, status, body, err)
					}
				}
				checkAcceptedStream835(t, e.name, addr)
			})

			t.Run("h2c", func(t *testing.T) {
				p := new(http.Protocols)
				p.SetUnencryptedHTTP2(true)
				cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Protocols: p}}
				defer cl.CloseIdleConnections()
				for _, method := range []string{"GET", "HEAD"} {
					wantBody := "denied835"
					if method == "HEAD" {
						wantBody = ""
					}
					desc := fmt.Sprintf("%s h2c %s /events835 (OnConnect rejects with 401)", e.name, method)
					req, err := http.NewRequestWithContext(context.Background(), method, "http://"+addr+"/events835", nil)
					if err != nil {
						t.Fatal(err)
					}
					resp, err := cl.Do(req)
					if err != nil {
						t.Errorf("%s: %v", desc, err)
						continue
					}
					b, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if resp.ProtoMajor != 2 {
						t.Errorf("%s: answered over %s, want HTTP/2", desc, resp.Proto)
					}
					if err != nil || resp.StatusCode != 401 || string(b) != wantBody {
						t.Errorf("%s: %d %q content-type %q (read error %v), want 401 %q",
							desc, resp.StatusCode, b, resp.Header.Get("content-type"), err, wantBody)
					}
				}
			})
		})
	}
}

// checkAcceptedStream835 is the control: an SSE route whose OnConnect accepts
// still streams (status 200, text/event-stream, the first event).
func checkAcceptedStream835(t *testing.T, engine, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET /events835-ok HTTP/1.1\r\nHost: x\r\nAccept: text/event-stream\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("%s: accepted stream: no response: %v", engine, err)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("content-type"), "text/event-stream") {
		t.Fatalf("%s: accepted stream: %d content-type %q, want 200 text/event-stream", engine, resp.StatusCode, resp.Header.Get("content-type"))
	}
	ebr := bufio.NewReader(resp.Body)
	var lines []string
	for len(lines) < 2 {
		line, err := ebr.ReadString('\n')
		if err != nil {
			t.Fatalf("%s: accepted stream: read %q, then %v", engine, lines, err)
		}
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			lines = append(lines, line)
		}
	}
	if lines[0] != "event: hello" || lines[1] != "data: 835" {
		t.Fatalf("%s: accepted stream: first event %q, want [event: hello data: 835]", engine, lines)
	}
}

// h1Do835 sends one request on conn and reads its response with a 3 s
// deadline, so a response that never comes fails the case instead of hanging.
func h1Do835(conn net.Conn, br *bufio.Reader, method, path string) (int, string, error) {
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: x\r\n\r\n", method, path); err != nil {
		return 0, "", err
	}
	resp, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return 0, "", fmt.Errorf("no response within 3 s: %w", err)
		}
		return 0, "", err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return resp.StatusCode, string(b), fmt.Errorf("body: %w", err)
	}
	return resp.StatusCode, string(b), nil
}
