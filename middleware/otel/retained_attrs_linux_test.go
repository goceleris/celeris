//go:build linux

package otel_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/engine"
	celotel "github.com/goceleris/celeris/middleware/otel"
	"github.com/goceleris/celeris/probe"
)

// TestAttributesSurviveNextRequest pins the otel site of celeris#732.
//
// A span processor keeps an ended span until it exports it, and the metric
// SDK keeps every attribute set it has seen as an aggregation key for the
// life of the provider; neither copies strings. On epoll and io_uring the
// request path, Host, User-Agent, the client IP and request ID and scheme
// that middleware derive from headers, a CustomAttributes value, a
// SpanNameFormatter result and a method the H1 parser does not intern are
// views of the connection's receive buffer, which the engine reuses for the
// connection's next request. Three requests with the same layout and
// different values go over one keep-alive connection; every span and every
// duration series must keep its own request's values.
func TestAttributesSurviveNextRequest(t *testing.T) {
	methods := []string{"TRACE", "PURGE", "MKCOL"} // TRACE is standard, the others "_OTHER"
	tenant := func(c *celeris.Context) []attribute.KeyValue {
		return []attribute.KeyValue{attribute.String("tenant", c.Header("x-tenant"))}
	}
	for _, a := range keptArms(t) {
		t.Run(a.name, func(t *testing.T) {
			exp := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			addr, stop := startKeptServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
				// What requestid and proxy middleware store from request headers.
				srv.Use(func(c *celeris.Context) error {
					c.SetRequestID(c.Header("x-request-id"))
					c.SetScheme(c.Header("x-forwarded-proto"))
					return c.Next()
				})
				srv.Use(celotel.New(celotel.Config{
					TracerProvider:         tp,
					MeterProvider:          mp,
					CollectClientIP:        true,
					SpanNameFormatter:      func(c *celeris.Context) string { return c.Path() },
					CustomAttributes:       tenant,
					CustomMetricAttributes: tenant,
					// server.address is opt-in on the metrics (celeris#924);
					// opted in, its copy is checked on the series too.
					MetricServerAddress: true,
				}))
				for _, m := range methods {
					srv.Handle(m, "/o/:id", func(c *celeris.Context) error { return c.String(200, "ok") })
				}
				return srv
			})
			defer stop()

			conn, br := keptDial(t, addr)
			defer func() { _ = conn.Close() }()
			var wantSpans []map[string]string
			var wantSeries []string
			for i, m := range methods {
				l := string(rune('a' + i))
				v := strings.Repeat(l, 4)
				keptRoundTrip(t, conn, br, m+" /o/"+v+" HTTP/1.1\r\nHost: host-"+v+".example\r\nUser-Agent: agent-"+v+
					"\r\nX-Forwarded-For: 10.0.0."+strconv.Itoa(i+1)+"\r\nX-Request-Id: rid-"+v+"\r\nX-Forwarded-Proto: sch"+l+
					"\r\nX-Tenant: tenant-"+v+"\r\n\r\n")
				method, orig := "_OTHER", m
				if m == "TRACE" {
					method, orig = m, ""
				}
				wantSpans = append(wantSpans, map[string]string{
					"name":                         "/o/" + v,
					"http.request.method":          method,
					"http.request.method_original": orig,
					"url.scheme":                   "sch" + l,
					"url.path":                     "/o/" + v,
					"client.address":               "10.0.0." + strconv.Itoa(i+1),
					"server.address":               "host-" + v + ".example",
					"user_agent.original":          "agent-" + v,
					"request.id":                   "rid-" + v,
					"tenant":                       "tenant-" + v,
				})
				// The metric records a scheme other than http or https as the
				// constant _OTHER (celeris#924); the span keeps the request's.
				wantSeries = append(wantSeries, method+"|_OTHER|host-"+v+".example|tenant-"+v)
			}
			sort.Strings(wantSeries)

			// The middleware ends a span after the response reaches the client.
			spans := exp.GetSpans()
			for i := 0; len(spans) < len(methods) && i < 200; i++ {
				time.Sleep(10 * time.Millisecond)
				spans = exp.GetSpans()
			}
			if len(spans) != len(methods) {
				t.Fatalf("got %d spans, want %d", len(spans), len(methods))
			}
			sort.Slice(spans, func(i, j int) bool { return spans[i].StartTime.Before(spans[j].StartTime) })
			var wrong []string
			for i, s := range spans {
				got := map[string]string{"name": s.Name}
				for _, kv := range s.Attributes {
					if kv.Value.Type() == attribute.STRING {
						got[string(kv.Key)] = kv.Value.AsString()
					}
				}
				keys := make([]string, 0, len(wantSpans[i]))
				for k := range wantSpans[i] {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					if got[k] != wantSpans[i][k] {
						wrong = append(wrong, fmt.Sprintf("span %d %s: %q (want %q)", i+1, k, got[k], wantSpans[i][k]))
					}
				}
			}

			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatal(err)
			}
			var series []string
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					h, ok := m.Data.(metricdata.Histogram[float64])
					if m.Name != "http.server.request.duration" || !ok {
						continue
					}
					for _, dp := range h.DataPoints {
						var parts []string
						for _, k := range []attribute.Key{"http.request.method", "url.scheme", "server.address", "tenant"} {
							v, _ := dp.Attributes.Value(k)
							parts = append(parts, v.AsString())
						}
						series = append(series, strings.Join(parts, "|"))
					}
				}
			}
			sort.Strings(series)
			if strings.Join(series, ",") != strings.Join(wantSeries, ",") {
				wrong = append(wrong, fmt.Sprintf("duration series (method|scheme|host|tenant) %q (want %q)", series, wantSeries))
			}
			t.Logf("KEPT732OTEL arm=%s spans=%d series=%d wrong=%d", a.name, len(spans), len(series), len(wrong))
			if len(wrong) > 0 {
				t.Errorf("attributes kept from a request read other bytes after the connection's later requests:\n  %s", strings.Join(wrong, "\n  "))
			}
		})
	}
}

