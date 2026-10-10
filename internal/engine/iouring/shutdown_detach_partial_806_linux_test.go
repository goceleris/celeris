//go:build linux

package iouring_test

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// trial806 is the shape of one run of the detached partial-write test. Stream
// i writes one chunk, startUs + i*stepUs microseconds after the shutdown
// began: the send drain's first 250 ms end about 100 us after that, and the
// loop's first pass past them comes within a few hundred us of it, so 64
// writes 40 us apart, from 100 us before, cover the moment. The stalled client
// holds the drain open until it reads, holdMs after the shutdown began.
type trial806 struct {
	streams, startUs, stepUs, chunkKiB, holdMs int
	attempts                                   int
}

var params806 = trial806{streams: 64, startUs: 249900, stepUs: 40, chunkKiB: 128, holdMs: 450, attempts: 1}

type logBuf806 struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf806) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf806) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestShutdownDetachedPartialWriteIsFlushed806: a detached stream (Server-Sent
// Events) whose producer writes at the moment the send drain ends the detached
// writes (celeris#806) may leave a part of what it wrote in the worker's queue:
// the accepted socket is non-blocking, so the inline write takes what the
// socket has room for. Those bytes must still be sent. The drain waits for
// them, and a drain that never sends them lasts the whole budget although
// every client reads: the celeris#806 symptom on the detached path, found in
// review of the fix.
//
// The shape: a stalled response holds the drain open for 450 ms (it is read
// then); 64 streams each write one chunk larger than the socket buffers across
// the moment the drain's first 250 ms end, to clients that read at once. The
// drain must end shortly after the stalled client has read (a slow host takes
// seconds over a response through 16 KiB windows: a CI runner took 1.7 s over
// 256 KiB), not at the budget, and must not log that it ran out of time. The
// moment is one loop pass wide, so this is a probabilistic reproduction (on the
// code before the fix it fails in most runs, not all); the deterministic one is
// TestStopDetachedProducersFlushesQueuedBytes806, in this package.
//
// io_uring, directly and as the engine Adaptive started on. The base tier
// accepts blocking sockets (its accept SQE has no SOCK_NONBLOCK), where an
// inline write waits for the client instead of leaving a remainder, so the
// state this test makes is not reachable there and the test passes before and
// after the fix; the unit test is tier-independent.
func TestShutdownDetachedPartialWriteIsFlushed806(t *testing.T) {
	cases := []struct {
		name  string
		eng   celeris.EngineType
		setup func(*testing.T)
	}{
		{"io_uring", celeris.IOUring, func(*testing.T) {}},
		{"adaptive-iouring", celeris.Adaptive, func(t *testing.T) { t.Setenv("CELERIS_ADAPTIVE_START", "iouring") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			for attempt := 1; attempt <= params806.attempts; attempt++ {
				if msg := runDetachedPartial806(t, tc.eng, params806); msg != "" {
					t.Fatalf("attempt %d of %d: %s", attempt, params806.attempts, msg)
				}
			}
		})
	}
}

// runDetachedPartial806 runs one shutdown and returns what it found wrong, or
// "". The server and every client are gone when it returns.
func runDetachedPartial806(t *testing.T, eng celeris.EngineType, p trial806) string {
	t.Helper()
	// A budget well above what a slow host needs to move the queued bytes
	// (several seconds under -race on a small runner), so only bytes that
	// never move last it out.
	const budget = 20 * time.Second
	chunk := make([]byte, p.chunkKiB<<10)
	big := make([]byte, 160<<10) // more than the socket buffers hold (about 64 KiB), so it stalls until the client reads
	var t0 atomic.Int64          // UnixNano of the shutdown's start; 0 before
	var idx atomic.Int64
	logs := &logBuf806{}
	served := make(chan struct{}, 1)

	srv, addr, startDone, cancel := startServer806(t, eng, budget, logs, func(s *celeris.Server) {
		s.GET("/hold", func(c *celeris.Context) error {
			defer func() {
				select {
				case served <- struct{}{}:
				default:
				}
			}()
			return c.Blob(http.StatusOK, "application/octet-stream", big)
		})
		s.GET("/push", func(c *celeris.Context) error {
			i := idx.Add(1) - 1
			done := c.Detach()
			sw := c.StreamWriter()
			_ = sw.WriteHeader(http.StatusOK, [][2]string{{"content-type", "application/octet-stream"}})
			_ = sw.Flush()
			run := func() {
				defer done()
				for t0.Load() == 0 {
					time.Sleep(200 * time.Microsecond)
				}
				at := time.Unix(0, t0.Load()).Add(time.Duration(p.startUs+int(i)*p.stepUs) * time.Microsecond)
				for d := time.Until(at); d > 0; d = time.Until(at) {
					if d > 2*time.Millisecond {
						time.Sleep(d - time.Millisecond)
					}
				}
				if _, err := sw.Write(chunk); err == nil {
					_ = sw.Flush()
				}
			}
			if c.EngineSupportsAsyncDetach() {
				go run()
				return nil
			}
			run()
			return nil
		})
	})
	defer func() {
		cancel()
		select {
		case <-startDone:
		case <-time.After(30 * time.Second):
			t.Error("the server did not stop within 30s of the cleanup")
		}
	}()

	// The engine under test must be the one serving.
	m := srv.EngineInfo().Metrics
	for dl := time.Now().Add(2 * time.Second); m.InlineBytes+m.RingBytes == 0 && time.Now().Before(dl); m = srv.EngineInfo().Metrics {
		time.Sleep(10 * time.Millisecond)
	}
	if m.InlineBytes+m.RingBytes == 0 {
		t.Fatal("celeris806 PREMISE: the server answered /ping but the io_uring engine has sent no byte: this is not an io_uring run")
	}

	// The clients of the streams read at once; each stream's handler
	// goroutine is told when to write, so connect them before the shutdown.
	var stop atomic.Bool
	var got atomic.Int64 // bytes the stream clients have read
	var wg sync.WaitGroup
	defer func() { stop.Store(true); wg.Wait() }()
	for range p.streams {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(c, "GET /push HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = c.Close() }()
			buf := make([]byte, 128<<10)
			for !stop.Load() {
				_ = c.SetReadDeadline(time.Now().Add(time.Second))
				n, err := c.Read(buf)
				got.Add(int64(n))
				if err != nil {
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						continue
					}
					return
				}
			}
		}()
	}
	// The stalled response that holds the drain open.
	hc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hc.Close() }()
	_ = hc.(*net.TCPConn).SetReadBuffer(16 << 10)
	if _, err := io.WriteString(hc, "GET /hold HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled response's handler did not run within 5s")
	}
	for dl := time.Now().Add(5 * time.Second); idx.Load() < int64(p.streams) && time.Now().Before(dl); {
		time.Sleep(5 * time.Millisecond)
	}
	if n := idx.Load(); n < int64(p.streams) {
		t.Fatalf("only %d of %d streams reached their handler", n, p.streams)
	}
	time.Sleep(150 * time.Millisecond) // the stream headers are out, the response is queued behind a full socket

	start := time.Now()
	t0.Store(start.UnixNano())
	shutDone := make(chan error, 1)
	go func() {
		sctx, scancel := context.WithTimeout(context.Background(), budget)
		defer scancel()
		shutDone <- srv.Shutdown(sctx)
	}()
	var holdDone atomic.Int64 // ns after the shutdown began that the stalled client had read it all
	time.AfterFunc(time.Duration(p.holdMs)*time.Millisecond, func() {
		_ = hc.SetReadDeadline(time.Now().Add(10 * time.Second))
		if resp, err := http.ReadResponse(bufio.NewReader(hc), nil); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			holdDone.Store(int64(time.Since(start)))
		}
	})
	var shutErr error
	select {
	case shutErr = <-shutDone:
	case <-time.After(budget + 3*time.Second):
		return fmt.Sprintf("Shutdown had not returned %v after it began", budget+3*time.Second)
	}
	took := time.Since(start)
	t.Logf("%v: streams=%d chunk=%dKiB Shutdown returned after %v (err=%v), the stalled client was done after %v", eng, p.streams, p.chunkKiB,
		took.Round(time.Millisecond), shutErr, time.Duration(holdDone.Load()).Round(time.Millisecond))
	if took <= budget/2 && !strings.Contains(logs.String(), "the send drain ran out of time") {
		return ""
	}
	// Held. The WARN is logged by the worker, which may be a moment behind
	// Shutdown's own return at the budget: give it that, and say all of it.
	time.Sleep(300 * time.Millisecond)
	var warns []string
	for _, ln := range strings.Split(logs.String(), "\n") {
		if strings.Contains(ln, "level=WARN") || strings.Contains(ln, "level=ERROR") {
			warns = append(warns, ln)
		}
	}
	m = srv.EngineInfo().Metrics
	return fmt.Sprintf("HELD: the send drain lasted %v of a %v budget although every client reads (bytes stranded in the worker); engine: InlineBytes=%d RingBytes=%d, the stream clients read %d bytes (%d streams of %d KiB), the stalled client was done after %v; log: %s",
		took.Round(time.Millisecond), budget, m.InlineBytes, m.RingBytes, got.Load(), p.streams, p.chunkKiB, time.Duration(holdDone.Load()).Round(time.Millisecond), strings.Join(warns, " | "))
}

