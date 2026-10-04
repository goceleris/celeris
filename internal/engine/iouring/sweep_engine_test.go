//go:build linux

package iouring

// celeris#657 P9/P10: a standby io_uring worker must examine the connections
// it still holds, and it must be able to start doing so at once.
//
// Two things stand in the way on the base tree. Nothing re-examines a
// connection that produces no completion, so an idle keep-alive is never
// looked at (measured: 64 of 64 at every sample for 1.5 s after a revert).
// And a worker with no listen socket waits a full second between ring
// completions (adaptiveTimeout), while the POLL_ADD on the wake eventfd is
// armed only for H2/h2c/driver/Detach work — so even a sweep cannot start
// before that second is up: with the sweep alone the first hand-off after an
// idle revert came at 1001.8-1008.8 ms in 4 of 4 runs, and at 0.7-5.7 ms in
// 4 of 4 with the wake poll kept armed.
//
// The 250 ms bound is what separates the two: far above the sweep's own 2 ms
// cadence, far below the 1 s wait.

import (
	"bufio"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"
)

// metricSoft reads one EngineMetrics field by name, or -1 when this tree has
// no such field. It lets a log line name a counter the base does not have yet
// without failing the package build, so the same test file produces the
// failing-first observation and the fixed one.
func metricSoft(e *Engine, name string) int64 {
	v := reflect.ValueOf(e.Metrics()).FieldByName(name)
	if !v.IsValid() {
		return -1
	}
	return int64(v.Uint())
}

// idleKeepAlives opens n keep-alive connections, serves one request on each
// and then leaves them open and silent — the state every connection is in
// when a switch finds it idle. The connections are closed at test cleanup.
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
		if _, err := c.Write([]byte(fdlGET)); err != nil {
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

func TestWorkerAskWakesTheRing(t *testing.T) {
	const (
		conns = 8
		bound = 250 * time.Millisecond
	)
	e, addr := startFDLEngine(t, fdlHandler{}, nil)
	idleKeepAlives(t, addr, conns)
	// No listen socket: the shape a standby worker is in after a revert, and
	// the one in which its ring wait is a full second.
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	for dl := time.Now().Add(2 * time.Second); time.Now().Before(dl); {
		if e.Metrics().ActiveConnections == conns {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := e.Metrics().ActiveConnections; n != conns {
		t.Fatalf("celeris657 ASKWAKE PREMISE: %d of %d conns live on the paused engine", n, conns)
	}

	tgt := &servingTarget{}
	defer tgt.close()
	t0 := time.Now()
	e.StartTransplant(tgt)
	var firstMs int64 = -1
	for {
		el := time.Since(t0)
		got := tgt.adopted.Load()
		if firstMs < 0 && got > 0 {
			firstMs = el.Milliseconds()
		}
		if got >= conns || el >= bound {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	adopted := tgt.adopted.Load()
	m := e.Metrics()
	t.Logf("celeris657 ASKWAKE adopted=%d of %d first_ms=%d within_ms=%d passes=%d reaps=%d "+
		"residual=[det=%d h2=%d pin=%d uns=%d busy=%d] w1=%d w2=%d",
		adopted, conns, firstMs, bound.Milliseconds(), metricSoft(e, "TransplantSweepPasses"), m.TransplantReaps,
		metricSoft(e, "TransplantResidualDetached"), metricSoft(e, "TransplantResidualH2"),
		metricSoft(e, "TransplantResidualPinned"), metricSoft(e, "TransplantResidualUnstarted"),
		metricSoft(e, "TransplantResidualBusy"),
		m.StaleRecvDataTransplanted+m.StaleRecvDataUnattributed, m.TransplantHandoffInFlight)

	if adopted < conns {
		t.Errorf("celeris657 ASKWAKE: %d of %d idle conns reached the target within %d ms (first at %d ms). "+
			"A worker with no listen socket waits 1 s between completions, and nothing re-examines a conn "+
			"that sends nothing", adopted, conns, bound.Milliseconds(), firstMs)
	}
	if n := m.TransplantHandoffInFlight; n != 0 {
		t.Errorf("celeris657 ASKWAKE W2: %d hand-offs were made with an op in flight, want 0", n)
	}
}
