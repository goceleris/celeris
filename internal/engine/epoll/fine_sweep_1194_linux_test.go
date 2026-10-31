//go:build linux

package epoll

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#1194. The sweep (checkTimeouts) is what closes a connection that ran
// past its IdleTimeout, ReadTimeout or WriteTimeout, and its 25 ms kernel timer
// (the timerfd) used to exist only with ReadHeaderTimeout set. With
// ReadHeaderTimeout -1 an idle loop swept once per 1024 returns of epoll_wait,
// which with the listener open settles at a 4 ms wait: a sweep every 4 to 5 s
// (measured: a 1 s IdleTimeout closed the connection at 3.2 to 5.8 s; with no
// listener the wait is 500 ms and a sweep comes every 8 minutes). The tests pin
// the bound the documentation promises with ReadHeaderTimeout off: a timeout of
// 1 s closes at 1 s plus one sweep period (25 ms), and these windows carry the
// scheduling jitter of a loaded runner on top.

// A connection is closed no sooner than slack1194 before its due time (the
// client's clock starts after the server's stamp), and no later than lateness1194
// after it. The sweep is 25 ms apart; the lateness measured on a container of 2
// CPUs is 0 to 30 ms (see the pull request), and the window is the 100 ms the
// bound of the documentation allows plus margin for a loaded runner.
const (
	slack1194    = 50 * time.Millisecond
	lateness1194 = 200 * time.Millisecond
)

// handler1194 answers every request with "ok", and /big with a body that no
// socket buffer of a loopback connection holds.
type handler1194 struct{ big []byte }

func (h handler1194) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := []byte("ok")
	if s.Path == "/big" {
		body = h.big
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "application/octet-stream"}, {"content-length", strconv.Itoa(len(body))}}, body)
}

// start1194 starts an engine with ReadHeaderTimeout off, so that no timeout
// but the one a test sets is enabled (the others are at their defaults, 60 s
// and 600 s), and returns its address and a channel that receives the time of
// every connection it closes.
func start1194(t *testing.T, mut func(*resource.Config)) (*Engine, string, <-chan time.Time) {
	t.Helper()
	closed := make(chan time.Time, 8)
	eng, addr := startE2E885(t, false, handler1194{big: make([]byte, 32<<20)}, func(c *resource.Config) {
		c.ReadHeaderTimeout = -1
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		c.Resources.SocketSend = 16 << 10
		c.OnDisconnect = func(string) {
			select {
			case closed <- time.Now():
			default:
			}
		}
		mut(c)
	})
	return eng, addr, closed
}

// closeWithin1194 waits for the engine to close the connection and asserts it
// did so within [earliest-slack, latest+lateness] of t0.
func closeWithin1194(t *testing.T, what string, closed <-chan time.Time, t0 time.Time, earliest, latest time.Duration) {
	t.Helper()
	select {
	case at := <-closed:
		got := at.Sub(t0)
		t.Logf("%s: closed after %v", what, got.Round(time.Millisecond))
		if got < earliest-slack1194 {
			t.Errorf("%s: closed after %v, want no sooner than %v (a timeout that fired early)", what, got.Round(time.Millisecond), earliest-slack1194)
		}
		if got > latest+lateness1194 {
			t.Errorf("%s: closed after %v, want within %v of %v (a sweep that waits for the next event or the 500 ms wait)",
				what, got.Round(time.Millisecond), lateness1194, latest)
		}
	case <-time.After(latest + 15*time.Second):
		t.Fatalf("%s: not closed %v after the start, due by %v", what, time.Since(t0).Round(time.Millisecond), latest)
	}
}

// An idle keep-alive connection is closed at IdleTimeout (1 s).
func TestIdleTimeoutIsOnTimeWithoutReadHeaderTimeout1194(t *testing.T) {
	_, addr, closed := start1194(t, func(c *resource.Config) { c.IdleTimeout = time.Second })
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("first response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	closeWithin1194(t, "idle keep-alive conn (IdleTimeout 1 s)", closed, time.Now(), time.Second, time.Second)
}

// A request body that stops arriving is cut at ReadTimeout (1 s) from the last
// byte.
func TestReadTimeoutIsOnTimeWithoutReadHeaderTimeout1194(t *testing.T) {
	_, addr, closed := start1194(t, func(c *resource.Config) { c.ReadTimeout = time.Second })
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := fmt.Fprint(c, "POST /up HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n"+string(make([]byte, 100))); err != nil {
		t.Fatal(err)
	}
	closeWithin1194(t, "stalled upload (ReadTimeout 1 s)", closed, time.Now(), time.Second, time.Second)
}

// A peer that takes nothing of a response it asked for is closed by the stall
// rule of WriteTimeout (1 s): the first sweep that finds the response parked
// takes the baseline, and the window ends at the first sweep a WriteTimeout
// later. The bytes the peer's kernel acknowledges in the first window (more
// than the rule's floor of 64 KiB on a loopback connection) pass it, and the
// close comes at the end of the second: 2.00 to 2.07 s measured with the sweep
// every 25 ms (4.0 to 4.4 s without), 2.06 to 2.17 s under -race, where the
// baseline sweep comes later, and 1.24 s once, with the first window failed
// (the peer's kernel acknowledged less than the floor). The window runs from
// one WriteTimeout to two and 300 ms more than the other tests allow, the
// stall rule's windows being measured from a sweep, not from the request.
func TestWriteTimeoutIsOnTimeWithoutReadHeaderTimeout1194(t *testing.T) {
	_, addr, closed := start1194(t, func(c *resource.Config) { c.WriteTimeout = time.Second })
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	t0 := time.Now()
	if _, err := fmt.Fprint(c, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	closeWithin1194(t, "non-reading peer (WriteTimeout 1 s)", closed, t0, time.Second, 2*time.Second+300*time.Millisecond)
}

// Every timeout rides the sweep, so the sweep has its kernel timer whatever
// the timeouts are, ReadHeaderTimeout included: a loop with all four off still
// has the WebSocket idle deadline of the middleware, the HTTP/2 deadlines and
// the closing drain to reap.
func TestEveryLoopHasASweepTimer1194(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*resource.Config)
	}{
		{"readheader-off", func(*resource.Config) {}},
		{"all-off", func(c *resource.Config) { c.ReadTimeout, c.WriteTimeout, c.IdleTimeout = -1, -1, -1 }},
		{"readheader-default", func(c *resource.Config) { c.ReadHeaderTimeout = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, _, _ := start1194(t, tc.mut)
			for i := 0; i < eng.NumWorkers(); i++ {
				if l := eng.WorkerLoop(i).(*Loop); l.timerFD < 0 {
					t.Errorf("loop %d has no sweep timerfd", i)
				}
			}
		})
	}
}
