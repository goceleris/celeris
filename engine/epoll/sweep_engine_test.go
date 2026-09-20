//go:build linux

package epoll

// celeris#657 P7: the epoll engine must examine what a drain still holds, not
// only what sends it something.
//
// On the base tree a loop calls tryTransplant at exactly one place: the end of
// a connection's own epoll event. A keep-alive connection that is idle when
// StartTransplant is called produces no event, so nothing looks at it, ever.
// Measured through the adaptive engine: all 64 connections stayed on the
// outgoing epoll for the full 3 s of the observation.
//
// Here that is one assertion with no adaptive engine and no io_uring in the
// picture: a real epoll engine, real established keep-alives, a drain started,
// and not one further byte from any client.

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// metricSoft reads one EngineMetrics field by name, or -1 when this tree has
// no such field, so a log line can name a counter the base does not have yet
// without failing the package build.
func metricSoft(e *Engine, name string) int64 {
	v := reflect.ValueOf(e.Metrics()).FieldByName(name)
	if !v.IsValid() {
		return -1
	}
	return int64(v.Uint())
}

// startSweepEngine runs an epoll engine with loops loops on a free loopback
// port and waits for the bind.
func startSweepEngine(t *testing.T, h *sweepHandler, loops int) (*Engine, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg := resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		Resources:     resource.Resources{Workers: loops},
		AsyncHandlers: h.async,
	}
	e, err := New(cfg, h)
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("engine did not stop within 5s")
		}
	})
	for dl := time.Now().Add(8 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Skip("epoll engine did not bind")
	}
	return e, addr
}

// idleKeepAlives opens n keep-alive connections, serves one request on each
// and leaves them open and silent — the state a switch finds an idle
// connection in. Closed at cleanup.
func idleKeepAlives(t *testing.T, addr string, n int) []net.Conn {
	t.Helper()
	out := make([]net.Conn, 0, n)
	t.Cleanup(func() {
		for _, c := range out {
			_ = c.Close()
		}
	})
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		out = append(out, c)
		if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		_ = resp.Body.Close()
		_ = c.SetReadDeadline(time.Time{})
	}
	return out
}

func TestSweepMovesIdleConnsWithoutTraffic(t *testing.T) {
	sweepMovesIdle(t, false)
}

func TestSweepMovesIdleAsyncConnsWithoutTraffic(t *testing.T) {
	sweepMovesIdle(t, true)
}

func sweepMovesIdle(t *testing.T, async bool) {
	const (
		conns = 16
		bound = 500 * time.Millisecond
	)
	h := &sweepHandler{async: async}
	e, addr := startSweepEngine(t, h, 2)
	idleKeepAlives(t, addr, conns)
	for dl := time.Now().Add(2 * time.Second); time.Now().Before(dl); {
		if e.Metrics().ActiveConnections == conns {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := e.Metrics().ActiveConnections; n != conns {
		t.Fatalf("celeris657 SWEEPIDLE PREMISE: %d of %d conns live on the engine", n, conns)
	}
	if async && e.Metrics().AsyncPromotedConns == 0 {
		t.Fatalf("celeris657 SWEEPIDLE PREMISE: no conn promoted to a dispatch goroutine")
	}

	tgt := &countingTarget{}
	defer tgt.closeAll()
	t0 := time.Now()
	e.StartTransplant(tgt)
	var firstMs int64 = -1
	for {
		el := time.Since(t0)
		got := tgt.count()
		if firstMs < 0 && got > 0 {
			firstMs = el.Milliseconds()
		}
		if got >= conns || el >= bound {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := tgt.count()
	m := e.Metrics()
	t.Logf("celeris657 SWEEPIDLE async=%v adopted=%d of %d first_ms=%d within_ms=%d detached=%d passes=%d "+
		"residual=[det=%d h2=%d pin=%d uns=%d busy=%d]", async, got, conns, firstMs, bound.Milliseconds(),
		m.TransplantDetached, metricSoft(e, "TransplantSweepPasses"),
		metricSoft(e, "TransplantResidualDetached"), metricSoft(e, "TransplantResidualH2"),
		metricSoft(e, "TransplantResidualPinned"), metricSoft(e, "TransplantResidualUnstarted"),
		metricSoft(e, "TransplantResidualBusy"))

	if got < conns {
		t.Errorf("celeris657 SWEEPIDLE: %d of %d idle conns were handed over within %d ms, with no client "+
			"sending anything. A conn is examined only at its own epoll event, and an idle keep-alive has none",
			got, conns, bound.Milliseconds())
	}
	if m.TransplantDetached != uint64(got) {
		t.Errorf("celeris657 SWEEPIDLE LEDGER: TransplantDetached=%d against %d adopts", m.TransplantDetached, got)
	}
}

// sweepHandler answers a tiny 200, and with async set marks every route async
// so each conn is promoted to a per-conn dispatch goroutine.
type sweepHandler struct{ async bool }

func (h *sweepHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}},
		[]byte("ok"))
}

func (h *sweepHandler) RouteAsync(_, _ string) bool { return h.async }
func (h *sweepHandler) HasAsyncRoutes() bool        { return h.async }

var _ stream.AsyncRouteResolver = (*sweepHandler)(nil)
