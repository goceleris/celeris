//go:build linux

package sse_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

// TestSentEventSurvivesSendersNextRequest pins the sse site of the
// celeris#732 class: what Client.Send keeps of the Event it is given after
// it returns.
//
// The documented sender is often another request: a handler that relays
// c.Param or c.Query to a connected Client. On epoll and io_uring (and
// Adaptive, which runs them) those strings are views of the relaying
// connection's receive buffer, which the engine reuses for that
// connection's next request. Send keeps the Event in two places: a
// NewRingBuffer replay store keeps it for the next reconnect, and in queued
// mode (MaxQueueDepth > 0) the per-client queue keeps it until the drain
// formats it. Here the relaying connection sends one event, with its type
// and its data from the path, and then two more requests with the same
// layout and other values (an async handler's request sits in a double
// buffer, so it takes two). Then the event the subscriber reads on the wire
// and the one the ring keeps must both still be the first request's.
//
// In blocking mode Send writes the event before it returns, so the wire is
// a control there and the ring is the site. In queued mode the replay store
// wraps the ring and holds the drain in Append until the two later requests
// are answered, so the drain formats the queued event, and appends it to
// the ring, only after the relaying connection's buffer has been reused.
func TestSentEventSurvivesSendersNextRequest(t *testing.T) {
	arms := sse732Arms(t)
	for _, mode := range []string{"blocking", "queued"} {
		for _, a := range arms {
			t.Run(mode+"/"+a.name, func(t *testing.T) {
				ring := sse.NewRingBuffer(16)
				gate := make(chan struct{})
				var gateOnce sync.Once
				release := func() { gateOnce.Do(func() { close(gate) }) }
				cfg := sse.Config{ReplayStore: ring, HeartbeatInterval: -1}
				if mode == "queued" {
					cfg.MaxQueueDepth = 8
					cfg.ReplayStore = gatedStore{ReplayStore: ring, gate: gate}
				}
				// The registry the relaying handler reads the subscriber from.
				var regMu sync.Mutex
				var reg *sse.Client
				subscribed := make(chan struct{}, 1)
				cfg.Handler = func(cl *sse.Client) {
					regMu.Lock()
					reg = cl
					regMu.Unlock()
					subscribed <- struct{}{}
					<-cl.Context().Done()
				}
				addr, stop := startSSE732(t, a, func(s *celeris.Server) {
					s.GET("/events", sse.New(cfg))
					s.GET("/say/:kind/:msg", func(c *celeris.Context) error {
						regMu.Lock()
						sub := reg
						regMu.Unlock()
						if err := sub.Send(sse.Event{Event: c.Param("kind"), Data: c.Param("msg")}); err != nil {
							return err
						}
						return c.String(200, "ok")
					})
					s.GET("/zzz/:kind/:msg", func(c *celeris.Context) error { return c.String(200, "ok") })
				})
				defer stop()
				// Before stop: a drain held in Append must not hold up the shutdown.
				defer release()

				ec, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = ec.Close() }()
				_ = ec.SetDeadline(time.Now().Add(20 * time.Second))
				if _, err := fmt.Fprintf(ec, "GET /events HTTP/1.1\r\nHost: h\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				ebr := bufio.NewReader(ec)
				select {
				case <-subscribed:
				case <-time.After(10 * time.Second):
					t.Fatal("the SSE handler did not start")
				}

				rc, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = rc.Close() }()
				_ = rc.SetDeadline(time.Now().Add(20 * time.Second))
				rbr := bufio.NewReader(rc)
				for _, p := range []string{"/say/kind-aaaa/msg-aaaa", "/zzz/kind-bbbb/msg-bbbb", "/zzz/kind-cccc/msg-cccc"} {
					if _, err := fmt.Fprintf(rc, "GET %s HTTP/1.1\r\nHost: h\r\n\r\n", p); err != nil {
						t.Fatal(err)
					}
					if err := sse732ReadOK(rbr); err != nil {
						t.Fatalf("%s: %v", p, err)
					}
				}
				release()

				wireKind, wireData, err := sse732ReadEvent(ebr)
				if err != nil {
					t.Fatalf("read the event on the subscriber's wire: %v", err)
				}
				evs, err := ring.Since(context.Background(), "0")
				if err != nil {
					t.Fatalf("ring Since: %v", err)
				}
				ringKind, ringData := "(none)", "(none)"
				if len(evs) == 1 {
					ringKind, ringData = evs[0].Event, evs[0].Data
				}
				var wrong []string
				if wireKind != "kind-aaaa" || wireData != "msg-aaaa" {
					wrong = append(wrong, fmt.Sprintf("wire event %q data %q", wireKind, wireData))
				}
				if len(evs) != 1 || ringKind != "kind-aaaa" || ringData != "msg-aaaa" {
					wrong = append(wrong, fmt.Sprintf("ring %d event(s), event %q data %q", len(evs), ringKind, ringData))
				}
				t.Logf("MW732SSE mode=%s arm=%s wire=%s/%s ring=%s/%s wrong=%d", mode, a.name, wireKind, wireData, ringKind, ringData, len(wrong))
				if len(wrong) > 0 {
					t.Errorf("the event sent with event kind-aaaa and data msg-aaaa reads other bytes after the sender's connection served two more requests:\n  %s", strings.Join(wrong, "\n  "))
				}
			})
		}
	}
}

