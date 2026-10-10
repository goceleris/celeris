//go:build linux

package iouring

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#806, the part the drain's own budget makes necessary: the send drain
// is the ordinary loop iteration, and it lasts as long as the shutdown budget
// (for a ctx without a deadline, until the ctx is done). If that iteration
// went on reading and dispatching requests on the keep-alive connections it
// has, steady traffic whose responses queue in the worker would keep it from
// ever finding the queues empty: the engine served new requests for the whole
// budget and then cut the responses in flight, and Shutdown(context.Background())
// returned only once the clients stopped. So past the drain's first 250 ms
// (the graceful window of celeris#595) the loop reads nothing more from an
// HTTP/1 connection and only finishes the responses it has, as epoll's loops,
// which stop at the cancel, do.

// loadHandler806 answers /big with a body of len(big) bytes and notes when the
// last request reached it. async makes every route async (a dispatch goroutine
// per connection), the other feed path of handleRecv.
type loadHandler806 struct {
	big       []byte
	async     bool
	lastEntry *atomic.Int64 // UnixNano of the latest /big request to reach the handler
}

func (h loadHandler806) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := []byte("ok")
	if s.Path == "/big" {
		body = h.big
		h.lastEntry.Store(time.Now().UnixNano())
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "application/octet-stream"}, {"content-length", strconv.Itoa(len(body))}}, body)
}

func (h loadHandler806) RouteAsync(_, _ string) bool { return h.async }
func (h loadHandler806) HasAsyncRoutes() bool        { return h.async }

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

// engine806 is a running io_uring engine whose Listen the test can cancel.
type engine806 struct {
	e      *Engine
	addr   string
	cancel context.CancelFunc
	done   chan error
}

// startEngine806 starts an io_uring engine on a free loopback port with a 16
// KiB send buffer, so a response of any size stays queued in the worker
// instead of in the kernel whatever the host's autotuning would have given.
func startEngine806(t *testing.T, h stream.Handler, mut func(*resource.Config)) *engine806 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg := resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2, SocketSend: 16 << 10},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mut != nil {
		mut(&cfg)
	}
	e, err := New(cfg, h)
	if err != nil {
		skipOrFail656(t, "iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	for deadline := time.Now().Add(8 * time.Second); ; {
		if c, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond); derr == nil {
			_ = c.Close()
			if e.NumWorkers() > 0 {
				break
			}
		}
		select {
		case err := <-done:
			cancel()
			skipOrFail656(t, "iouring engine failed to start: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("engine did not start listening within 8s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r := &engine806{e: e, addr: addr, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("engine did not stop within 15s")
		}
	})
	return r
}

// shutdown is Server.shutdown's order: the budget goes to the engine, then
// Listen's context is cancelled.
func (r *engine806) shutdown(ctx context.Context) {
	go func() {
		_ = r.e.Shutdown(ctx)
		r.cancel()
	}()
}

