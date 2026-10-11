//go:build linux

package iouring

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#811: a promoted async conn whose ring SEND waits for a client that
// does not read stayed on the dirty list for as long as the SEND was in
// flight. flushDirty skips a conn with a SEND outstanding and removes it only
// when the SEND is done, and baseTimeout returns 0 while the list is not
// empty, so every pass of the loop waited with a zero timeout: the worker
// never parked, and burned a core (the issue measured ~370 ms of CPU per
// second for one such conn) for as long as the client held out.
//
// These tests are the issue's measurement as a test. N async conns ask for a
// large response and do not read it (a 4 KiB receive buffer, so the server's
// direct write takes a few hundred KiB at most and the rest goes out as a ring
// SEND, which the client's closed window keeps outstanding). The process's CPU
// time over a window is the budget; then every client reads, and must get
// every byte, in order, and the answer to a request pipelined behind the
// response, which only the worker can send once the SEND completes (the wake
// the worker owes when it stops spinning).
//
// Process CPU (getrusage), not a per-thread figure: the worker is the only
// thread doing work in the window, and the process figure is race-safe and
// has no thread to find. The per-thread maximum is logged as a diagnostic.

const (
	// busy811Conns is N: every conn is its own dirty-list entry, and a loop
	// that spins burns the same core whatever N is.
	busy811Conns = 4
	// busy811Body is each conn's response. The server's send buffer is set to
	// 64 KiB (busy811SndBuf), so the direct write is short on any host and a
	// ring SEND carries the rest (the autotuned default takes up to a few MiB,
	// and on some hosts the whole response). It stays under the HTTP/1
	// back-pressure limit (maxSendQueueBytes, 4 MiB): a pipelined request
	// behind a larger backlog is refused and the conn closed (celeris#761).
	busy811Body = 3 << 20
	// busy811Window is the CPU measurement's length. The window starts
	// busy811Settle after the last request was sent, when every SEND is in
	// flight and nothing can progress.
	busy811SndBuf = 64 << 10
	busy811Window = 2 * time.Second
	busy811Settle = 500 * time.Millisecond
	// busy811Budget is the CPU a worker may use in the window while nothing
	// can progress, as a fraction of one core. A worker that parks measures
	// 0.5% to 3% (a timed wait wakes it every 25-100 ms); one that spins, 37%
	// or more in the issue's measurement. 10% is far from both, and -race and
	// a loaded runner move the parked figure by a few percent at most.
	busy811Budget = 0.10
	// busy811ResumeBound bounds the whole resumed transfer of busy811Conns
	// responses: a worker that parked and was not woken never finishes it.
	busy811ResumeBound = 40 * time.Second
)

// busy811Pattern is the byte at offset i of a response body.
func busy811Pattern(i int) byte { return byte(i*31 + 7) }

type busyHandler811 struct{ body []byte }

func newBusyHandler811() *busyHandler811 {
	b := make([]byte, busy811Body)
	for i := range b {
		b[i] = busy811Pattern(i)
	}
	return &busyHandler811{body: b}
}

func (h *busyHandler811) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := []byte("small")
	if s.Path == "/big" {
		body = h.body
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "application/octet-stream"}, {"content-length", strconv.Itoa(len(body))}},
		body)
}
func (*busyHandler811) RouteAsync(_, _ string) bool { return true }
func (*busyHandler811) HasAsyncRoutes() bool        { return true }

var _ stream.AsyncRouteResolver = (*busyHandler811)(nil)

// busyClient811 is a client with a 4 KiB receive buffer that has not read.
type busyClient811 struct {
	c  net.Conn
	br *bufio.Reader
}

func dialBusy811(t *testing.T, addr string) *busyClient811 {
	t.Helper()
	d := net.Dialer{
		Timeout: 3 * time.Second,
		Control: func(_, _ string, rc syscall.RawConn) error {
			var serr error
			if err := rc.Control(func(fd uintptr) {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
			}); err != nil {
				return err
			}
			return serr
		},
	}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &busyClient811{c: c, br: bufio.NewReaderSize(c, 64<<10)}
}

func (b *busyClient811) send(t *testing.T, path string) {
	t.Helper()
	_ = b.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(b.c, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path); err != nil {
		t.Fatalf("send %s: %v", path, err)
	}
}

