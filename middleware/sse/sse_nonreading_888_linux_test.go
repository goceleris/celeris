//go:build linux

package sse_test

import (
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

// A client that stays connected and reads nothing of an SSE stream
// (celeris#888 items 38 and 39, celeris#884 item 69).
//
// On epoll and io_uring a detached connection was exempt from WriteTimeout
// (checkTimeouts skipped it), and Client.Send never parks there: the stream's
// drain goroutine empties the 4-slot queue into the engine's own buffer, so
// ClientPolicyClose was never reached either. Nothing ended the stream of a
// client that took nothing, short of the 64 MiB cap on what the engine holds
// for it. WriteTimeout now bounds it as "no progress for WriteTimeout" (the
// peer takes no byte of what the engine has queued for it), and the close
// reaches the stream through SetWSDetachClose, so OnDisconnect runs about
// WriteTimeout after the stream stalled, as it does on std.

const nonReadWriteTimeout888 = time.Second

// disconnect888 is what OnDisconnect reports: when it ran, and how many events
// dropEarly dropped on a full queue before that (logged by the test, never by
// OnDisconnect, which can run after the subtest has returned).
type disconnect888 struct {
	at    time.Time
	early int64
}

// nonReadCell888 is one engine configuration of the matrix.
type nonReadCell888 struct {
	name   string
	engine celeris.EngineType
	async  bool
}

func nonReadCells888(t *testing.T) []nonReadCell888 {
	t.Helper()
	var cells []nonReadCell888
	hasURing := false
	for _, kind := range sseNativeEngineKinds(t) {
		if kind == celeris.IOUring {
			hasURing = true
		}
		cells = append(cells,
			nonReadCell888{kind.String() + "/async", kind, true},
			nonReadCell888{kind.String() + "/sync", kind, false},
		)
	}
	if hasURing {
		cells = append(cells, nonReadCell888{"adaptive/async", celeris.Adaptive, true})
	}
	// std is the control: its writes are bounded by WriteTimeout one at a time.
	return append(cells, nonReadCell888{"std/async", celeris.Std, true})
}

// producer888 shapes what the handler sends: one event of burstBytes at once,
// which fills the socket buffers (a few MB on a loopback connection whose peer
// has a 4 KiB receive buffer) and leaves the rest queued in the engine, then,
// after settle, trickleBytes every trickleEvery until the client is gone. One
// event, not many, and a pause before the next: while the engine moves a burst
// of MB into the socket, it holds the connection's write lock for each partial
// send, the drain goroutine waits for it, and events sent meanwhile overflow the
// 4-slot queue: ClientPolicyClose would end the stream for that reason,
// whatever the engine did about the peer. The totals stay far
// below the 64 MiB the engine holds for a detached conn (celeris#1067): that
// cap closes the conn too, and a test that reached it would pass without the
// stall bound.
//
// With fastFor set, the burst is skipped and fastBytes every fastEvery is sent
// for fastFor first (the shape of a chat or ticker stream whose peer stopped
// reading: many small events, until the socket is full), then the trickle.
type producer888 struct {
	burstBytes   int
	settle       time.Duration
	fastBytes    int
	fastEvery    time.Duration
	fastFor      time.Duration
	trickleBytes int
	trickleEvery time.Duration

	// writeTimeout is the server's WriteTimeout (nonReadWriteTimeout888 when
	// zero). noHeaderTimer sets ReadHeaderTimeout -1, which takes away the
	// epoll engine's 25 ms timerfd: with a detached conn the sweep then
	// depends on the loop's own iterations.
	writeTimeout  time.Duration
	noHeaderTimer bool

	// dropEarly makes a full queue drop the event, not close the stream, for
	// the first WriteTimeout of the handler's run. The fast phase sends 2000
	// events a second; a drain goroutine that falls 4 events behind (the loop
	// holds the connection's write lock for a partial send, or the runner is
	// starved under -race and coverage) is a full queue, and
	// ClientPolicyClose would end the stream at once, which says nothing about
	// the stall bound this test measures (CI saw cuts at 0.5 s, when the
	// socket fills). From WriteTimeout on the policy is Close, as in the other
	// producers.
	dropEarly bool
}

func (p producer888) wt() time.Duration {
	if p.writeTimeout > 0 {
		return p.writeTimeout
	}
	return nonReadWriteTimeout888
}

// burstFor888 is a burst that exceeds the send queue of a loopback connection
// whose peer has a 4 KiB receive buffer: twice the largest send buffer the
// kernel autotunes to (tcp_wmem), at least 8 MiB. It skips the test where that
// is over 24 MiB (a tuned host), since the engine's own cap is 64 MiB.
func burstFor888(tb testing.TB) int {
	tb.Helper()
	wmem := 4 << 20
	if b, err := os.ReadFile("/proc/sys/net/ipv4/tcp_wmem"); err == nil {
		if f := strings.Fields(string(b)); len(f) == 3 {
			if n, err := strconv.Atoi(f[2]); err == nil {
				wmem = n
			}
		}
	}
	n := max(2*wmem, 8<<20)
	if n > 24<<20 {
		tb.Skipf("tcp_wmem allows a send buffer of %d MiB: a burst that fills it nears the engine's 64 MiB cap", wmem>>20)
	}
	return n
}

// serve888 runs an SSE stream that produces by p until the client is gone,
// and reports when OnDisconnect ran. MaxQueueDepth 4 with ClientPolicyClose is
// the shape the finding names: the policy that was never reached.
func serve888(tb testing.TB, cell nonReadCell888, p producer888) (addr string, disconnected <-chan disconnect888, shutdown func()) {
	tb.Helper()
	done := make(chan disconnect888, 1)
	var started, early atomic.Int64 // handler start (unix nanos), events dropped by dropEarly
	wt := p.wt()
	var burst string
	if p.fastFor == 0 {
		burst = strings.Repeat("x", p.burstBytes)
	}
	trickle := strings.Repeat("y", p.trickleBytes)
	handler := sse.New(sse.Config{
		HeartbeatInterval: -1,
		MaxQueueDepth:     4,
		OnSlowClient: func(*sse.Client, sse.Event) sse.ClientPolicy {
			if p.dropEarly && time.Since(time.Unix(0, started.Load())) < wt {
				early.Add(1)
				return sse.ClientPolicyDrop
			}
			return sse.ClientPolicyClose
		},
		OnDisconnect: func(*celeris.Context, *sse.Client) {
			select {
			case done <- disconnect888{time.Now(), early.Load()}:
			default:
			}
		},
		Handler: func(client *sse.Client) {
			started.Store(time.Now().UnixNano())
			ctx := client.Context()
			if p.fastFor > 0 {
				fast := strings.Repeat("z", p.fastBytes)
				tk := time.NewTicker(p.fastEvery)
				defer tk.Stop()
				for end := time.Now().Add(p.fastFor); time.Now().Before(end); {
					select {
					case <-ctx.Done():
						return
					case <-tk.C:
						if err := client.Send(sse.Event{Event: "fast", Data: fast}); err != nil {
							return
						}
					}
				}
			} else if err := client.Send(sse.Event{Event: "burst", Data: burst}); err != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(p.settle):
			}
			tick := time.NewTicker(p.trickleEvery)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if err := client.Send(sse.Event{Event: "tick", Data: trickle}); err != nil {
						return
					}
				}
			}
		},
	})
	cfg := celeris.Config{
		Engine:          cell.engine,
		AsyncHandlers:   cell.async,
		WriteTimeout:    p.wt(),
		ShutdownTimeout: 2 * time.Second,
	}
	if p.noHeaderTimer {
		cfg.ReadHeaderTimeout = -1
	}
	addr, shutdown = startSSEServerCfg(tb, cfg, false, handler)
	return addr, done, shutdown
}