func TestShutdownUnderKeepAliveLoad806(t *testing.T) {
	const bodyLen = 1 << 20
	const clients = 16
	const floor = 250 * time.Millisecond
	big := make([]byte, bodyLen)
	for _, async := range []bool{false, true} {
		for _, mode := range []string{"budget-3s", "background"} {
			name := "sync/"
			if async {
				name = "async/"
			}
			t.Run(name+mode, func(t *testing.T) {
				var lastEntry atomic.Int64
				r := startEngine806(t, loadHandler806{big: big, async: async, lastEntry: &lastEntry}, func(c *resource.Config) {
					c.AsyncHandlers = async
				})
				var stop atomic.Bool
				var wg sync.WaitGroup
				var served, cut, inBody, closedAtReq atomic.Int64
				for range clients {
					wg.Add(1)
					go func() {
						defer wg.Done()
						d := net.Dialer{Control: func(_, _ string, rc syscall.RawConn) error {
							return rc.Control(func(fd uintptr) { _ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 16<<10) })
						}}
						c, err := d.Dial("tcp", r.addr)
						if err != nil {
							return
						}
						defer func() { _ = c.Close() }()
						br := bufio.NewReaderSize(c, 8<<10)
						buf := make([]byte, 8<<10)
						for !stop.Load() {
							if _, err := io.WriteString(c, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
								return
							}
							resp, err := http.ReadResponse(br, nil)
							if err != nil {
								closedAtReq.Add(1)
								return // the engine closed the conn: a request it had not taken
							}
							inBody.Add(1)
							n := 0
							for {
								m, err := resp.Body.Read(buf)
								n += m
								if err != nil {
									break
								}
								time.Sleep(200 * time.Microsecond) // a client that takes its response at a finite pace
							}
							_ = resp.Body.Close()
							inBody.Add(-1)
							if n != bodyLen {
								cut.Add(1) // a response whose headers came, and whose body did not
								return
							}
							served.Add(1)
						}
					}()
				}
				defer func() { stop.Store(true); wg.Wait() }()
				for deadline := time.Now().Add(10 * time.Second); served.Load() < clients*2; {
					if time.Now().After(deadline) {
						t.Fatalf("premise: only %d responses in 10s before the shutdown", served.Load())
					}
					time.Sleep(10 * time.Millisecond)
				}
				before := served.Load()
				t.Logf("at the shutdown: %d of %d clients are inside a response body", inBody.Load(), clients)
				start := time.Now()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "budget-3s" {
					ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
				}
				r.shutdown(ctx)
				var took time.Duration
				select {
				case <-r.done:
					took = time.Since(start)
				case <-time.After(5 * time.Second):
					t.Fatalf("Listen had not returned 5s after the shutdown began (budget %s) with %d keep-alive clients still requesting: "+
						"the drain went on serving requests; %d responses completed meanwhile",
						mode, clients, served.Load()-before)
				}
				r.done <- nil // for the cleanup
				// Mechanism: no request reaches a handler once the drain's first
				// 250 ms are over (one that reaches the loop in that window may
				// still be answered). A request is a 1 MiB response away from
				// the next, so 350 ms of slack covers the dispatch, not a second
				// wave of them.
				if late := time.Duration(lastEntry.Load() - start.UnixNano()); late > floor+350*time.Millisecond {
					t.Errorf("a request reached the handler %v into the shutdown, past the drain's first %v: the loop is still serving keep-alive connections", late.Round(time.Millisecond), floor)
				}
				if took > 1500*time.Millisecond {
					t.Errorf("Listen returned %v after the shutdown began, want under 1.5s (the responses queued behind %d paced clients need ~100 ms past the first %v)", took.Round(time.Millisecond), clients, floor)
				}
				// The clients read what the closed conns still hold; a response
				// they began and did not get whole is the data loss of a drain
				// that gave up, which none of these budgets should cause.
				stop.Store(true)
				wg.Wait()
				if n := cut.Load(); n != 0 {
					t.Errorf("%d responses were cut: the drain closed connections whose response the client had not yet taken", n)
				}
				t.Logf("806load %s%s: Listen returned %v after the shutdown began; %d responses completed after it began; %d cut; %d clients saw the conn closed before a response", name, mode, took.Round(time.Millisecond), served.Load()-before, cut.Load(), closedAtReq.Load())
			})
		}
	}
}

// TestShutdownBoundWarns806: a client that never reads holds the drain to the
// shutdown's budget and no further, and the loss that bound is (the response
// bytes the socket never took) is logged: one WARN naming the connections and
// the queued bytes. The 16 KiB send buffer of startEngine806 makes the loss
// certain whatever the host's autotuning would have given.
func TestShutdownBoundWarns806(t *testing.T) {
	const budget = 500 * time.Millisecond
	logs := &syncBuf806{}
	var lastEntry atomic.Int64
	r := startEngine806(t, loadHandler806{big: make([]byte, 3<<20), lastEntry: &lastEntry}, func(c *resource.Config) {
		c.Logger = slog.New(slog.NewTextHandler(logs, nil))
	})
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.(*net.TCPConn).SetReadBuffer(16 << 10)
	if _, err := io.WriteString(c, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); lastEntry.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the handler did not run within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // the response is queued behind a full socket
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	r.shutdown(ctx)
	select {
	case <-r.done:
	case <-time.After(budget + time.Second):
		t.Fatalf("Listen had not returned %v after the shutdown began: a client that never reads held the drain past a %v budget", budget+time.Second, budget)
	}
	r.done <- nil // for the cleanup
	t.Logf("Listen returned %v after the shutdown began (budget %v)", time.Since(start).Round(time.Millisecond), budget)
	if !strings.Contains(logs.String(), "the send drain ran out of time") {
		t.Errorf("the engine cut a response the client had not taken and logged nothing about it:\n%s", logs.String())
	}
}
