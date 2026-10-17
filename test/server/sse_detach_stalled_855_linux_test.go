//go:build linux

package celeris_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

// Tests for celeris#855, the other half: a detached stream is not cut at
// WriteTimeout on std, so a client that stays connected and reads nothing must
// still be got rid of. Two things end such a stream on std, neither of them an
// absolute deadline:
//
//   - a write that has not finished within WriteTimeout fails (a bound on
//     each write, not on the response), so a handler that is bounded, or that
//     is told to drop a slow client, ends the stream within WriteTimeout of
//     the write it is parked in;
//   - the drain budget: Shutdown that runs out of budget fails the writes of
//     the streams still running, as it cancels their contexts, so Start does
//     not return and leave a goroutine, a file descriptor and its buffers
//     behind.
//
// Review round 1 of celeris#998 found both gone after the first version of the
// fix, which cleared the deadlines and put nothing in their place.

// stalledShape855 is how the handler under a client that reads nothing sends.
type stalledShape855 string

const (
	// shapeTight855 sends as fast as Send lets it: parked in Send's write.
	shapeTight855 stalledShape855 = "tight"
	// shapeBudget855 sends as fast as shapeTight855 does until it has sent
	// budget855 bytes, then waits for its context. On every engine that is
	// under the 64 MiB a native engine holds unsent before it closes the conn
	// (celeris#884 item 45, #885 item 30), so only Shutdown can end it.
	shapeBudget855 stalledShape855 = "budget"
	// shapeTick855 sends every 10 ms and watches its context.
	shapeTick855 stalledShape855 = "tick"
	// shapeQueue855 is shapeTick855 through the client queue (MaxQueueDepth),
	// whose drain goroutine is the one parked in the write.
	shapeQueue855 stalledShape855 = "queue"
	// shapeClose855 is a queue of 4 that closes the client when it is full.
	shapeClose855 stalledShape855 = "closepolicy"
)

// budget855 is how much shapeBudget855 sends: 16 MiB, a quarter of the limit.
const budget855 = 16 << 20

// stalledSSE855 is a server whose /events stream is held by a client that
// connected and reads nothing.
type stalledSSE855 struct {
	s         *celeris.Server
	addr      string
	startDone chan error
	// hret and disc are the UnixNano at which the user's handler returned
	// and OnDisconnect ran, 0 until they have.
	hret, disc atomic.Int64
	// sendErr is the UnixNano at which a Send of the handler first failed.
	sendErr atomic.Int64
	conn    net.Conn
	cl      *http.Client
	resp    *http.Response
}

