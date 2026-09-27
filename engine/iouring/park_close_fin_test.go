//go:build linux

package iouring

// celeris#712. finishClose's HTTP/1 fast path is a plain close(fd), and the
// recv the connection still has armed holds its own reference to the file, so
// close(fd) alone sends nothing: the socket goes, and the FIN with it, when
// the cancelled recv completes and puts that reference. On a DEFER_TASKRUN
// ring that completion is task work, and task work runs only inside an
// io_uring_enter with GETEVENTS. A running worker makes one on its next wait.
// A worker that parks in the iteration of the close made none: the pre-park
// submit (celeris#657 A5) is an enter without GETEVENTS, and the park itself
// waits on a Go channel. So the client of a connection closed in the parking
// iteration saw no FIN, and both ends stayed ESTABLISHED, until something
// woke the worker.
//
// Every arm below closes one sync-mode connection the engine gave up on and
// asks one thing of the client's side of it: that the close arrives.

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// asyncFdlHandler is fdlHandler with every route async: the engine then runs
// AsyncHandlers, every HTTP/1 connection gets a detachMu, and closes go
// through finishCloseDetached, whose HTTP/1 branch shuts down SHUT_WR first.
type asyncFdlHandler struct{ fdlHandler }

func (asyncFdlHandler) RouteAsync(_, _ string) bool { return true }
func (asyncFdlHandler) HasAsyncRoutes() bool        { return true }

var _ stream.AsyncRouteResolver = asyncFdlHandler{}

// tcpState712 returns the /proc/net/tcp state column of the socket with this
// local and remote port ("01" ESTABLISHED, "04" FIN_WAIT1, "05" FIN_WAIT2,
// "08" CLOSE_WAIT), or "none". Diagnostic only.
func tcpState712(localPort, remotePort int) string {
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, ln := range strings.Split(string(b), "\n")[1:] {
			fs := strings.Fields(ln)
			if len(fs) < 4 {
				continue
			}
			l, _ := strconv.ParseInt(fs[1][strings.LastIndex(fs[1], ":")+1:], 16, 32)
			r, _ := strconv.ParseInt(fs[2][strings.LastIndex(fs[2], ":")+1:], 16, 32)
			if int(l) == localPort && int(r) == remotePort {
				return fs[3]
			}
		}
	}
	return "none"
}

// clientSeesClose polls c until its peer's close arrives (EOF or reset) or
// bound runs out; it returns how long that took, or -1.
func clientSeesClose(c net.Conn, bound time.Duration) (time.Duration, string) {
	t0 := time.Now()
	for time.Since(t0) < bound {
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		_, err := c.Read(make([]byte, 64))
		if err == nil {
			continue
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			continue
		}
		return time.Since(t0), err.Error()
	}
	return -1, "no close seen"
}

