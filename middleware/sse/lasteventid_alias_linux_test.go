//go:build linux

package sse_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/middleware/sse"
	"github.com/goceleris/celeris/probe"
)

// TestClientLastEventIDSurvivesPeerBytes pins celeris#717, the SSE twin of
// celeris#714, and the request ID the requestid middleware stores in the
// std context (EnableStdContext), which the stream keeps through
// Client.Context().
//
// On epoll and io_uring, c.Header returns a view of the engine's receive
// buffer. The middleware read Last-Event-ID before Context.Detach and kept
// that view in the Client for the whole stream, and requestid put its view
// of X-Request-Id into the context the Client's context derives from. The
// engine keeps receiving into the same buffer on a detached connection.
// Here the peer sends bytes after the stream is open (the engine then closes
// the connection), and the handler reads both values once its context is
// done: each must still be what the client sent. The handler also copies
// both at stream start and sends an event, and the client sends its bytes
// only after that event, so those copies predate the bytes. They are the
// control: they show the values were right to begin with.
func TestClientLastEventIDSurvivesPeerBytes(t *testing.T) {
	kinds := []celeris.EngineType{celeris.Epoll}
	if ok, p := c714ProbeIOUring(); ok {
		kinds = append(kinds, celeris.IOUring)
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring cells", p.IOUringTier, p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s: io_uring cells not run", p.IOUringTier, p.KernelVersion)
	}
	var cells []sseCell
	for _, kind := range kinds {
		cells = append(cells,
			sseCell{name: kind.String() + "/sync", engine: kind},
			sseCell{name: kind.String() + "/async", engine: kind, async: true},
		)
	}
	cells = append(cells, sseCell{name: "std/sync", engine: celeris.Std})

	// what the handler read: Last-Event-ID and the request ID, at stream
	// start (copied) and once the stream's context is done.
	type reading struct{ eventStart, ridStart, eventEnd, ridEnd string }

	for _, cell := range cells {
		t.Run(cell.name, func(t *testing.T) {
			got := make(chan reading, 1)
			addr, shutdown := startC714SSEServer(t, cell.engine, cell.async,
				requestid.New(requestid.Config{EnableStdContext: true}),
				sse.New(sse.Config{
					// std notices a gone peer only when a write fails (its
					// request context is not cancelled once the peer has sent
					// bytes), so keep a short heartbeat for it.
					HeartbeatInterval: 100 * time.Millisecond,
					Handler: func(client *sse.Client) {
						r := reading{
							eventStart: strings.Clone(client.LastEventID()),
							ridStart:   strings.Clone(requestid.FromStdContext(client.Context())),
						}
						// The client sends its bytes only after this event.
						_ = client.SendData("start")
						<-client.Context().Done()
						r.eventEnd = client.LastEventID()
						r.ridEnd = requestid.FromStdContext(client.Context())
						got <- r
					},
				}))
			defer shutdown()

			const n = 20
			wait := 2 * time.Second
			if cell.engine == celeris.Std {
				wait = 250 * time.Millisecond
			}
			// wrong[field][phase]: field 0 = Last-Event-ID, 1 = request ID;
			// phase 0 = stream start, 1 = after the peer sent bytes.
			var wrong [2][2]int
			var samples []string
			for i := 0; i < n; i++ {
				id := fmt.Sprintf("evt-%06d-%s", i, strings.Repeat("k", 24))
				rid := fmt.Sprintf("rid-%06d-%s", i, strings.Repeat("r", 24))
				conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				req := "GET /events HTTP/1.1\r\nHost: " + addr + "\r\nAccept: text/event-stream\r\nLast-Event-ID: " + id +
					"\r\nX-Request-Id: " + rid + "\r\n\r\n"
				if _, err := conn.Write([]byte(req)); err != nil {
					t.Fatal(err)
				}
				br := bufio.NewReader(conn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						t.Fatalf("read response head: %v", err)
					}
					if line == "\r\n" {
						break
					}
				}
				// Wait for the handler's first event: it has read the values
				// at stream start by then, so that reading predates the bytes
				// sent next.
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						t.Fatalf("read the first event: %v", err)
					}
					if strings.TrimSpace(line) == "data: start" {
						break
					}
				}
				// More bytes than the whole request, so every request byte
				// in the receive buffer is overwritten.
				_, _ = conn.Write([]byte(strings.Repeat("Z", 4*len(req))))
				// The native engines close a detached SSE connection that
				// sends; std does not, so bound the wait and then close.
				_ = conn.SetReadDeadline(time.Now().Add(wait))
				_, _ = io.Copy(io.Discard, br)
				_ = conn.Close()
				select {
				case r := <-got:
					for f, pair := range [2][2]string{{r.eventStart, r.eventEnd}, {r.ridStart, r.ridEnd}} {
						want := id
						if f == 1 {
							want = rid
						}
						for phase, v := range pair {
							if v != want {
								wrong[f][phase]++
								if len(samples) < 4 {
									samples = append(samples, fmt.Sprintf("field=%d phase=%d want %q got %q", f, phase, want, v))
								}
							}
						}
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("conn %d: handler did not see its context end", i)
				}
			}
			t.Logf("C714SSE cell=%s streams=%d last_event_id_wrong_at_start=%d last_event_id_wrong_after=%d request_id_wrong_at_start=%d request_id_wrong_after=%d",
				cell.name, n, wrong[0][0], wrong[0][1], wrong[1][0], wrong[1][1])
			if wrong[0][0]+wrong[0][1] > 0 {
				t.Errorf("Client.LastEventID is not the header the client sent: wrong at stream start %d/%d, after the peer sent bytes %d/%d; samples %q",
					wrong[0][0], n, wrong[0][1], n, samples)
			}
			if wrong[1][0]+wrong[1][1] > 0 {
				t.Errorf("requestid.FromStdContext(Client.Context()) is not the X-Request-Id the client sent: wrong at stream start %d/%d, after the peer sent bytes %d/%d; samples %q",
					wrong[1][0], n, wrong[1][1], n, samples)
			}
		})
	}
}