// stallBegin855 starts a server and a client that reads nothing. bound > 0 is
// how long the handler runs before it returns of its own accord.
func stallBegin855(t *testing.T, eng celeris.EngineType, wt, shutdown time.Duration, shape stalledShape855, h2c bool, bound time.Duration) *stalledSSE855 {
	t.Helper()
	st := &stalledSSE855{startDone: make(chan error, 1)}
	payload := strings.Repeat("x", 256<<10)
	cfg := sse.Config{
		HeartbeatInterval: -1,
		OnDisconnect:      func(*celeris.Context, *sse.Client) { st.disc.Store(time.Now().UnixNano()) },
		Handler: func(cl *sse.Client) {
			defer func() { st.hret.Store(time.Now().UnixNano()) }()
			start := time.Now()
			ctx := cl.Context()
			var tick <-chan time.Time
			if shape != shapeTight855 {
				tk := time.NewTicker(10 * time.Millisecond)
				defer tk.Stop()
				tick = tk.C
			}
			sent := 0
			for bound <= 0 || time.Since(start) < bound {
				if shape == shapeBudget855 && sent >= budget855 {
					<-ctx.Done()
					return
				}
				if tick != nil {
					select {
					case <-ctx.Done():
						return
					case <-tick:
					}
				}
				if ctx.Err() != nil {
					return
				}
				if err := cl.Send(sse.Event{Data: payload}); err != nil {
					st.sendErr.CompareAndSwap(0, time.Now().UnixNano())
					return
				}
				sent += len(payload)
			}
		},
	}
	switch shape {
	case shapeQueue855:
		cfg.MaxQueueDepth = 16
	case shapeClose855:
		cfg.MaxQueueDepth = 4
		cfg.OnSlowClient = func(*sse.Client, sse.Event) sse.ClientPolicy { return sse.ClientPolicyClose }
	}
	var addr string
	for tries := 0; ; tries++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr = ln.Addr().String()
		_ = ln.Close()
		s := celeris.New(celeris.Config{Addr: addr, Engine: eng, ReadTimeout: wt, WriteTimeout: wt, ShutdownTimeout: shutdown})
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		s.GET("/events", sse.New(cfg))
		st.s, st.startDone = s, make(chan error, 1)
		go func(done chan error) { done <- s.Start() }(st.startDone)
		err = waitReady761(addr, st.startDone)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond) // a ring's memlock is returned when the engine before it has closed
		if tries >= 15 {
			t.Fatalf("server did not start: %v", err)
		}
	}
	st.addr = addr
	t.Cleanup(func() {
		if st.conn != nil {
			_ = st.conn.Close()
		}
		if st.resp != nil {
			_ = st.resp.Body.Close()
		}
		if st.cl != nil {
			st.cl.CloseIdleConnections()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = st.s.Shutdown(ctx)
	})
	if h2c {
		st.cl = h2cClient836(1)
		st.cl.Timeout = 0
		resp, err := st.cl.Get("http://" + addr + "/events")
		if err != nil {
			t.Fatalf("GET /events over h2c: %v", err)
		}
		if resp.ProtoMajor != 2 {
			t.Fatalf("GET /events came back as %s, not HTTP/2", resp.Proto)
		}
		st.resp = resp // its body is never read
		return st
	}
	d := net.Dialer{Timeout: 2 * time.Second, Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
		})
	}}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	st.conn = conn
	if _, err := conn.Write([]byte("GET /events HTTP/1.1\r\nHost: " + addr + "\r\nAccept: text/event-stream\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	return st
}

// stalledCells855 are the cells of both tests: every engine over HTTP/1.1,
// and std over h2c too. The native engines do not stream h2c (#889).
func stalledCells855(shapes ...stalledShape855) []struct {
	name  string
	eng   celeris.EngineType
	h2c   bool
	shape stalledShape855
} {
	var cells []struct {
		name  string
		eng   celeris.EngineType
		h2c   bool
		shape stalledShape855
	}
	for _, e := range engines761 {
		for _, sh := range shapes {
			if sh == shapeClose855 && e.name != "std" {
				// Pre-existing and not std's: ClientPolicyClose does not end
				// the stream of a client that reads nothing on the native
				// engines (a polish item on #888).
				continue
			}
			cells = append(cells, struct {
				name  string
				eng   celeris.EngineType
				h2c   bool
				shape stalledShape855
			}{e.name + "/h1/" + string(sh), e.eng, false, sh})
			if e.name == "std" {
				cells = append(cells, struct {
					name  string
					eng   celeris.EngineType
					h2c   bool
					shape stalledShape855
				}{e.name + "/h2c/" + string(sh), e.eng, true, sh})
			}
		}
	}
	return cells
}

// TestStalledSSEEndsWithShutdown855: a client that connected and reads
// nothing, WriteTimeout 30 s (so the write deadline is not what ends the
// stream), a drain budget of 1 s. The stream's handler has returned, and
// OnDisconnect has run, by the time Start has returned (within 2 s of it: the
// handler is woken as the budget runs out, not waited for).
func TestStalledSSEEndsWithShutdown855(t *testing.T) {
	for _, cell := range stalledCells855(shapeTight855, shapeBudget855, shapeTick855, shapeQueue855) {
		t.Run(cell.name, func(t *testing.T) {
			if cell.eng == celeris.Std {
				t.Parallel() // the native cells start one at a time: at 8 MiB of RLIMIT_MEMLOCK several rings at once fail ENOMEM
			}
			st := stallBegin855(t, cell.eng, 30*time.Second, time.Second, cell.shape, cell.h2c, 0)
			time.Sleep(time.Second) // the client reads nothing; the socket buffers fill
			if st.hret.Load() != 0 {
				// A native engine refuses a detached conn's write once 64 MiB are
				// unsent, closes the conn and fails the Send (celeris#884 item 45,
				// #885 item 30): the tight handler sends 256 KiB events as fast as
				// Send lets it, and reaches that in well under a second, where it
				// used to spin on writes that were dropped without a word. That is
				// the stream ending, not the handler returning without cause.
				// The close cancels the client's context, so the loop may leave on
				// that before a Send fails: either marks the stream as ended by the
				// engine.
				if cell.eng != celeris.Std && cell.shape == shapeTight855 && (st.sendErr.Load() != 0 || st.disc.Load() != 0) {
					t.Logf("STALL855 cell=%s: the tight handler's stream ended at the engine's unsent limit before Shutdown", cell.name)
					return
				}
				t.Fatal("the handler returned before Shutdown")
			}
			sd := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			serr := st.s.Shutdown(ctx)
			cancel()
			var startErr error
			select {
			case startErr = <-st.startDone:
			case <-time.After(10 * time.Second):
				t.Fatal("Start has not returned 10 s after Shutdown")
			}
			startRet := time.Since(sd)
			deadline := time.Now().Add(2 * time.Second)
			for (st.hret.Load() == 0 || st.disc.Load() == 0) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			rel := func(v int64) string {
				if v == 0 {
					return "never"
				}
				return time.Duration(v - sd.UnixNano()).Round(time.Millisecond).String()
			}
			t.Logf("STALL855 cell=%s shutdownErr=%v startErr=%v startReturned=%v handlerReturned=%s onDisconnect=%s",
				cell.name, serr, startErr, startRet.Round(time.Millisecond), rel(st.hret.Load()), rel(st.disc.Load()))
			if st.hret.Load() == 0 || st.disc.Load() == 0 {
				t.Fatalf("the stalled stream's handler had not returned (%s) or OnDisconnect had not run (%s) 2 s after Start returned",
					rel(st.hret.Load()), rel(st.disc.Load()))
			}
		})
	}
}

// TestStalledSSESendFailsAtWriteTimeoutStd855 is the regression test for the
// two majors of the first review of celeris#998: a client that connects and
// reads nothing, WriteTimeout 1 s. The handler (it never ends of its own
// accord) is parked in Send's write; that Send fails within about 2 s of the
// connect (the socket buffers fill in well under a second, then the write
// has not finished within WriteTimeout), and Shutdown with a 5 s budget returns,
// with Start, at once: nothing is left to wait for. On h1 and on h2c. The
// stream is not cut at a fixed lifetime, only when a write is stuck:
// TestSSEStaysOpenPastServerTimeouts855 holds one that is read for 4.5 s.
func TestStalledSSESendFailsAtWriteTimeoutStd855(t *testing.T) {
	for _, h2c := range []bool{false, true} {
		name := "h1"
		if h2c {
			name = "h2c"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			connect := time.Now()
			st := stallBegin855(t, celeris.Std, time.Second, 5*time.Second, shapeTight855, h2c, 0)
			deadline := time.Now().Add(10 * time.Second)
			for st.sendErr.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if st.sendErr.Load() == 0 {
				t.Fatal("Send has not failed 10 s after a client that reads nothing connected (WriteTimeout 1 s)")
			}
			after := time.Duration(st.sendErr.Load() - connect.UnixNano())
			t.Logf("STALL855 cell=std/%s sendFailedAfter=%v (WriteTimeout 1s)", name, after.Round(10*time.Millisecond))
			if after > 2500*time.Millisecond {
				t.Fatalf("Send failed %v after the connect, want within about 2 s (WriteTimeout 1 s)", after)
			}
			sd := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := st.s.Shutdown(ctx); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			select {
			case err := <-st.startDone:
				t.Logf("STALL855 cell=std/%s shutdownTook=%v startErr=%v", name, time.Since(sd).Round(time.Millisecond), err)
			case <-time.After(5 * time.Second):
				t.Fatal("Start has not returned after Shutdown")
			}
			if st.hret.Load() == 0 || st.disc.Load() == 0 {
				t.Fatal("the stalled stream's handler had not returned or OnDisconnect had not run when Start returned")
			}
			if took := time.Since(sd); took > 3*time.Second {
				t.Fatalf("Shutdown took %v with nothing running, budget 5 s", took)
			}
		})
	}
}