// cpu811 is the process's user+system CPU time.
func cpu811(t *testing.T) time.Duration {
	t.Helper()
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// threadCPU811 is each thread's user+system clock ticks, from /proc.
func threadCPU811() map[string]uint64 {
	out := map[string]uint64{}
	ents, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return out
	}
	for _, e := range ents {
		raw, err := os.ReadFile("/proc/self/task/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// The command is parenthesised and may hold spaces: split after the
		// last ')'. utime and stime are fields 14 and 15 overall, 12 and 13
		// after the state.
		s := string(raw)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 13 {
			continue
		}
		u, _ := strconv.ParseUint(f[11], 10, 64)
		sy, _ := strconv.ParseUint(f[12], 10, 64)
		out[e.Name()] = u + sy
	}
	return out
}

// busiestThread811 is the thread that used the most CPU between two
// threadCPU811 snapshots, in milliseconds (the clock tick is 10 ms).
func busiestThread811(before, after map[string]uint64) (tid string, ms int) {
	var best uint64
	for k, v := range after {
		if d := v - before[k]; d >= best {
			best, tid = d, k
		}
	}
	return tid, int(best) * 10
}

// readBig811 reads one response whole and checks it: 200, busy811Body bytes,
// every one at its offset.
func readBig811(b *busyClient811) error {
	resp, err := http.ReadResponse(b.br, nil)
	if err != nil {
		return fmt.Errorf("response head: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 || resp.ContentLength != busy811Body {
		return fmt.Errorf("status %d, content-length %d; want 200 and %d", resp.StatusCode, resp.ContentLength, busy811Body)
	}
	buf := make([]byte, 256<<10)
	off := 0
	for off < busy811Body {
		n, err := resp.Body.Read(buf)
		for i := 0; i < n; i++ {
			if buf[i] != busy811Pattern(off+i) {
				return fmt.Errorf("byte %d is %#x, want %#x: the response is not the one the handler wrote",
					off+i, buf[i], busy811Pattern(off+i))
			}
		}
		off += n
		if err != nil {
			if err == io.EOF && off == busy811Body {
				break
			}
			return fmt.Errorf("after %d of %d body bytes: %w", off, busy811Body, err)
		}
	}
	return nil
}

func readSmall811(b *busyClient811) error {
	resp, err := http.ReadResponse(b.br, nil)
	if err != nil {
		return fmt.Errorf("second response head: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, []byte("small")) {
		return fmt.Errorf("second response body %q, err %v; want %q", got, err, "small")
	}
	return nil
}

// TestIouringStalledAsyncSendDoesNotBusyPoll811: the CPU budget and the wake.
//
// Arm single: one request per conn, answered by a response the client does not
// read. Arm pipelined: the same, plus a second request sent while the first
// response's SEND is in flight, which the dispatch goroutine answers into
// writeBuf behind that SEND. Both end the same way: the client reads, and
// every byte arrives, in order, with the second response last.
func TestIouringStalledAsyncSendDoesNotBusyPoll811(t *testing.T) {
	h := newBusyHandler811()
	e, addr := startFDLEngine(t, h, func(c *resource.Config) {
		c.AsyncHandlers = true
		c.Resources.SocketSend = busy811SndBuf
	})
	t.Logf("celeris811 engine workers=%d tier=%s memlock_kib=%s tier_env=%q", e.NumWorkers(), tier844(e),
		memlock811(), os.Getenv("CELERIS_MAX_IOURING_TIER"))

	// The idle baseline: the same process, the engine up, nothing connected. A
	// red run is read against it.
	i0, it0 := cpu811(t), time.Now()
	time.Sleep(500 * time.Millisecond)
	t.Logf("celeris811 IDLE window_ms=%d cpu_ms=%.1f", time.Since(it0).Milliseconds(), float64(cpu811(t)-i0)/1e6)

	for _, pipelined := range []bool{false, true} {
		arm := "single"
		if pipelined {
			arm = "pipelined"
		}
		t.Run(arm, func(t *testing.T) {
			clients := make([]*busyClient811, busy811Conns)
			for i := range clients {
				clients[i] = dialBusy811(t, addr)
				clients[i].send(t, "/big")
			}
			if pipelined {
				time.Sleep(300 * time.Millisecond) // every first SEND is in flight
				for _, c := range clients {
					c.send(t, "/small")
				}
			}
			time.Sleep(busy811Settle)

			th0, c0, t0 := threadCPU811(), cpu811(t), time.Now()
			time.Sleep(busy811Window)
			spent, wall := cpu811(t)-c0, time.Since(t0)
			tid, tms := busiestThread811(th0, threadCPU811())
			frac := float64(spent) / float64(wall)
			t.Logf("celeris811 CPU arm=%s conns=%d window_ms=%d cpu_ms=%.1f cpu_frac=%.3f busiest_thread=%s busiest_thread_ms=%d",
				arm, busy811Conns, wall.Milliseconds(), float64(spent)/1e6, frac, tid, tms)
			if frac > busy811Budget {
				t.Errorf("celeris#811: %d async conns whose clients do not read, with a ring SEND each outstanding, "+
					"cost the worker %.0f ms of CPU in %d ms (%.1f%% of a core, budget %.0f%%): it is not parked",
					busy811Conns, float64(spent)/1e6, wall.Milliseconds(), 100*frac, 100*busy811Budget)
			}

			// The wake: every client reads now.
			errs := make(chan error, len(clients))
			t0 = time.Now()
			first := make(chan time.Duration, len(clients))
			for _, c := range clients {
				go func() {
					_ = c.c.SetReadDeadline(time.Now().Add(busy811ResumeBound))
					if _, err := c.br.Peek(1); err != nil {
						errs <- fmt.Errorf("first byte: %w", err)
						return
					}
					first <- time.Since(t0)
					if err := readBig811(c); err != nil {
						errs <- err
						return
					}
					if pipelined {
						errs <- readSmall811(c)
						return
					}
					errs <- nil
				}()
			}
			for range clients {
				if err := <-errs; err != nil {
					t.Errorf("celeris#811: after the clients resumed reading: %v", err)
				}
			}
			var ttfb time.Duration
			for len(first) > 0 {
				ttfb = max(ttfb, <-first)
			}
			t.Logf("celeris811 RESUME arm=%s conns=%d first_byte_max_ms=%.1f transfer_ms=%.1f bytes=%d", arm, busy811Conns,
				float64(ttfb)/1e6, float64(time.Since(t0))/1e6, busy811Conns*busy811Body)
		})
	}
}

// memlock811 is RLIMIT_MEMLOCK in KiB, "unlimited" if it is.
func memlock811() string {
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_MEMLOCK, &rl); err != nil {
		return "?"
	}
	if rl.Cur == ^uint64(0) {
		return "unlimited"
	}
	return strconv.FormatUint(rl.Cur>>10, 10)
}
