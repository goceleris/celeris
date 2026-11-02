package memcached

// celeris#1218: Config.Timeout, documented as the per-operation default (3 s), was
// never applied. A command on a server that accepts TCP and never answers it held
// the caller until its context ended: for ever under context.Background().

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/memcached/protocol"
)

// startMuteServer accepts TCP conns and reads what they send. For the binary
// protocol it answers the first request of a conn (the VERSION handshake) and
// nothing after it; for the text protocol it never answers. It counts the conns
// the client closed.
func startMuteServer(t *testing.T, binaryProto bool) (addr string, closed *int32Counter) {
	t.Helper()
	closed = &int32Counter{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go func() {
				defer closed.add()
				if binaryProto {
					var hdr [protocol.BinHeaderLen]byte
					if _, err := io.ReadFull(c, hdr[:]); err != nil {
						return
					}
					body := make([]byte, binary.BigEndian.Uint32(hdr[8:12]))
					if _, err := io.ReadFull(c, body); err != nil {
						return
					}
					var out bytes.Buffer
					writeResp(&out, hdr[1], 0, 0, binary.BigEndian.Uint32(hdr[12:16]), nil, nil, []byte("1.6.0"))
					if _, err := c.Write(out.Bytes()); err != nil {
						return
					}
				}
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()
	return ln.Addr().String(), closed
}

type int32Counter struct {
	mu sync.Mutex
	n  int
}

func (c *int32Counter) add() { c.mu.Lock(); c.n++; c.mu.Unlock() }

// opClients are the three ways a Client reaches its conns: direct (what a client
// without an engine uses), the unix event loop, and the net.Conn loop.
var opClients = []struct {
	name string
	new  func(t *testing.T, cfg Config) *Client
}{
	{"direct", func(t *testing.T, cfg Config) *Client {
		c := &Client{cfg: cfg, pool: newDirectPool(cfg)}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}},
	{"eventloop", func(t *testing.T, cfg Config) *Client {
		prov, err := eventloop.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		c := &Client{cfg: cfg, pool: newPool(cfg, prov, true)}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}},
	{"netconn", func(t *testing.T, cfg Config) *Client {
		prov := netConnProvider(t)
		p := &memcachedPool{cfg: cfg, provider: prov}
		p.pool = async.NewPool[*memcachedConn](async.PoolConfig{
			MaxOpen: 4, MaxIdlePerWorker: 2, MaxLifetime: time.Hour, MaxIdleTime: time.Hour, NumWorkers: 2,
		}, func(ctx context.Context, w int) (*memcachedConn, error) {
			return dialMemcachedConnNetConn(ctx, prov, cfg, w)
		})
		c := &Client{cfg: cfg, pool: p}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}},
}

// opWithin runs op and fails the test (not the test binary's timeout) if it is
// still blocked after limit.
func opWithin(t *testing.T, limit time.Duration, op func() error) (time.Duration, error) {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- op() }()
	select {
	case err := <-done:
		return time.Since(start), err
	case <-time.After(limit):
		t.Fatalf("the operation is still blocked after %v: Config.Timeout is not applied", limit)
		return 0, nil
	}
}

// A command on a server that never answers: it fails after about Config.Timeout,
// with a context that has no deadline and with one that is much later, for the text
// and the binary protocol, on the three paths, for a command (Get) and for Ping and
// Version. The conn it used is dropped from the pool.
func TestOperationIsBoundedByTimeout1218(t *testing.T) {
	const bound = 300 * time.Millisecond
	for _, proto := range []Protocol{ProtocolText, ProtocolBinary} {
		for _, cl := range opClients {
			for _, op := range []string{"Get", "Ping", "Version"} {
				for _, cx := range []string{"background", "later-deadline"} {
					t.Run(proto.String()+"/"+cl.name+"/"+op+"/"+cx, func(t *testing.T) {
						t.Parallel()
						addr, _ := startMuteServer(t, proto == ProtocolBinary)
						c := cl.new(t, Config{Addr: addr, Protocol: proto, Timeout: bound, DialTimeout: 5 * time.Second})
						ctx := context.Background()
						if cx == "later-deadline" {
							var cancel context.CancelFunc
							ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
							defer cancel()
						}
						elapsed, err := opWithin(t, 2*time.Second, func() error {
							switch op {
							case "Get":
								_, err := c.Get(ctx, "k")
								return err
							case "Ping":
								return c.Ping(ctx)
							}
							_, err := c.Version(ctx)
							return err
						})
						t.Logf("%s: %v after %v", t.Name(), err, elapsed.Round(10*time.Millisecond))
						if err == nil {
							t.Fatal("no error")
						}
						if !errors.Is(err, context.DeadlineExceeded) {
							t.Errorf("error %v does not match context.DeadlineExceeded", err)
						}
						if !strings.Contains(err.Error(), "timed out") {
							t.Errorf("error %q does not say that the operation timed out", err)
						}
						if elapsed < bound/2 {
							t.Errorf("returned after %v, before the bound of %v", elapsed, bound)
						}
						if n := c.PoolStats().Open; n != 0 {
							t.Errorf("the pool holds %d conns after the timeout, want 0 (the conn is poisoned)", n)
						}
					})
				}
			}
		}
	}
}

// A caller's shorter deadline still wins, and its error is not relabelled as the
// driver's. (Passes on main by design: it guards the "no earlier one" rule.)
func TestOperationKeepsTheCallersShorterDeadline1218(t *testing.T) {
	for _, cl := range opClients {
		t.Run(cl.name, func(t *testing.T) {
			t.Parallel()
			addr, _ := startMuteServer(t, false)
			c := cl.new(t, Config{Addr: addr, Timeout: 30 * time.Second})
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			elapsed, err := opWithin(t, 2*time.Second, func() error {
				_, err := c.Get(ctx, "k")
				return err
			})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error %v, want one matching context.DeadlineExceeded (also on the direct path)", err)
			}
			if strings.Contains(err.Error(), "operation timed out") {
				t.Errorf("error %q: the caller's own deadline is reported as Config.Timeout", err)
			}
			if elapsed > time.Second {
				t.Errorf("returned after %v, want about the caller's 200 ms", elapsed)
			}
		})
	}
}