// startC714SSEServer starts a server with handlers mounted at /events on a
// fresh loopback listener and returns its address and a shutdown closure.
//
// An io_uring start that fails only with ENOMEM is retried for up to 10 s.
// The kernel charges ring memory to RLIMIT_MEMLOCK per UID and gives it back
// 12-23 ms after a ring closes (engine/iouring/ring_budget_linux_test.go),
// so at the CI runner's 8 MiB a start made right after the previous cell
// stopped, or while another package's test binary holds rings, can fail
// although nothing leaked.
func startC714SSEServer(t *testing.T, engine celeris.EngineType, async bool, handlers ...celeris.HandlerFunc) (string, func()) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for tries := 1; ; tries++ {
		s := celeris.New(celeris.Config{Engine: engine, AsyncHandlers: async, ShutdownTimeout: 2 * time.Second})
		s.GET("/events", handlers...)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
		addr, err := c714WaitReady(s, done)
		if err == nil {
			if tries > 1 {
				t.Logf("server start retried on ring ENOMEM: %d tries", tries)
			}
			return addr, func() { cancel(); <-done }
		}
		cancel()
		_ = ln.Close()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start: %v", err)
	}
}

// c714WaitReady waits until s accepts connections, or its start returns.
func c714WaitReady(s *celeris.Server, done <-chan error) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			if err == nil {
				err = errors.New("start returned before the server was ready")
			}
			return "", err
		default:
		}
		if a := s.Addr(); a != nil {
			if c, err := net.DialTimeout("tcp", a.String(), 100*time.Millisecond); err == nil {
				_ = c.Close()
				return a.String(), nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", errors.New("server not ready within 30s")
}

// c714ProbeIOUring probes the kernel's io_uring support. With
// CELERIS_REQUIRE_IOURING_WORKERS=1 a probe that finds no usable ring is
// retried for up to 10 s before the io_uring arms count as missing: the
// probe's ring can fail with ENOMEM against RLIMIT_MEMLOCK while the rings
// of engines stopped moments ago, or of another test binary run by the same
// user, are still charged (engine/iouring/ring_budget_linux_test.go).
func c714ProbeIOUring() (usable bool, p celerisengine.CapabilityProfile) {
	p = probe.Probe()
	usable = p.IOUringTier >= celerisengine.High && p.ProvidedBuffers
	if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") != "1" {
		return usable, p
	}
	for deadline := time.Now().Add(10 * time.Second); !usable && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		p = probe.Probe()
		usable = p.IOUringTier >= celerisengine.High && p.ProvidedBuffers
	}
	return usable, p
}