// finAfterClose runs one arm. park: PauseAccept first, so the close takes the
// worker's last connection and the worker parks in that iteration. readTimeout:
// the close is checkTimeouts' ReadTimeout branch instead of the header timer's
// CQE. async: the engine runs AsyncHandlers.
func finAfterClose(t *testing.T, park, readTimeout, async bool) {
	var h stream.Handler = fdlHandler{}
	if async {
		h = asyncFdlHandler{}
	}
	e, addr := startFDLEngine(t, h, func(c *resource.Config) {
		// No TCP_DEFER_ACCEPT, so no pause linger (celeris#662): the
		// listeners close as PauseAccept is called, well inside the
		// connection's deadline, and the close lands on a worker that has
		// no listener, the only kind that parks.
		c.DisableDeferAccept = true
		c.AsyncHandlers = async
		if readTimeout {
			c.ReadHeaderTimeout = 60 * time.Second
			c.ReadTimeout = 300 * time.Millisecond
			c.IdleTimeout = 10 * time.Minute
		} else {
			c.ReadHeaderTimeout = 400 * time.Millisecond
		}
	})
	e.mu.Lock()
	ws := append([]*Worker(nil), e.workers...)
	tier := e.tier.Tier().String()
	e.mu.Unlock()
	allParked := func() bool {
		for _, w := range ws {
			if !w.suspended.Load() {
				return false
			}
		}
		return true
	}
	if !parkWait712(3*time.Second, func() bool {
		m := e.Metrics()
		return m.ActiveConnections == 0 && m.AcceptCount >= 1 && m.AcceptCount == m.CloseCount
	}) {
		t.Fatalf("celeris712 PREMISE: startFDLEngine's probe connection is still live")
	}
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	srvPort := c.RemoteAddr().(*net.TCPAddr).Port
	cliPort := c.LocalAddr().(*net.TCPAddr).Port
	t0 := time.Now()
	const partial = "GET /slow HTTP/1.1\r\nHost: x\r\n" // no blank line: mid-headers until a deadline
	if _, err := c.Write([]byte(partial)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !parkWait712(2*time.Second, func() bool { return e.Metrics().ActiveConnections == 1 }) {
		t.Fatalf("celeris712 PREMISE: the connection was never accepted")
	}
	if park {
		if err := e.PauseAccept(); err != nil {
			t.Fatalf("pause: %v", err)
		}
		if n := e.Metrics().ActiveConnections; n != 1 {
			t.Fatalf("celeris712 PREMISE: the connection closed (active=%d) before the listeners did", n)
		}
	}
	// The engine's close, timed from the client's write.
	var closedAt atomic.Int64
	go func() {
		if parkWait712(10*time.Second, func() bool { return e.Metrics().ActiveConnections == 0 }) {
			closedAt.Store(int64(time.Since(t0)))
		}
	}()
	seen, why := clientSeesClose(c, 3*time.Second)
	seenMs := int64(-1)
	if seen >= 0 {
		seenMs = time.Since(t0).Milliseconds()
	}
	if !parkWait712(10*time.Second, func() bool { return closedAt.Load() != 0 }) {
		t.Fatalf("celeris712 PREMISE: the engine never closed the connection")
	}
	engineMs := time.Duration(closedAt.Load()).Milliseconds()
	parked := allParked()
	if park && !parked {
		// The client's wait can end before the worker sets suspended.
		parked = parkWait712(time.Second, allParked)
	}
	stSrv, stCli := tcpState712(srvPort, cliPort), tcpState712(cliPort, srvPort)
	wakeMs := int64(-1)
	if seen < 0 && park {
		tw := time.Now()
		_ = e.ResumeAccept() // ends the park
		if d, _ := clientSeesClose(c, 2*time.Second); d >= 0 {
			wakeMs = time.Since(tw).Milliseconds()
		}
	}
	t.Logf("celeris712 park=%v read_timeout=%v async=%v tier=%s workers=%d engine_close_ms=%d parked=%v "+
		"client_close_ms=%d (%s) tcp_state_srv=%s tcp_state_cli=%s wake_to_close_ms=%d",
		park, readTimeout, async, tier, len(ws), engineMs, parked, seenMs, why, stSrv, stCli, wakeMs)
	if park && !parked {
		t.Fatalf("celeris712 PREMISE: the worker did not park after the close")
	}
	if !park && parked {
		t.Fatalf("celeris712 PREMISE: a worker with its listener open parked")
	}
	if seen < 0 {
		t.Errorf("celeris712 NOFIN: the engine counted the connection closed %d ms after the client's write, "+
			"but the client saw no close for 3 s (server socket state %s, client %s; the close arrived %d ms "+
			"after a wake)", engineMs, stSrv, stCli, wakeMs)
	}
}

func parkWait712(d time.Duration, f func() bool) bool {
	for dl := time.Now().Add(d); ; {
		if f() {
			return true
		}
		if time.Now().After(dl) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestParkedWorkerSendsFINForAHeaderTimeoutClose: the header timer's CQE
// closes the last connection of a paused worker, which parks in that
// iteration. The measured case of the issue.
func TestParkedWorkerSendsFINForAHeaderTimeoutClose(t *testing.T) {
	finAfterClose(t, true, false, false)
}

// TestParkedWorkerSendsFINForAReadTimeoutClose: the same, with checkTimeouts'
// ReadTimeout branch as the close.
func TestParkedWorkerSendsFINForAReadTimeoutClose(t *testing.T) { finAfterClose(t, true, true, false) }

// TestRunningWorkerSendsFINForAHeaderTimeoutClose is the control: the same
// close on a worker that keeps its listener and keeps waiting on its ring.
func TestRunningWorkerSendsFINForAHeaderTimeoutClose(t *testing.T) {
	finAfterClose(t, false, false, false)
}

// TestParkedAsyncWorkerSendsFINForAHeaderTimeoutClose is the second control:
// an AsyncHandlers engine closes through finishCloseDetached, whose
// shutdown(SHUT_WR) acts on the socket and needs no recv completion.
func TestParkedAsyncWorkerSendsFINForAHeaderTimeoutClose(t *testing.T) {
	finAfterClose(t, true, false, true)
}