// startServer806 starts a server of engine eng with routes on
// StartWithContext (ShutdownTimeout budget, a 16 KiB send buffer so a chunk
// stays queued behind the socket) and waits until it answers /ping. An
// io_uring start that fails only with ENOMEM is retried with a new server, for
// up to 30 s: the kernel charges ring memory to RLIMIT_MEMLOCK per UID and
// gives it back some milliseconds after a ring closes, so at the CI runner's
// 8 MiB a start made right after the previous server stopped can fail although
// nothing leaked.
func startServer806(t *testing.T, eng celeris.EngineType, budget time.Duration, logs io.Writer, routes func(*celeris.Server)) (*celeris.Server, string, chan error, context.CancelFunc) {
	t.Helper()
	retryUntil := time.Now().Add(30 * time.Second)
	for {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		s := celeris.New(celeris.Config{
			Engine: eng, Addr: addr, ShutdownTimeout: budget, SocketSendBuf: 16 << 10,
			Logger: slog.New(slog.NewTextHandler(logs, nil)),
		})
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		routes(s)
		ctx, cancel := context.WithCancel(context.Background())
		startDone := make(chan error, 1)
		go func() { startDone <- s.StartWithContext(ctx) }()
		err = waitReady806(addr, startDone)
		if err == nil {
			return s, addr, startDone, cancel
		}
		cancel()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(retryUntil) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start: %v", err)
	}
}

// waitReady806 polls /ping until it answers 200, or returns the error the
// start returned first.
func waitReady806(addr string, startDone <-chan error) error {
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
