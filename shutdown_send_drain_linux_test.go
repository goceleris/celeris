//go:build linux

package celeris_test

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
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestShutdownSendsTheWholeResponse pins celeris#760: epoll's shutdown closed
// every connection as soon as the handlers had returned, with whatever the
// socket had not taken yet still queued, so a response larger than the socket
// buffers, to a client that reads more slowly than the loop shuts down, lost
// its tail and the client got EOF in the middle of the body. io_uring drained
// its sends first (celeris#595) and std drains through net/http.
//
// The client has a 64 KiB receive buffer and starts reading readDelay after
// the handler returns, which is 200 ms into the shutdown, well past the
// drain's 250 ms floor, so the loop is
// certain to reach its shutdown with most of the 3 MiB still queued. The
// shutdown's budget is 30 s, so a drain bounded by it has all the time it
// needs; a drain that closes at once cuts the body short every time. The
// ways of shutting down are a direct Shutdown with that budget, a direct
// Shutdown(context.Background()), whose ctx has no deadline at all (net/http's
// "wait as long as it takes", which the drain first took for no budget and
// gave its 250 ms floor), and a cancel of StartWithContext's context; both
// kinds of route (the handler on the worker, and on a dispatch goroutine) are
// covered. io_uring is not: its own drain (celeris#595)
// gives up after 250 ms whatever the budget, before this client reads, and
// whether the tail survives then depends on what the kernel has taken
// (celeris#806).
func TestShutdownSendsTheWholeResponse(t *testing.T) {
	const size = 3 << 20
	const readDelay = 500 * time.Millisecond
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i*7 + i>>13)
	}
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"std", celeris.Std}, {"epoll", celeris.Epoll}, {"adaptive", celeris.Adaptive}} {
		for _, route := range []string{"sync", "async-route"} {
			for _, mode := range []string{"Shutdown", "Shutdown-background", "cancel"} {
				t.Run(e.name+"/"+route+"/"+mode, func(t *testing.T) {
					desc := e.name + "/" + route + "/" + mode
					entered := make(chan struct{})
					release := make(chan struct{})
					srv := startServer760(t, e.eng, 30*time.Second, 0, func(s *celeris.Server) {
						r := s.GET("/big", func(c *celeris.Context) error {
							close(entered)
							<-release
							return c.Blob(http.StatusOK, "application/octet-stream", body)
						})
						if route == "async-route" {
							r.Async()
						}
					})
					c := dialSlowReader760(t, srv.addr)
					if _, err := io.WriteString(c, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
						t.Fatal(err)
					}
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("handler did not start within 5s")
					}
					shutErr := srv.beginShutdown(mode, 30*time.Second)
					time.Sleep(200 * time.Millisecond)
					close(release)
					time.Sleep(readDelay)
					got, n, end := readResponse760(c)
					if !bytes.Equal(got, body) {
						t.Errorf("%s: the client got %d of %d body bytes (%d in all), then %s", desc, len(got), size, n, end)
					}
					if err := srv.waitShutdown(mode, shutErr, 20*time.Second); err != nil {
						t.Errorf("%s: %v", desc, err)
					}
				})
			}
		}
	}
}

// TestShutdownSendDrainIsBounded is the other half of the contract: a client
// that never reads must not hold epoll's shutdown past its budget. The drain
// gives up when the shutdown's budget runs out, never before 250 ms
// (io_uring's bound, celeris#595), and the Start call returns within bound of
// the budget. A ctx with no deadline bounds the drain by its cancel
// ("Shutdown-withcancel", cancelled budget into the shutdown), and, when
// nothing cancels it (context.Background()), by the config's WriteTimeout,
// the bound a live conn's stalled write gets ("Shutdown-background", with
// WriteTimeout = budget): a client that never reads cannot hold that Shutdown
// for ever. io_uring is not asserted: its own drain returns about 10 s late
// with a stalled send whatever the budget (celeris#806). std's handler writes
// the response itself and blocks in that write; net/http's WriteTimeout is
// its bound.
func TestShutdownSendDrainIsBounded(t *testing.T) {
	// Larger than the socket buffers, so part of it stays queued while the
	// client does not read, and smaller than the 4 MiB write cap, so the
	// native engines queue it whole (celeris#761 is a different defect).
	const size = 3 << 20
	const budget = 500 * time.Millisecond
	const bound = time.Second
	body := make([]byte, size)
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"adaptive", celeris.Adaptive}} {
		for _, mode := range []string{"Shutdown", "Shutdown-withcancel", "Shutdown-background", "cancel"} {
			t.Run(e.name+"/"+mode, func(t *testing.T) {
				desc := e.name + "/" + mode
				served := make(chan struct{})
				var writeTimeout time.Duration
				if mode == "Shutdown-background" {
					writeTimeout = budget
				}
				srv := startServer760(t, e.eng, budget, writeTimeout, func(s *celeris.Server) {
					s.GET("/big", func(c *celeris.Context) error {
						defer close(served) // native engines: the body is queued, not written, here
						return c.Blob(http.StatusOK, "application/octet-stream", body)
					})
				})
				c := dialSlowReader760(t, srv.addr)
				if _, err := io.WriteString(c, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				select {
				case <-served:
				case <-time.After(5 * time.Second):
					t.Fatal("handler did not run within 5s")
				}
				time.Sleep(100 * time.Millisecond) // the response is queued behind a full socket
				start := time.Now()
				shutErr := srv.beginShutdown(mode, budget)
				if err := srv.waitShutdown(mode, shutErr, budget+bound); err != nil {
					t.Fatalf("%s: %v (a drain the stalled client can hold open past the budget)", desc, err)
				}
				t.Logf("%s: Start returned %v after the shutdown began", desc, time.Since(start).Round(time.Millisecond))
				// The engine has stopped, so the connection must be closed:
				// the client, which has read nothing so far, reads what the
				// kernel took and then EOF (or, where the kernel took it
				// all, the whole body), never an open connection.
				got, n, end := readResponse760(c)
				if end != "EOF" && end != "the end of the body" {
					t.Errorf("%s: after the shutdown the client got %d of %d body bytes (%d in all), then %s", desc, len(got), size, n, end)
				}
			})
		}
	}
}