// A client that answers within the bound is not affected, and the bound does not
// stay on the conn: the conn is used again after the time it would have fired.
// (Passes on main by design.)
func TestTimeoutDoesNotTouchAnAnsweredOperation1218(t *testing.T) {
	f := startFake(t)
	for _, cl := range opClients {
		t.Run(cl.name, func(t *testing.T) {
			c := cl.new(t, Config{Addr: f.Addr(), Timeout: 200 * time.Millisecond})
			ctx := context.Background()
			if err := c.Set(ctx, "k"+cl.name, "v", 0); err != nil {
				t.Fatal(err)
			}
			time.Sleep(300 * time.Millisecond) // past Timeout
			if v, err := c.Get(ctx, "k"+cl.name); err != nil || v != "v" {
				t.Fatalf("Get after Timeout passed = %q, %v", v, err)
			}
		})
	}
}

// A client whose Timeout is unset gets the default (3 s). The test lowers the
// default through the test hook, so it does not wait 3 s.
func TestOperationIsBoundedByTheDefaultTimeout1218(t *testing.T) {
	const def = 300 * time.Millisecond
	d := def
	testHookDefaultOpTimeout.Store(&d)
	t.Cleanup(func() { testHookDefaultOpTimeout.Store(nil) })
	for _, proto := range []Protocol{ProtocolText, ProtocolBinary} {
		for _, cl := range opClients {
			t.Run(proto.String()+"/"+cl.name, func(t *testing.T) {
				t.Parallel()
				addr, _ := startMuteServer(t, proto == ProtocolBinary)
				c := cl.new(t, Config{Addr: addr, Protocol: proto}) // no Timeout
				elapsed, err := opWithin(t, 2*time.Second, func() error {
					ctx := context.Background()
					_, err := c.Get(ctx, "k")
					return err
				})
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "after "+def.String()) {
					t.Errorf("error %v, want the operation timeout of the default (%v)", err, def)
				}
				if elapsed < def/2 {
					t.Errorf("returned after %v, before the default of %v", elapsed, def)
				}
			})
		}
	}
}

// A negative Timeout turns the per-operation deadline off: only the caller's
// context ends the operation (here a deadline of 900 ms, three times the lowered
// default), and its error is not relabelled.
func TestNegativeTimeoutTurnsTheOperationBoundOff1218(t *testing.T) {
	d := 300 * time.Millisecond
	testHookDefaultOpTimeout.Store(&d)
	t.Cleanup(func() { testHookDefaultOpTimeout.Store(nil) })
	for _, cl := range opClients {
		t.Run(cl.name, func(t *testing.T) {
			t.Parallel()
			addr, _ := startMuteServer(t, false)
			c := cl.new(t, Config{Addr: addr, Timeout: -1})
			ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
			defer cancel()
			elapsed, err := opWithin(t, 3*time.Second, func() error {
				_, err := c.Get(ctx, "k")
				return err
			})
			if err == nil || strings.Contains(err.Error(), "operation timed out") || elapsed < 800*time.Millisecond {
				t.Errorf("returned after %v with %v: the bound was not off", elapsed, err)
			}
		})
	}
}

