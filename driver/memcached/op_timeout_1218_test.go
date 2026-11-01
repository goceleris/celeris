package memcached

// celeris#1218: Config.Timeout, documented as the per-operation default (3 s), was
// never applied. A command on a server that accepts TCP and never answers it held
// the caller until its context ended: for ever under context.Background().

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
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
			if !errors.Is(err, context.DeadlineExceeded) && !isTimeout(err) {
				t.Fatalf("error %v, want a deadline error", err)
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

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
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
// as the driver's timeout, and the conn is dropped. (A direct conn does not watch a
// context's cancellation, only its deadline, so it is not in the cases.)
func TestOperationKeepsTheCallersCancellation1218(t *testing.T) {
	if defaultTimeout != 3*time.Second {
		t.Errorf("defaultTimeout = %v, want the documented 3 s", defaultTimeout)
	}
	for _, cl := range opClients {
		if cl.name == "direct" {
			continue
		}
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