type server760 struct {
	s         *celeris.Server
	addr      string
	cancel    context.CancelFunc
	startDone chan error
}

// startServer760 starts a server with routes on StartWithContext, with
// ShutdownTimeout budget (and WriteTimeout writeTimeout, 0 for the default),
// and waits until it answers /ping. An io_uring start
// that fails only with ENOMEM is retried, with a new server, for up to 30 s:
// the kernel charges ring memory to RLIMIT_MEMLOCK per UID and gives it back
// some milliseconds after a ring closes, so at the CI runner's 8 MiB a start
// made right after the previous server stopped can fail although nothing
// leaked (see startC714DetachServer).
func startServer760(t *testing.T, eng celeris.EngineType, budget, writeTimeout time.Duration, routes func(*celeris.Server)) *server760 {
	t.Helper()
	retryUntil := time.Now().Add(30 * time.Second)
	for tries := 1; ; tries++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		s := celeris.New(celeris.Config{Engine: eng, Addr: addr, ShutdownTimeout: budget, WriteTimeout: writeTimeout})
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		routes(s)
		ctx, cancel := context.WithCancel(context.Background())
		srv := &server760{s: s, addr: addr, cancel: cancel, startDone: make(chan error, 1)}
		go func() { srv.startDone <- s.StartWithContext(ctx) }()
		err = waitReady760(addr, srv.startDone)
		if err == nil {
			if tries > 1 {
				t.Logf("server start retried on ring ENOMEM: %d tries", tries)
			}
			t.Cleanup(func() {
				cancel()
				select {
				case <-srv.startDone:
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

// waitReady760 polls /ping until it answers 200, or returns the error the
// start returned first.
func waitReady760(addr string, startDone <-chan error) error {
	probe := &http.Client{Timeout: 300 * time.Millisecond}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		select {
		case err := <-startDone:
			if err == nil {
				err = errors.New("StartWithContext returned nil before the server was ready")
			}
			return err
		default:
		}
		if resp, err := probe.Get("http://" + addr + "/ping"); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("no answer on /ping at %s within 15s", addr)
}

// beginShutdown starts the shutdown: a direct Shutdown with a budget of its
// own ("Shutdown"), a direct Shutdown whose ctx has no deadline, never
// cancelled ("Shutdown-background") or cancelled budget into the shutdown
// ("Shutdown-withcancel"), or a cancel of StartWithContext's context (whose
// budget is the server's ShutdownTimeout). The returned channel carries the
// direct Shutdown's error; it is nil for a cancel.
func (srv *server760) beginShutdown(mode string, budget time.Duration) chan error {
	if mode == "cancel" {
		srv.cancel()
		return nil
	}
	ch := make(chan error, 1)
	go func() {
		var ctx context.Context
		var cancel context.CancelFunc
		switch mode {
		case "Shutdown-background":
			ctx, cancel = context.Background(), func() {}
		case "Shutdown-withcancel":
			ctx, cancel = context.WithCancel(context.Background())
			time.AfterFunc(budget, cancel)
		default:
			ctx, cancel = context.WithTimeout(context.Background(), budget)
		}
		defer cancel()
		ch <- srv.s.Shutdown(ctx)
	}()
	return ch
}

// waitShutdown waits up to limit for the Start call (and a direct Shutdown)
// to return.
func (srv *server760) waitShutdown(mode string, shutErr chan error, limit time.Duration) error {
	deadline := time.After(limit)
	if shutErr != nil {
		select {
		case <-shutErr:
		case <-deadline:
			return fmt.Errorf("Shutdown had not returned %v after it began", limit)
		}
	}
	select {
	case err := <-srv.startDone:
		srv.startDone <- err // for the cleanup
		return nil
	case <-deadline:
		return fmt.Errorf("StartWithContext had not returned %v after the shutdown (%s) began", limit, mode)
	}
}

// dialSlowReader760 dials a raw connection with a 64 KiB receive buffer, so
// the server's socket takes little of a large response until the client
// reads.
func dialSlowReader760(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.TCPConn).SetReadBuffer(64 << 10)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// readResponse760 reads one HTTP/1.1 response from c, giving up after 5 s
// without a byte, and returns its body, the bytes received in all, and how
// the read ended.
func readResponse760(c net.Conn) (body []byte, total int, end string) {
	cr := &countReader760{c: c}
	resp, err := http.ReadResponse(bufio.NewReaderSize(cr, 64<<10), nil)
	if err != nil {
		return nil, cr.n, describeEnd760(err)
	}
	body, err = io.ReadAll(resp.Body)
	return body, cr.n, describeEnd760(err)
}

type countReader760 struct {
	c net.Conn
	n int
}

func (r *countReader760) Read(p []byte) (int, error) {
	_ = r.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := r.c.Read(p)
	r.n += n
	return n, err
}

func describeEnd760(err error) string {
	var ne net.Error
	switch {
	case err == nil:
		return "the end of the body"
	case errors.As(err, &ne) && ne.Timeout():
		return "no byte for 5s, connection still open"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "EOF"
	}
	return err.Error()
}
