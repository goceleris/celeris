package redis

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
)

// A fake redis for the direct-wait tests (celeris#854). Unlike startFakeRedis
// it flushes a whole pipeline's replies at once, after a controllable delay,
// split into chunks of controllable sizes with a controllable gap, and it can
// fail a reply midway. GET k<i> answers waitVal(i).

type waitFault int32

const (
	waitFaultNone   waitFault = iota
	waitFaultSilent           // read the batch, never reply
	waitFaultFINMid           // write half of the reply, then close (FIN)
	waitFaultRSTMid           // write half of the reply, then close with RST
	waitFaultFINPre           // close (FIN) without any reply byte
)

type waitFake struct {
	ln     net.Listener
	delay  atomic.Int64 // ns before a batch with a GET is answered
	gap    atomic.Int64 // ns between chunks
	fault  atomic.Int32
	chunks atomic.Pointer[[]int] // chunk sizes, cycled; nil = one write
	mu     sync.Mutex
	conns  []net.Conn
}

func startWaitFake(tb testing.TB) *waitFake {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	f := &waitFake{ln: ln}
	tb.Cleanup(func() {
		_ = ln.Close()
		f.dropConns()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns = append(f.conns, c)
			f.mu.Unlock()
			go f.serve(c)
		}
	}()
	return f
}

// dropConns closes every accepted connection (the server goes away).
func (f *waitFake) dropConns() {
	f.mu.Lock()
	cs := f.conns
	f.conns = nil
	f.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
}

func (f *waitFake) addr() string { return f.ln.Addr().String() }

func (f *waitFake) setDelay(d time.Duration) { f.delay.Store(int64(d)) }

// waitVal is the value of GET k<i>: a length and a fill byte that differ per
// key, so a reply parsed at the wrong offset cannot pass for a right one.
func waitVal(i int) string {
	return strings.Repeat(string(rune('a'+i%26)), 1000+37*i)
}

func waitReadCommand(br *bufio.Reader) ([]string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 || line[0] != '*' {
		return nil, fmt.Errorf("fake: bad array header %q", line)
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, n)
	for i := 0; i < n; i++ {
		hdr, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		l, err := strconv.Atoi(strings.TrimSpace(hdr[1:]))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, l+2)
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:l]))
	}
	return args, nil
}

func (f *waitFake) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReaderSize(c, 64<<10)
	for {
		var out bytes.Buffer
		w := bufio.NewWriter(&out)
		hasGet := false
		for {
			cmd, err := waitReadCommand(br)
			if err != nil {
				return
			}
			switch strings.ToUpper(cmd[0]) {
			case "HELLO":
				handleHELLO(w, 3)
			case "GET":
				hasGet = true
				i, _ := strconv.Atoi(strings.TrimPrefix(cmd[1], "k"))
				writeBulk(w, waitVal(i))
			default:
				writeSimple(w, "OK")
			}
			if br.Buffered() == 0 {
				break
			}
		}
		_ = w.Flush()
		if !hasGet {
			if _, err := c.Write(out.Bytes()); err != nil {
				return
			}
			continue
		}
		if !f.reply(c, out.Bytes()) {
			return
		}
	}
}

// reply answers one batch; false ends the connection.
func (f *waitFake) reply(c net.Conn, b []byte) bool {
	fault := waitFault(f.fault.Load())
	if fault == waitFaultSilent {
		return true
	}
	if d := time.Duration(f.delay.Load()); d > 0 {
		time.Sleep(d)
	}
	if fault == waitFaultFINPre {
		return false
	}
	if fault == waitFaultFINMid || fault == waitFaultRSTMid {
		b = b[:len(b)/2]
	}
	gap := time.Duration(f.gap.Load())
	var sizes []int
	if p := f.chunks.Load(); p != nil {
		sizes = *p
	}
	for k := 0; len(b) > 0; k++ {
		n := len(b)
		if len(sizes) > 0 {
			n = min(n, sizes[k%len(sizes)])
		}
		if _, err := c.Write(b[:n]); err != nil {
			return false
		}
		b = b[n:]
		if gap > 0 && len(b) > 0 {
			time.Sleep(gap)
		}
	}
	switch fault {
	case waitFaultFINMid:
		return false
	case waitFaultRSTMid:
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		return false
	}
	return true
}

// newWaitClient is a Client on the direct (AsyncHandlers) command pool.
func newWaitClient(tb testing.TB, addr string, pool int) *Client {
	tb.Helper()
	cfg := Config{Addr: addr}
	WithPoolSize(pool)(&cfg)
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { eventloop.Release(prov) })
	cl := &Client{cfg: cfg, pool: newDirectCmdPool(cfg, prov)}
	tb.Cleanup(func() { _ = cl.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cl.pool.acquireCmd(ctx, 0)
	if err != nil {
		tb.Fatal(err)
	}
	defer cl.pool.releaseCmd(c)
	if !c.useDirect {
		tb.Fatal("fixture: want a direct conn, got useDirect=false")
	}
	return cl
}

// setDirectWaitForTest sets the wait mode for the test and restores it after.
// It is the hook the tests use to pick a mode: they never go through the
// environment, which is read once, on first use.
func setDirectWaitForTest(tb testing.TB, k directWaitKind) {
	tb.Helper()
	prev := directWaitMode.Swap(int32(k))
	tb.Cleanup(func() { directWaitMode.Store(prev) })
}

// resetDirectWaitForTest puts the mode back to "environment not read yet", as
// at program start, and restores the mode and the once after the test.
func resetDirectWaitForTest(tb testing.TB) {
	tb.Helper()
	prev := directWaitMode.Swap(int32(directWaitUnset))
	directWaitOnce = sync.Once{}
	tb.Cleanup(func() {
		directWaitMode.Store(prev)
		directWaitOnce = sync.Once{}
	})
}

// waitWatchdog is how long a test waits for work that must finish before it
// reports a hang rather than blocking until the test binary's timeout.
const waitWatchdog = 20 * time.Second

// withWatchdog runs f and fails the test, from the test goroutine, when f has
// not returned within d.
func withWatchdog(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("hang: no result within %v", d)
	}
}

// waitExec runs one Pipeline of n GETs (k0..k<n-1>) under ctx and returns the
// Exec error. A reply that came back is checked value by value; a mismatch is
// returned as an error too.
func waitExec(ctx context.Context, cl *Client, n int) error {
	p := cl.Pipeline()
	defer p.Release()
	cmds := make([]*StringCmd, n)
	for i := range cmds {
		cmds[i] = p.Get("k" + strconv.Itoa(i))
	}
	if err := p.Exec(ctx); err != nil {
		return err
	}
	for i, c := range cmds {
		got, err := c.Result()
		if err != nil {
			return err
		}
		if got != waitVal(i) {
			return fmt.Errorf("value %d: got %d bytes starting %.8q, want %d bytes", i, len(got), got, len(waitVal(i)))
		}
	}
	return nil
}

// execN runs n Execs one after another, each under its own 5 s deadline, and
// fails the test on the first error or on a hang.
func execN(t *testing.T, cl *Client, n, cmds int) {
	t.Helper()
	withWatchdog(t, waitWatchdog, func() {
		for i := 0; i < n; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := waitExec(ctx, cl, cmds)
			cancel()
			if err != nil {
				t.Errorf("exec %d: %v", i, err)
				return
			}
		}
	})
}