// A caller that cancels its context gets its own cancellation back, not relabelled
// as the driver's timeout, and the conn is dropped. This includes the direct path,
// the one a client without an engine uses: its blocked read is woken by the cancel,
// it does not wait for Config.Timeout, and the error matches context.Canceled, not a
// bare i/o timeout.
func TestOperationKeepsTheCallersCancellation1218(t *testing.T) {
	if defaultTimeout != 3*time.Second {
		t.Errorf("defaultTimeout = %v, want the documented 3 s", defaultTimeout)
	}
	for _, cl := range opClients {
		t.Run(cl.name, func(t *testing.T) {
			t.Parallel()
			addr, _ := startMuteServer(t, false)
			c := cl.new(t, Config{Addr: addr, Timeout: 30 * time.Second})
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(200*time.Millisecond, cancel)
			elapsed, err := opWithin(t, 2*time.Second, func() error {
				_, err := c.Get(ctx, "k")
				return err
			})
			if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "operation timed out") {
				t.Errorf("error %v, want the caller's context.Canceled as it is", err)
			}
			if elapsed > time.Second {
				t.Errorf("returned after %v, want about the cancel at 200 ms", elapsed)
			}
			if n := c.PoolStats().Open; n != 0 {
				t.Errorf("the pool holds %d conns after the cancel, want 0", n)
			}
		})
	}
}

// opError never relabels an application answer, whatever the time: only a deadline
// error (the socket's, on a direct conn) is turned into the per-operation timeout,
// and a caller's ended context wins over it.
func TestOpErrorRelabelsOnlyDeadlineErrors1218(t *testing.T) {
	c := &memcachedConn{opTimeout: 300 * time.Millisecond}
	if err := c.opError(context.Background(), ErrCacheMiss); err != ErrCacheMiss {
		t.Errorf("an application answer was relabelled: %v", err)
	}
	sock := os.ErrDeadlineExceeded
	err := c.opError(context.Background(), sock)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, os.ErrDeadlineExceeded) || !strings.Contains(err.Error(), "operation timed out after 300ms") {
		t.Errorf("the socket's deadline error became %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = c.opError(ctx, sock)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "operation timed out") {
		t.Errorf("a caller's cancel became %v", err)
	}
	// No limit on the command: the socket's error is left alone while the caller's
	// deadline has not been reached, and is the caller's once it has.
	c.opTimeout = 0
	if err := c.opError(context.Background(), sock); err != sock {
		t.Errorf("an error of an unlimited command was changed: %v", err)
	}
	dctx, dcancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer dcancel()
	if err := c.opError(dctx, sock); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a caller's deadline became %v", err)
	}
}

// The pool's health check pings an idle conn with conn.Ping, outside any Client
// command, after the command that last used it. The limit of that command must have
// been cleared: a stale deadline in the past would fail every ping at once, and the
// check would close every idle conn.
func TestHealthCheckPingAfterALimitedCommand1218(t *testing.T) {
	f := startFake(t)
	for _, cl := range opClients {
		t.Run(cl.name, func(t *testing.T) {
			c := cl.new(t, Config{Addr: f.Addr(), Timeout: 40 * time.Millisecond})
			ctx := context.Background()
			if err := c.Set(ctx, "k"+cl.name, "v", 0); err != nil {
				t.Fatal(err)
			}
			time.Sleep(80 * time.Millisecond) // past that command's limit
			conn, err := c.pool.acquire(ctx, -1)
			if err != nil {
				t.Fatal(err)
			}
			defer c.pool.release(conn)
			for i := 0; i < 5; i++ {
				if err := conn.Ping(ctx); err != nil {
					t.Fatalf("health-style Ping %d after a limited command: %v", i, err)
				}
			}
		})
	}
}

// startSlowFirstServer answers the first command of each conn after a delay (the
// caller parks for it) and none after it (it stalls): the second command on a
// reused conn is the one the limit must end.
func startSlowFirstServer(t *testing.T, delay time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go func() {
				r := bufio.NewReader(c)
				if _, err := r.ReadString('\n'); err != nil {
					return
				}
				time.Sleep(delay)
				if _, err := c.Write([]byte("VERSION 1.6.0\r\n")); err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, r) // the next command is never answered
			}()
		}
	}()
	return ln.Addr().String()
}