// dialSmallRcvBuf888 dials addr with a 4 KiB receive buffer set before the
// connect, so that the server's send queue to this client fills within the
// first few hundred KiB, sends the SSE request and reads exactly the response
// head, byte by byte. It returns the conn with nothing more read.
func dialSmallRcvBuf888(tb testing.TB, addr string) net.Conn {
	tb.Helper()
	d := net.Dialer{
		Timeout: 2 * time.Second,
		Control: func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
			}); err != nil {
				return err
			}
			return serr
		},
	}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		tb.Fatalf("dial: %v", err)
	}
	tb.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	req := "GET /events HTTP/1.1\r\nHost: " + addr + "\r\nAccept: text/event-stream\r\nConnection: keep-alive\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		tb.Fatalf("write request: %v", err)
	}
	var head []byte
	one := make([]byte, 1)
	for !strings.HasSuffix(string(head), "\r\n\r\n") {
		if _, err := conn.Read(one); err != nil {
			tb.Fatalf("read head after %q: %v", head, err)
		}
		head = append(head, one[0])
	}
	if !strings.HasPrefix(string(head), "HTTP/1.1 200") {
		tb.Fatalf("response head %q, want 200", head)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn
}

// TestNonReadingClientEndsStream888 pins the finding: a client that took the
// head and reads nothing more ends its stream about WriteTimeout after the
// engine's queue for it stalled, and not before WriteTimeout. The lower bound
// is what tells this close from an early one (a cut at accept or at the first
// event); the producers here stay far below the 64 MiB cap (celeris#1067) for
// the whole run, so it is not the cap that ends the stream either.
func TestNonReadingClientEndsStream888(t *testing.T) {
	prods := []struct {
		name string
		p    producer888
	}{
		// The burst fills the socket and queues the rest in the engine; then
		// a trickle of 1 KB every 5 ms.
		{"steady", producer888{burstBytes: burstFor888(t), settle: 700 * time.Millisecond, trickleBytes: 1000, trickleEvery: 5 * time.Millisecond}},
		// The same, then one event every 300 ms: a sweep must still reach the
		// conn when almost nothing wakes the worker.
		{"sparse", producer888{burstBytes: burstFor888(t), settle: 700 * time.Millisecond, trickleBytes: 1000, trickleEvery: 300 * time.Millisecond}},
		// No burst: 4 KB events every 0.5 ms for 1.2 s fill the socket with
		// small sends (an io_uring SEND of under 64 KiB is not one the worker
		// is told to expect to wait for), then one event every 300 ms.
		// WriteTimeout 2 s puts the close after the fast phase.
		{"small-events", producer888{fastBytes: 4 << 10, fastEvery: 500 * time.Microsecond, fastFor: 1200 * time.Millisecond, settle: 100 * time.Millisecond,
			trickleBytes: 1000, trickleEvery: 300 * time.Millisecond, writeTimeout: 2 * time.Second, dropEarly: true}},
		// The same without the epoll timerfd (ReadHeaderTimeout -1), so nothing
		// but the loop's own iterations reaches the sweep.
		{"small-events-no-header-timer", producer888{fastBytes: 4 << 10, fastEvery: 500 * time.Microsecond, fastFor: 1200 * time.Millisecond, settle: 100 * time.Millisecond,
			trickleBytes: 1000, trickleEvery: 300 * time.Millisecond, writeTimeout: 2 * time.Second, noHeaderTimer: true, dropEarly: true}},
	}
	for _, cell := range nonReadCells888(t) {
		for _, pr := range prods {
			t.Run(cell.name+"/"+pr.name, func(t *testing.T) {
				addr, disconnected, shutdown := serve888(t, cell, pr.p)
				defer shutdown()
				began := time.Now()
				_ = dialSmallRcvBuf888(t, addr)
				const slack = 5 * time.Second
				wt := pr.p.wt()
				select {
				case d := <-disconnected:
					el := d.at.Sub(began)
					t.Logf("OnDisconnect %v after connect (WriteTimeout %v), %d events dropped on a full queue before WriteTimeout", el.Round(time.Millisecond), wt, d.early)
					// std ends a stream whose write is blocked as soon as the
					// queue behind it is full (ClientPolicyClose), earlier than
					// WriteTimeout: the lower bound is for the native engines.
					if cell.engine != celeris.Std && el < wt {
						t.Errorf("OnDisconnect ran %v after connect, before WriteTimeout %v: the stream was ended by something other than the stall bound", el, wt)
					}
				case <-time.After(wt + slack):
					t.Fatalf("OnDisconnect never ran %v after connect (WriteTimeout %v): a client that reads nothing holds its stream for ever (celeris#888 item 38)",
						wt+slack, wt)
				}
			})
		}
	}
}

