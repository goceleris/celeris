//go:build linux

package metrics_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/middleware/metrics"
	"github.com/goceleris/celeris/probe"
)

// TestLabelValuesSurviveNextRequest pins the metrics site of celeris#732.
//
// client_golang keeps the label values of every new series for the life of
// the registry and does not copy them. On epoll and io_uring the method (for
// a method the H1 parser does not intern) and a LabelFuncs value read from a
// header are views of the connection's receive buffer, which the engine
// reuses for the connection's next request. Three requests with the same
// layout and different values go over one keep-alive connection; each
// creates a series in requests_total, request_duration_seconds and
// response_size_bytes, and every series must keep its own labels, with no
// Gather error.
func TestLabelValuesSurviveNextRequest(t *testing.T) {
	methods := []string{"TRACE", "PURGE", "MKCOL"}
	for _, a := range keptArms(t) {
		t.Run(a.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			addr, stop := startKeptServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
				srv.Use(metrics.New(metrics.Config{
					Registry:   reg,
					LabelFuncs: map[string]func(*celeris.Context) string{"tenant": func(c *celeris.Context) string { return c.Header("x-tenant") }},
				}))
				for _, m := range methods {
					srv.Handle(m, "/m", func(c *celeris.Context) error { return c.String(200, "ok") })
				}
				return srv
			})
			defer stop()

			conn, br := keptDial(t, addr)
			defer func() { _ = conn.Close() }()
			var want []string
			for i, m := range methods {
				tenant := "tenant-" + strings.Repeat(string(rune('a'+i)), 4)
				keptRoundTrip(t, conn, br, m+" /m HTTP/1.1\r\nHost: x\r\nX-Tenant: "+tenant+"\r\n\r\n")
				want = append(want, m+"|"+tenant)
			}
			sort.Strings(want)
			for _, name := range []string{"celeris_requests_total", "celeris_request_duration_seconds", "celeris_response_size_bytes"} {
				got, gerr := seriesOf(reg, name)
				t.Logf("KEPT732METRICS arm=%s %s series=%q gather_err=%v", a.name, name, got, gerr)
				if gerr != nil || strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("%s series (method|tenant) %q, Gather error %v; want %q and no error", name, got, gerr, want)
				}
			}
		})
	}
}

// TestLabelValuesSurviveConnectionReuse is the cross-connection form of
// TestLabelValuesSurviveNextRequest. When a connection closes, the engine
// pools its receive buffer and the next accepted connection reads into it,
// so a label kept as a view reads another client's request. Each round,
// connection A sends one labelled request and closes; connection B sends a
// request with an Authorization header laid out over the same offsets. No
// label may hold B's bytes. With async handlers the request is parsed from
// the dispatch input buffer, and in this layout no series read B's bytes
// before the fix either (0 of 20 rounds); those two arms are kept as
// coverage, and TestLabelValuesSurviveNextRequest covers async handlers.
func TestLabelValuesSurviveConnectionReuse(t *testing.T) {
	const rounds = 20
	for _, a := range keptArms(t) {
		t.Run(a.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			addr, stop := startKeptServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async, Workers: 2})
				srv.Use(metrics.New(metrics.Config{
					Registry:   reg,
					LabelFuncs: map[string]func(*celeris.Context) string{"tenant": func(c *celeris.Context) string { return c.Header("x-tenant") }},
				}))
				srv.GET("/m", func(c *celeris.Context) error { return c.String(200, "ok") })
				return srv
			})
			defer stop()

			want := []string{"GET|"} // B's requests carry no X-Tenant
			for i := range rounds {
				tenant := "tenant-" + strconv.Itoa(1000+i)
				want = append(want, "GET|"+tenant)
				for _, req := range []string{
					"GET /m HTTP/1.1\r\nHost: x\r\nConnection: close\r\nX-Tenant: " + tenant + "\r\n\r\n",
					"GET /m HTTP/1.1\r\nHost: x\r\nConnection: close\r\nAuthorization: SECRETSECRETSECRETSECRET\r\n\r\n",
				} {
					conn, br := keptDial(t, addr)
					keptRoundTrip(t, conn, br, req)
					_, _ = io.Copy(io.Discard, br) // until the server closes it
					_ = conn.Close()
				}
			}
			sort.Strings(want)
			got, gerr := seriesOf(reg, "celeris_requests_total")
			secret := 0
			for _, s := range got {
				if strings.Contains(s, "SECRET") {
					secret++
				}
			}
			t.Logf("KEPT732METRICSXCONN arm=%s rounds=%d series=%d holding another connection's Authorization bytes=%d gather_err=%v", a.name, rounds, len(got), secret, gerr)
			if gerr != nil || secret > 0 || strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("requests_total series (method|tenant) %q (%d hold Authorization bytes), Gather error %v; want %q and no error", got, secret, gerr, want)
			}
		})
	}
}

// seriesOf gathers reg and returns the series of the named metric as sorted
// "method|tenant" strings.
func seriesOf(reg *prometheus.Registry, name string) ([]string, error) {
	mfs, err := reg.Gather()
	var out []string
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, lp := range m.GetLabel() {
				l[lp.GetName()] = lp.GetValue()
			}
			out = append(out, fmt.Sprintf("%s|%s", l["method"], l["tenant"]))
		}
	}
	sort.Strings(out)
	return out, err
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