// The limit is armed again on a conn that is used again: the first command parks and
// is answered, the second on the same conn stalls and must end at about Timeout, not
// run unbounded. (Every other bounded test uses a fresh conn, whose first park builds
// the timer; a loop conn that does not re-arm its timer on reuse leaves every command
// after the first unbounded.)
func TestLimitIsArmedAgainOnAReusedConn1218(t *testing.T) {
	const bound = 150 * time.Millisecond
	for _, cl := range opClients {
		t.Run(cl.name, func(t *testing.T) {
			t.Parallel()
			addr := startSlowFirstServer(t, 40*time.Millisecond)
			c := cl.new(t, Config{Addr: addr, Timeout: bound})
			ctx := context.Background()
			if _, err := c.Version(ctx); err != nil {
				t.Fatalf("the first command: %v", err)
			}
			elapsed, err := opWithin(t, 2*time.Second, func() error {
				_, err := c.Version(ctx)
				return err
			})
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out") {
				t.Errorf("second command error %v, want the operation timeout", err)
			}
			if elapsed < bound/2 {
				t.Errorf("second command returned after %v, before the bound of %v", elapsed, bound)
			}
		})
	}
}

// The wake callback of a command that ended as its context was cancelled can still
// be in flight when the next command starts on the same direct conn; it must not
// give that command, whose context is alive, a deadline of "now" (a false "operation
// timed out" and a lost healthy conn). The hook holds the callback of the first
// command for 100 ms so that it lands after the second one has armed its own wake.
func TestLateWakeDoesNotFailTheNextCommand1218(t *testing.T) {
	d := 100 * time.Millisecond
	testHookWakeDelay.Store(&d)
	t.Cleanup(func() { testHookWakeDelay.Store(nil) })
	for _, proto := range []struct {
		name  string
		p     Protocol
		start func(t *testing.T) string
	}{
		{"text", ProtocolText, func(t *testing.T) string { return startFake(t).Addr() }},
		{"binary", ProtocolBinary, func(t *testing.T) string { return startFakeBinary(t).Addr() }},
	} {
		t.Run(proto.name, func(t *testing.T) {
			c := opClients[0].new(t, Config{Addr: proto.start(t), Protocol: proto.p, Timeout: 30 * time.Second})
			conn, err := c.pool.acquire(context.Background(), -1)
			if err != nil {
				t.Fatal(err)
			}
			defer c.pool.release(conn)
			// First command: its context ends as it finishes, the stop of its wake
			// fails (the callback has started) and the callback is held by the hook.
			ctx1, cancel1 := context.WithCancel(context.Background())
			conn.armWake(ctx1)
			cancel1()
			conn.disarmWake()
			// Second command, on a live context.
			ctx2, cancel2 := context.WithCancel(context.Background())
			defer cancel2()
			conn.armWake(ctx2)
			defer conn.disarmWake()
			time.Sleep(3 * d) // the held callback of the first command runs now, if it still can
			if err := conn.Ping(ctx2); err != nil {
				t.Fatalf("a live command on the conn after a cancelled one: %v", err)
			}
		})
	}
}

// A cancellation reaches a command on a conn that has already served one: the wake
// is armed again for every command, so the cancel ends it at once instead of at
// Config.Timeout. (Every other cancellation test uses a fresh conn; one whose wake
// state is not reset per command has it spent by the first command's disarm, and
// every cancel after it waits for the limit. For the binary protocol the handshake
// is that first use.) The first command runs under a cancellable context, since a
// context that cannot be cancelled arms no wake.
func TestCancelOnAReusedDirectConn1218(t *testing.T) {
	for _, proto := range []struct {
		name  string
		p     Protocol
		start func(t *testing.T) string
		first func(ctx context.Context, c *Client) error
	}{
		{"text", ProtocolText, func(t *testing.T) string { return startSlowFirstServer(t, 10*time.Millisecond) },
			func(ctx context.Context, c *Client) error { _, err := c.Version(ctx); return err }},
		{"binary", ProtocolBinary, func(t *testing.T) string { a, _ := startMuteServer(t, true); return a },
			func(ctx context.Context, c *Client) error { return nil }}, // the handshake was the first use
	} {
		t.Run(proto.name, func(t *testing.T) {
			t.Parallel()
			c := opClients[0].new(t, Config{Addr: proto.start(t), Protocol: proto.p, Timeout: 30 * time.Second})
			ctx1, cancel1 := context.WithCancel(context.Background())
			defer cancel1()
			if err := proto.first(ctx1, c); err != nil {
				t.Fatalf("the first command: %v", err)
			}
			ctx2, cancel2 := context.WithCancel(context.Background())
			time.AfterFunc(150*time.Millisecond, cancel2)
			elapsed, err := opWithin(t, 3*time.Second, func() error {
				_, err := c.Get(ctx2, "k")
				return err
			})
			if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "operation timed out") {
				t.Errorf("error %v, want the caller's context.Canceled", err)
			}
			if elapsed > time.Second {
				t.Errorf("returned after %v, want about the cancel at 150 ms and not the 30 s Timeout", elapsed)
			}
		})
	}
}
