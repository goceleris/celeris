//go:build linux

package otel_test

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

	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
	"github.com/goceleris/celeris/middleware/otel"
	"github.com/goceleris/celeris/middleware/sse"
)

// TestExtractedContextSurvivesPeerBytes pins the OpenTelemetry face of
// celeris#714: what the propagators extract from the request is kept in the
// context the middleware installs with SetContext, and a detached stream
// keeps that context for its whole life (SSE Client.Context(), WebSocket
// Conn.Context(), both derived from c.Context()).
//
// On epoll and io_uring the carrier handed the propagators views of the
// engine's receive buffer, and the OTel API keeps them: trace.ParseTraceState
// and baggage.Parse return substrings of their input, and a propagator that
// walks Keys() (for example one that maps prefixed headers into baggage, as
// the Jaeger propagator does) keeps the header names it matches. The engine
// keeps receiving into that buffer on a detached connection. Here the peer
// sends bytes after the stream is open, and the handler reads the tracestate
// entry, the baggage member and the kept header name once the stream's
// context is done: each must still be what the client sent. The handler also
// copies all three at stream start and sends an event, and the client sends
// its bytes only after that event, so those copies predate the bytes. They
// are the control: they show the values were extracted at all.
func TestExtractedContextSurvivesPeerBytes(t *testing.T) {
	type arm struct {
		name   string
		engine celeris.EngineType
		async  bool
	}
	arms := []arm{
		{"std", celeris.Std, false},
		{"epoll", celeris.Epoll, false},
		{"epoll-async", celeris.Epoll, true},
	}
	if ok, p := c714ProbeIOUring(); ok {
		arms = append(arms, arm{"io_uring", celeris.IOUring, false}, arm{"io_uring-async", celeris.IOUring, true})
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arms", p.IOUringTier, p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s: io_uring arms not run", p.IOUringTier, p.KernelVersion)
	}
	fields := []string{"tracestate", "baggage", "header-name"}

	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			tp := sdktrace.NewTracerProvider()
			defer func() { _ = tp.Shutdown(context.Background()) }()
			got := make(chan [2][3]string, 1) // [stream start|after the peer's bytes][field]
			mw := otel.New(otel.Config{
				TracerProvider: tp,
				DisableMetrics: true,
				Propagators: propagation.NewCompositeTextMapPropagator(
					propagation.TraceContext{}, propagation.Baggage{}, prefixedHeaderPropagator{}),
			})
			stream := sse.New(sse.Config{
				// std notices a gone peer only when a write fails, so keep a
				// short heartbeat for it.
				HeartbeatInterval: 100 * time.Millisecond,
				Handler: func(client *sse.Client) {
					var r [2][3]string
					for i, v := range extractedView(client.Context()) {
						r[0][i] = strings.Clone(v)
					}
					// The client sends its bytes only after this event.
					_ = client.SendData("start")
					<-client.Context().Done()
					r[1] = extractedView(client.Context())
					got <- r
				},
			})
			addr, stop := startC714OtelServer(t, func() *celeris.Server {
				s := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async, ShutdownTimeout: 2 * time.Second})
				s.GET("/events", mw, stream)
				return s
			})
			defer stop()

			const n = 20
			wait := 2 * time.Second
			if a.engine == celeris.Std {
				wait = 250 * time.Millisecond
			}
			var wrong [3][2]int // [field][stream start|after]
			var samples []string
			for i := 0; i < n; i++ {
				id := fmt.Sprintf("%06d%s", i, strings.Repeat("k", 24))
				want := [3]string{"ts" + id, "bg" + id, "ctx-" + id}
				req := "GET /events HTTP/1.1\r\nHost: " + addr + "\r\nAccept: text/event-stream\r\n" +
					"Traceparent: 00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01\r\n" +
					"Tracestate: c714=" + want[0] + "\r\n" +
					"Baggage: c714=" + want[1] + "\r\n" +
					want[2] + ": 1\r\n\r\n"
				conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
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
				_ = conn.SetReadDeadline(time.Now().Add(wait))
				_, _ = io.Copy(io.Discard, br)
				_ = conn.Close()
				select {
				case r := <-got:
					for phase := range r {
						for f := range want {
							if r[phase][f] != want[f] {
								wrong[f][phase]++
								if len(samples) < 6 {
									samples = append(samples, fmt.Sprintf("%s phase=%d want %q got %q", fields[f], phase, want[f], r[phase][f]))
								}
							}
						}
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("conn %d: handler did not see its context end", i)
				}
			}
			var tally []string
			bad := false
			for f, name := range fields {
				tally = append(tally, fmt.Sprintf("%s=%d/%d", name, wrong[f][0], wrong[f][1]))
				bad = bad || wrong[f][0]+wrong[f][1] > 0
			}
			t.Logf("C714OTEL arm=%s streams=%d wrong (at stream start/after the peer sent bytes): %s", a.name, n, strings.Join(tally, " "))
			if bad {
				t.Errorf("the context a detached stream keeps does not hold what the propagators extracted once the peer has sent more bytes (wrong at stream start/after, over %d streams: %s); samples %q",
					n, strings.Join(tally, " "), samples)
			}
		})
	}
}

// extractedView reads, from a stream's context, the tracestate entry and
// the baggage member the request carried, and the header name
// prefixedHeaderPropagator kept.
func extractedView(ctx context.Context) [3]string {
	name, _ := ctx.Value(prefixedHeaderKey{}).(string)
	return [3]string{
		trace.SpanContextFromContext(ctx).TraceState().Get("c714"),
		baggage.FromContext(ctx).Member("c714").Value(),
		name,
	}
}

type prefixedHeaderKey struct{}

// prefixedHeaderPropagator keeps the name of the first request header that
// starts with "ctx-", the way propagators that map prefixed headers into
// context walk the carrier's Keys() and keep the names they match.
type prefixedHeaderPropagator struct{}

func (prefixedHeaderPropagator) Inject(context.Context, propagation.TextMapCarrier) {}

func (prefixedHeaderPropagator) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	for _, k := range carrier.Keys() {
		if strings.HasPrefix(k, "ctx-") {
			return context.WithValue(ctx, prefixedHeaderKey{}, k)
		}
	}
	return ctx
}

func (prefixedHeaderPropagator) Fields() []string { return nil }

// startC714OtelServer starts the server mk builds on a fresh loopback
// listener and returns its address and a shutdown closure.
//
// An io_uring start that fails only with ENOMEM is retried, with a new
// server, for up to 10 s. The kernel charges ring memory to RLIMIT_MEMLOCK
// per UID and gives it back 12-23 ms after a ring closes
// (internal/engine/iouring/ring_budget_linux_test.go in the celeris module), so at
// the CI runner's 8 MiB a start made right after the previous arm stopped
// can fail although nothing leaked.
func startC714OtelServer(t *testing.T, mk func() *celeris.Server) (string, func()) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for tries := 1; ; tries++ {
		s := mk()
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
// user, are still charged (internal/engine/iouring/ring_budget_linux_test.go).
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