// TestStalledSSEEndsWhenItsHandlerIsBounded855: a client that reads nothing,
// WriteTimeout 2 s, a handler that returns after 3 s (or, for closepolicy, a
// queue of 4 that closes the client when it is full). OnDisconnect has run
// within the handler's bound and the WriteTimeout of the write it is parked
// in, and a few seconds of slack: where the stream used to be held for ever on
// std once it was detached.
func TestStalledSSEEndsWhenItsHandlerIsBounded855(t *testing.T) {
	const (
		wt    = 2 * time.Second
		bound = 3 * time.Second
	)
	for _, cell := range stalledCells855(shapeTight855, shapeClose855) {
		t.Run(cell.name, func(t *testing.T) {
			if cell.eng == celeris.Std {
				t.Parallel() // the native cells start one at a time: at 8 MiB of RLIMIT_MEMLOCK several rings at once fail ENOMEM
			}
			start := time.Now()
			st := stallBegin855(t, cell.eng, wt, 10*time.Second, cell.shape, cell.h2c, bound)
			limit := bound + wt + 3*time.Second
			deadline := time.Now().Add(limit)
			for st.disc.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			after := "never"
			if v := st.disc.Load(); v != 0 {
				after = time.Duration(v - start.UnixNano()).Round(10 * time.Millisecond).String()
			}
			t.Logf("STALL855 cell=%s onDisconnectAfter=%s (bound %v, WriteTimeout %v)", cell.name, after, bound, wt)
			if after == "never" {
				t.Fatalf("OnDisconnect has not run %v after a client that reads nothing connected (handler bound %v, WriteTimeout %v)", limit, bound, wt)
			}
		})
	}
}

