//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
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
// covered. So is io_uring, directly and as the engine Adaptive started on
// (celeris#806): its drain (celeris#595) gave up after 250 ms whatever the
// budget, before this client reads, and whether the tail survived then
// depended on what the kernel had taken.
func TestShutdownSendsTheWholeResponse(t *testing.T) {
	const size = 3 << 20
	const readDelay = 500 * time.Millisecond
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i*7 + i>>13)
	}
	for _, e := range drainEngines806 {
		for _, route := range []string{"sync", "async-route"} {
			for _, mode := range []string{"Shutdown", "Shutdown-background", "cancel"} {
				if e.slim && mode == "Shutdown-background" {
					continue
				}
				for _, sb := range sndBufs806 {
					if e.slim != (sb.apply != nil) {
						continue // slim engines: only the small send buffer; the others only the default
					}
					t.Run(e.name+"/"+route+"/"+mode+sb.suffix, func(t *testing.T) {
						desc := e.name + "/" + route + "/" + mode + sb.suffix
						entered := make(chan struct{})
						release := make(chan struct{})
						srv := startDrainServer806(t, e, 30*time.Second, 0, sb.apply, func(s *celeris.Server) {
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
// for ever. io_uring, directly and as the engine Adaptive started on, is
// asserted too: its drain returned about 10 s late with a stalled send
// whatever the budget (celeris#806). std's handler writes the response
// itself and blocks in that write; net/http's WriteTimeout is its bound.
func TestShutdownSendDrainIsBounded(t *testing.T) {
	// Larger than the socket buffers, so part of it stays queued while the
	// client does not read, and smaller than the 4 MiB write cap, so the
	// native engines queue it whole (celeris#761 is a different defect).
	const size = 3 << 20
	const budget = 500 * time.Millisecond
	const bound = time.Second
	body := make([]byte, size)
	for _, e := range drainEngines806 {
		if e.name == "std" {
			continue
		}
		for _, mode := range []string{"Shutdown", "Shutdown-withcancel", "Shutdown-background", "cancel"} {
			if e.slim && (mode == "Shutdown" || mode == "Shutdown-withcancel") {
				continue // slim engines: the WriteTimeout bound and the cancel
			}
			for _, reader := range readers806(e, mode) {
				t.Run(e.name+"/"+mode+reader, func(t *testing.T) {
					desc := e.name + "/" + mode + reader
					served := make(chan struct{})
					var writeTimeout time.Duration
					if mode == "Shutdown-background" {
						writeTimeout = budget
					}
					logs := &syncBuf806{}
					srv := startDrainServer806(t, e, budget, writeTimeout, func(c *celeris.Config) {
						c.Logger = slog.New(slog.NewTextHandler(logs, nil))
						if e.slim {
							// The kernel cannot hold the response, so the cut the
							// WARN below reports is certain, not host luck.
							sndBufs806[1].apply(c)
						}
					}, func(s *celeris.Server) {
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
					if reader == "/trickle" {
						// The client takes some of the response 100 ms into the
						// drain, then stops again: a partial SEND completion in
						// the drain, whose remainder the loop submits there, and
						// the wait after that submit must still be bounded.
						time.Sleep(100 * time.Millisecond)
						buf := make([]byte, 32<<10)
						for got := 0; got < 256<<10; {
							_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
							n, err := c.Read(buf)
							got += n
							if err != nil {
								t.Fatalf("%s: the trickling client's read: %v", desc, err)
							}
						}
					}
					if err := srv.waitShutdown(mode, shutErr, budget+bound); err != nil {
						t.Fatalf("%s: %v (a drain the stalled client can hold open past the budget)", desc, err)
					}
					t.Logf("%s: Start returned %v after the shutdown began", desc, time.Since(start).Round(time.Millisecond))
					// The engine has stopped, so the connection must be closed:
					// the client, which has read nothing so far, reads what the
					// kernel took and then EOF (or, where the kernel took it
					// all, the whole body), never an open connection.
					if reader == "/trickle" {
						// Part of the stream was read above, so it no longer
						// parses as a response: read what is left to the end.
						_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
						if _, err := io.Copy(io.Discard, c); err != nil {
							t.Errorf("%s: after the shutdown the rest of the stream ended with %v, want EOF", desc, err)
						}
					} else if got, n, end := readResponse760(c); end != "EOF" && end != "the end of the body" {
						t.Errorf("%s: after the shutdown the client got %d of %d body bytes (%d in all), then %s", desc, len(got), size, n, end)
					}
					// The io_uring engine states the data loss the bound is: a
					// WARN naming the connections and the queued bytes cut.
					if e.slim && !strings.Contains(logs.String(), "the send drain ran out of time") {
						t.Errorf("%s: the engine cut a response the client had not taken and logged nothing about it:\n%s", desc, logs.String())
					}
				})
			}
		}
	}
}

// readers806 are the client behaviours of the bounded test for e in mode: the
// stalled one always; the slim engines add a client that reads a part of the
// response during the drain and stops (in the cancel mode only).
func readers806(e drainEngine806, mode string) []string {
	if e.slim && mode == "cancel" {
		return []string{"", "/trickle"}
	}
	return []string{""}
}

// TestShutdownSendDrainStopsAccepting806: a send drain that can last the
// whole budget does not go on serving new connections once its first 250 ms
// are over (a connection accepted in it would be cut at its end), as epoll's,
// whose loops have stopped, and std's, whose listener is closed, do not.
// Within those 250 ms io_uring still accepts (celeris#595), which this does
// not pin. The probe is a fresh connection 600 ms into the drain. io_uring
// only: the others do not accept at all by then, and the root package's race
// run has little time to spare.
func TestShutdownSendDrainStopsAccepting806(t *testing.T) {
	const budget = 2 * time.Second
	body := make([]byte, 3<<20)
	for _, e := range drainEngines806 {
		if e.name != "io_uring" {
			continue
		}
		t.Run(e.name, func(t *testing.T) {
			served := make(chan struct{})
			srv := startDrainServer806(t, e, budget, 0, nil, func(s *celeris.Server) {
				s.GET("/big", func(c *celeris.Context) error {
					defer close(served)
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
			time.Sleep(100 * time.Millisecond)
			shutErr := srv.beginShutdown("Shutdown", budget)
			time.Sleep(600 * time.Millisecond)
			probe := &http.Client{Timeout: 400 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
			resp, err := probe.Get("http://" + srv.addr + "/ping")
			if err == nil {
				_ = resp.Body.Close()
				t.Errorf("%s: a new connection 600 ms into a send drain that lasts %v was answered %d: the engine is still accepting", e.name, budget, resp.StatusCode)
			} else {
				t.Logf("%s: the probe 600 ms into the drain failed as wanted: %v", e.name, err)
			}
			if err := srv.waitShutdown("Shutdown", shutErr, budget+time.Second); err != nil {
				t.Errorf("%s: %v", e.name, err)
			}
		})
	}
}

// drainEngines806 are the engines the two drain tests run on. Adaptive is run
// on each of the sub-engines it can start on, set explicitly (its automatic
// choice is not the test's business, and a test that did not say would cover
// whichever one the host's heuristics picked): the shutdown a server is asked
// for reaches the sub-engine that is active, so each is a different drain.
// setup runs in the subtest, before the server starts; premise, after it
// answered, fails the test when the engine under test is not the one it
// names (celeris#806).
type drainEngine806 struct {
	name    string
	eng     celeris.EngineType
	setup   func(*testing.T)
	premise func(*server760) error
	// slim runs a reduced matrix (see slimModes806): the root package's race
	// run has about 40 s of its 300 s timeout to spare, and the io_uring
	// engines are added on top of it.
	slim bool
}

var drainEngines806 = []drainEngine806{
	{"std", celeris.Std, func(*testing.T) {}, func(*server760) error { return nil }, false},
	{"epoll", celeris.Epoll, func(*testing.T) {}, func(*server760) error { return nil }, false},
	{"io_uring", celeris.IOUring, func(*testing.T) {}, requireIOUring806, true},
	{"adaptive-epoll", celeris.Adaptive, func(t *testing.T) { t.Setenv("CELERIS_ADAPTIVE_START", "epoll") }, requireNoIOUring806, false},
	{"adaptive-iouring", celeris.Adaptive, func(t *testing.T) { t.Setenv("CELERIS_ADAPTIVE_START", "iouring") }, requireIOUring806, true},
}

// sndBufs806 are the server's send buffers the whole-response test runs with:
// the OS default, and 64 KiB, which with the client's 64 KiB receive buffer
// leaves the kernel almost none of a 3 MiB response, however much room the
// host's autotuning would give a larger one. Without it whether a drain that
// gave up early lost the tail depended on the host (celeris#806: the laptop's
// kernel took the whole response, a CI runner's about 2.6 MiB of it). The
// io_uring engines run the small one only (the engines that drain by the
// budget pass either, and the matrix has to stay small, see
// drainEngine806.slim); the others the default.
var sndBufs806 = []struct {
	suffix string
	apply  func(*celeris.Config)
}{
	{"", nil},
	{"/sndbuf64k", func(c *celeris.Config) { c.SocketSendBuf = 64 << 10 }},
}

// ringBytes806 is what the io_uring engine has sent so far, by either of its
// send paths (EngineMetrics.InlineBytes, RingBytes): zero on an engine that
// is not io_uring, so, after one answered request, it says which one is
// serving.
func ringBytes806(srv *server760) uint64 {
	m := srv.s.EngineInfo().Metrics
	return m.InlineBytes + m.RingBytes
}

func requireIOUring806(srv *server760) error {
	// Both counters are flushed once per loop iteration: give the worker one.
	for dl := time.Now().Add(2 * time.Second); ringBytes806(srv) == 0 && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if ringBytes806(srv) == 0 {
		return errors.New("celeris806 PREMISE: the server answered /ping but the io_uring engine has sent no byte (InlineBytes+RingBytes = 0): this is not an io_uring run")
	}
	return nil
}

func requireNoIOUring806(srv *server760) error {
	time.Sleep(100 * time.Millisecond)
	if n := ringBytes806(srv); n != 0 {
		return fmt.Errorf("celeris806 PREMISE: Adaptive started on epoll was serving through io_uring (InlineBytes+RingBytes = %d)", n)
	}
	return nil
}

// startDrainServer806 is startServer760 for an engine of drainEngines806, and
// the one place its premise is checked. Adaptive started on io_uring falls
// back to epoll, with a WARN, when the ring cannot be built (at the CI
// runner's 8 MiB memlock a start made right after the previous server
// stopped can fail although nothing leaked: see startServer760); that run
// would silently test epoll twice. So a server that is not the engine asked
// for is stopped and started again, for up to 30 s, and the test fails when
// it never is.
func startDrainServer806(t *testing.T, e drainEngine806, budget, writeTimeout time.Duration, cfgFn func(*celeris.Config), routes func(*celeris.Server)) *server760 {
	t.Helper()
	e.setup(t)
	retryUntil := time.Now().Add(30 * time.Second)
	for {
		srv := startServer760(t, e.eng, budget, writeTimeout, cfgFn, routes)
		err := e.premise(srv)
		if err == nil {
			return srv
		}
		srv.cancel()
		select {
		case serr := <-srv.startDone:
			srv.startDone <- serr // for the cleanup
		case <-time.After(30 * time.Second):
			t.Fatalf("a server started on the wrong engine did not stop within 30s: %v", err)
		}
		if !time.Now().Before(retryUntil) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// syncBuf806 is a log sink the engine's threads and the test both touch.
type syncBuf806 struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf806) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf806) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
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
func startServer760(t *testing.T, eng celeris.EngineType, budget, writeTimeout time.Duration, cfgFn func(*celeris.Config), routes func(*celeris.Server)) *server760 {
	t.Helper()
	retryUntil := time.Now().Add(30 * time.Second)
	for tries := 1; ; tries++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		cfg := celeris.Config{Engine: eng, Addr: addr, ShutdownTimeout: budget, WriteTimeout: writeTimeout}
		if cfgFn != nil {
			cfgFn(&cfg)
		}
		s := celeris.New(cfg)
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