type keptArm struct {
	name   string
	engine celeris.EngineType
	async  bool
}

// keptArms returns std, and epoll and io_uring with sync and async
// handlers. With CELERIS_REQUIRE_IOURING_WORKERS=1 a kernel with no usable
// io_uring fails the test instead of dropping the io_uring arms.
func keptArms(t *testing.T) []keptArm {
	t.Helper()
	arms := []keptArm{
		{"std", celeris.Std, false},
		{"epoll", celeris.Epoll, false},
		{"epoll-async", celeris.Epoll, true},
	}
	if ok, p := keptProbeIOUring(); ok {
		arms = append(arms, keptArm{"io_uring", celeris.IOUring, false}, keptArm{"io_uring-async", celeris.IOUring, true})
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arms", p.IOUringTier, p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s: io_uring arms not run", p.IOUringTier, p.KernelVersion)
	}
	return arms
}

// keptProbeIOUring probes the kernel's io_uring support. With
// CELERIS_REQUIRE_IOURING_WORKERS=1 a probe that finds no usable ring is
// retried for up to 10 s: the probe's ring can fail with ENOMEM against
// RLIMIT_MEMLOCK while the rings of engines stopped moments ago, or of
// another test binary of the same user, are still charged
// (engine/iouring/ring_budget_linux_test.go).
func keptProbeIOUring() (usable bool, p celerisengine.CapabilityProfile) {
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

// startKeptServer starts the server mk builds on a fresh loopback listener
// and returns its address and a shutdown closure. A start that fails only
// with ENOMEM (io_uring ring memory still charged to RLIMIT_MEMLOCK, see
// keptProbeIOUring) is retried with a new server for up to 10 s.
func startKeptServer(t *testing.T, mk func() *celeris.Server) (string, func()) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := mk()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
		addr, err := keptWaitReady(s, done)
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

func keptWaitReady(s *celeris.Server, done <-chan error) (string, error) {
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

func keptDial(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	return conn, bufio.NewReader(conn)
}

// keptRoundTrip writes one request and reads its response; it fails the
// test unless the status is 2xx.
func keptRoundTrip(t *testing.T, conn net.Conn, br *bufio.Reader, req string) {
	t.Helper()
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	n := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if line == "\r\n" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "content-length") {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if _, err := br.Discard(n); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if f := strings.Fields(status); len(f) < 2 || f[1][0] != '2' {
		t.Fatalf("status %q for %q", strings.TrimSpace(status), strings.SplitN(req, "\r\n", 2)[0])
	}
}