// TestSSEIdleBetweenEventsPastWriteTimeoutStd855: the bound is on a write that
// is stuck, not on the stream's age or on a deadline left standing, so a
// stream whose events are further apart than WriteTimeout is not cut while
// idle. Over h2c this is the cell that tells the designs apart: the HTTP/2
// server resets a stream when its write deadline passes, whether or not a
// write is pending, so a deadline that is extended only when a write comes
// (never cleared) kills an idle stream there, where on h1 it would pass.
func TestSSEIdleBetweenEventsPastWriteTimeoutStd855(t *testing.T) {
	for _, h2c := range []bool{false, true} {
		name := "h1"
		if h2c {
			name = "h2c"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			addr := startServerConfig761(t, armWrite855.config(celeris.Std, false), func(s *celeris.Server) {
				s.GET("/events", sse.New(sse.Config{
					HeartbeatInterval: -1,
					Handler: func(client *sse.Client) {
						ctx := client.Context()
						for i := 1; ; i++ {
							select {
							case <-ctx.Done():
								return
							case <-time.After(3 * timeout855 / 2):
							}
							if err := client.Send(sse.Event{Data: fmt.Sprint(i)}); err != nil {
								return
							}
						}
					},
				}))
			})
			var (
				events int
				span   time.Duration
				err    error
			)
			if h2c {
				events, span, err = sseOverH2C855(t, addr)
			} else {
				events, span, err = sseOverH1855(t, addr)
			}
			t.Logf("SSE855 idle cell=%s events=%d span=%v err=%v", name, events, span.Round(time.Millisecond), err)
			if err != nil {
				t.Fatalf("the stream ended after %d events and %v with an event every %v and a WriteTimeout of %v: %v",
					events, span.Round(time.Millisecond), 3*timeout855/2, timeout855, err)
			}
		})
	}
}