// TestSlowReadingClientKeepsStream888 is the control that the bound is not
// "the stream is older than WriteTimeout": a client that reads steadily, but
// slower than the producer writes, keeps a backlog queued for the whole run
// (the stall bound's trigger is on all that time) and its stream must live for
// several WriteTimeouts. Bytes the client takes restart the clock; a clock
// keyed on the last read of the connection (the request, which is long ago)
// would end it.
func TestSlowReadingClientKeepsStream888(t *testing.T) {
	for _, cell := range nonReadCells888(t) {
		if cell.engine == celeris.Std {
			continue // std closes a stream whose queue is full (ClientPolicyClose) at once
		}
		t.Run(cell.name, func(t *testing.T) {
			// The burst fills the socket and queues the rest in the engine;
			// then 400 KB/s into a client that takes about 100 KB/s: bytes are
			// queued for it the whole run, and the backlog (a few MB) stays far
			// below 64 MiB.
			addr, disconnected, shutdown := serve888(t, cell, producer888{burstBytes: burstFor888(t), settle: 700 * time.Millisecond, trickleBytes: 4 << 10, trickleEvery: 10 * time.Millisecond})
			defer shutdown()
			conn := dialSmallRcvBuf888(t, addr)
			stop := make(chan struct{})
			readDone := make(chan int64, 1)
			go func() {
				var total int64
				buf := make([]byte, 4096)
				tk := time.NewTicker(40 * time.Millisecond)
				defer tk.Stop()
				for {
					select {
					case <-stop:
						readDone <- total
						return
					case <-tk.C:
					}
					_ = conn.SetReadDeadline(time.Now().Add(time.Second))
					n, err := conn.Read(buf)
					total += int64(n)
					if err != nil {
						readDone <- total
						return
					}
				}
			}()
			const run = 5 * nonReadWriteTimeout888
			select {
			case <-disconnected:
				close(stop)
				t.Fatalf("the stream of a client that kept reading (%d bytes taken) was ended before %v (WriteTimeout %v)",
					<-readDone, run, nonReadWriteTimeout888)
			case <-time.After(run):
			}
			close(stop)
			if got := <-readDone; got < 100<<10 {
				t.Fatalf("the client took only %d bytes in %v: the control did not read", got, run)
			}
			t.Logf("the stream lived %v with a reading client", run)
		})
	}
}