// gatedStore holds Append until gate is closed: in queued mode the drain
// appends each event before it formats it, so this holds the drain.
type gatedStore struct {
	sse.ReplayStore
	gate <-chan struct{}
}

func (g gatedStore) Append(ctx context.Context, e sse.Event) (string, error) {
	select {
	case <-g.gate:
	case <-ctx.Done():
	}
	return g.ReplayStore.Append(ctx, e)
}

type sse732Arm struct {
	name   string
	engine celeris.EngineType
	async  bool
}

// sse732Arms returns std, and epoll, io_uring and Adaptive with sync and
// async handlers. With CELERIS_REQUIRE_IOURING_WORKERS=1 a kernel with no
// usable io_uring fails the test instead of dropping the io_uring arms.
func sse732Arms(t *testing.T) []sse732Arm {
	t.Helper()
	arms := []sse732Arm{{"std", celeris.Std, false}, {"epoll", celeris.Epoll, false}, {"epoll-async", celeris.Epoll, true}}
	if ok, p := c714ProbeIOUring(); ok {
		arms = append(arms, sse732Arm{"io_uring", celeris.IOUring, false}, sse732Arm{"io_uring-async", celeris.IOUring, true})
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arms", p.IOUringTier, p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s: io_uring arms not run", p.IOUringTier, p.KernelVersion)
	}
	return append(arms, sse732Arm{"adaptive", celeris.Adaptive, false}, sse732Arm{"adaptive-async", celeris.Adaptive, true})
}

// startSSE732 starts a server with the routes on a fresh loopback listener
// and returns its address and a shutdown closure. Start's error fails the
// test at once (c714WaitReady); one that is only ENOMEM (io_uring ring
// memory still charged to RLIMIT_MEMLOCK) is retried with a new server for
// up to 10 s.
func startSSE732(t *testing.T, a sse732Arm, routes func(*celeris.Server)) (string, func()) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async, ShutdownTimeout: 2 * time.Second})
		routes(s)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
		addr, err := c714WaitReady(s, done)
		if err == nil {
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

// sse732ReadOK reads one response and its body, and fails unless it is a 200.
func sse732ReadOK(br *bufio.Reader) error {
	status, err := br.ReadString('\n')
	if err != nil {
		return err
	}
	n := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if line == "\r\n" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "content-length") {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if _, err := br.Discard(n); err != nil {
		return err
	}
	if f := strings.Fields(status); len(f) < 2 || f[1] != "200" {
		return fmt.Errorf("status %q", strings.TrimSpace(status))
	}
	return nil
}

// sse732ReadEvent reads the stream up to the first event's data line and
// returns its event type and data.
func sse732ReadEvent(br *bufio.Reader) (kind, data string, err error) {
	for range 200 {
		line, err := br.ReadString('\n')
		if err != nil {
			return kind, data, err
		}
		line = strings.TrimRight(line, "\r\n")
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			kind = v
		}
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			return kind, v, nil
		}
	}
	return kind, data, fmt.Errorf("no data line in 200 lines")
}
