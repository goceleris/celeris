package postgres

// celeris#1212: ConnectTimeout is documented as bounding "dial + startup", but it
// was only given to the dialer. doStartup ran on the caller's context, so a
// server that accepts the TCP connection and never completes the startup held
// Open / acquire until the caller's context ended: for ever under
// context.Background(), and in direct mode even a caller's deadline was not
// honoured while the read was blocked.

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// stalledPG is a server that accepts TCP and does not complete the startup. It
// reports on closed when a client socket closes.
type stalledPG struct {
	addr   string
	closed chan struct{}
}

// startStalledPG starts a server that reads the StartupMessage and then, depending
// on stage, answers nothing ("silent") or everything but ReadyForQuery
// ("no-ready-for-query": AuthenticationOk, ParameterStatus, BackendKeyData). It
// holds the conn until the client closes it or the test ends.
func startStalledPG(t *testing.T, stage string) *stalledPG {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &stalledPG{addr: ln.Addr().String(), closed: make(chan struct{}, 16)}
	var mu sync.Mutex
	var conns []net.Conn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					_ = c.Close()
					select {
					case s.closed <- struct{}{}:
					default:
					}
				}()
				if stage != "silent" {
					readStartup(t, c)
					_ = writeAuthOK(c)
					_ = writeMsg(c, protocol.BackendParameterStatus, []byte("server_version\x0016.0\x00"))
					_ = writeBackendKeyData(c, 1, 2)
				}
				_, _ = io.Copy(io.Discard, c) // until the client or the test closes it
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return s
}

func (s *stalledPG) dsn(connectTimeout time.Duration) DSN {
	host, port, _ := net.SplitHostPort(s.addr)
	return DSN{Host: host, Port: port, User: "u", Database: "d",
		Options: Options{SSLMode: "disable", StatementCacheSize: 4, ConnectTimeout: connectTimeout},
		Params:  map[string]string{}}
}

// startupDialModes are the three ways a conn is dialled: direct (standalone, or an
// engine that runs handlers on unlocked Gs), the unix event loop, and the
// net.Conn loop of platforms without descriptors.
var startupDialModes = []struct {
	name string
	dial func(t *testing.T, ctx context.Context, dsn DSN) (*pgConn, error)
}{
	{"direct", func(t *testing.T, ctx context.Context, dsn DSN) (*pgConn, error) {
		return dialDirectConn(ctx, dsn)
	}},
	{"eventloop", func(t *testing.T, ctx context.Context, dsn DSN) (*pgConn, error) {
		prov, err := eventloop.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { eventloop.Release(prov) })
		return dialConn(ctx, prov, nil, dsn, 0)
	}},
	{"netconn", func(t *testing.T, ctx context.Context, dsn DSN) (*pgConn, error) {
		return dialConnNetConn(ctx, netConnProvider(t), nil, dsn, 1)
	}},
}

// dialWithin runs the dial and fails the test (instead of waiting for the test
// binary's timeout) if it is still blocked after limit.
func dialWithin(t *testing.T, limit time.Duration, dial func() (*pgConn, error)) (time.Duration, error) {
	t.Helper()
	type res struct {
		c   *pgConn
		err error
	}
	ch := make(chan res, 1)
	start := time.Now()
	go func() {
		c, err := dial()
		ch <- res{c, err}
	}()
	select {
	case r := <-ch:
		if r.c != nil {
			_ = r.c.Close()
			t.Fatal("the dial succeeded against a server that never completes the startup")
		}
		return time.Since(start), r.err
	case <-time.After(limit):
		t.Fatalf("the dial is still blocked after %v: the startup is not bounded", limit)
		return 0, nil
	}
}

// A server that accepts TCP and never completes the startup: the dial returns an
// error after about ConnectTimeout, with a context that has no deadline, closes
// its socket, and says what timed out. Whether the server stays silent or has sent
// everything but ReadyForQuery, the bound is on the whole startup.
func TestStartupIsBoundedByConnectTimeout1212(t *testing.T) {
	for _, stage := range []string{"silent", "no-ready-for-query"} {
		for _, m := range startupDialModes {
			t.Run(stage+"/"+m.name, func(t *testing.T) {
				t.Parallel()
				srv := startStalledPG(t, stage)
				const bound = 200 * time.Millisecond
				elapsed, err := dialWithin(t, 2*time.Second, func() (*pgConn, error) {
					return m.dial(t, context.Background(), srv.dsn(bound))
				})
				if err == nil {
					t.Fatal("no error")
				}
				t.Logf("%s: %v after %v", stage+"/"+m.name, err, elapsed.Round(10*time.Millisecond))
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("error %v does not match context.DeadlineExceeded", err)
				}
				if !strings.Contains(err.Error(), "startup") {
					t.Errorf("error %q does not say that the startup timed out", err)
				}
				if elapsed < bound/2 {
					t.Errorf("returned after %v, before the bound of %v", elapsed, bound)
				}
				select {
				case <-srv.closed:
				case <-time.After(2 * time.Second):
					t.Error("the client did not close its socket after the startup timed out")
				}
			})
		}
	}
}

// The caller's own context still ends the startup, at once: a deadline shorter
// than ConnectTimeout wins, and a context cancelled while the server is silent
// ends the dial when ConnectTimeout is not set (zero: no client-side timeout). The
// caller's error comes back as it is.
func TestStartupEndsWithTheCallersContext1212(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		ctx     func() (context.Context, context.CancelFunc)
		want    error
	}{
		{"shorter-deadline", 30 * time.Second, func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 200*time.Millisecond)
		}, context.DeadlineExceeded},
		{"cancel-without-connect-timeout", 0, func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(200*time.Millisecond, cancel)
			return ctx, cancel
		}, context.Canceled},
	} {
		for _, m := range startupDialModes {
			t.Run(tc.name+"/"+m.name, func(t *testing.T) {
				t.Parallel()
				srv := startStalledPG(t, "silent")
				ctx, cancel := tc.ctx()
				defer cancel()
				elapsed, err := dialWithin(t, 2*time.Second, func() (*pgConn, error) {
					return m.dial(t, ctx, srv.dsn(tc.timeout))
				})
				t.Logf("%s/%s: %v after %v", tc.name, m.name, err, elapsed.Round(10*time.Millisecond))
				if !errors.Is(err, tc.want) {
					t.Errorf("error %v does not match %v", err, tc.want)
				}
				if strings.Contains(err.Error(), "startup timed out") {
					t.Errorf("error %q blames ConnectTimeout, but the caller's context ended the dial", err)
				}
				select {
				case <-srv.closed:
				case <-time.After(2 * time.Second):
					t.Error("the client did not close its socket")
				}
			})
		}
	}
}

// A startup that completes inside ConnectTimeout is not touched by the bound: the
// conn works afterwards, and stays usable past the moment the bound would have
// fired (a timer left armed would fail it, or poison the socket).
func TestStartupBoundDoesNotOutliveASuccessfulStartup1212(t *testing.T) {
	for _, m := range startupDialModes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			addr := startFakePG(t, func(c net.Conn) {
				fakePGTrustStartup(t, c, 42, 7, func(c net.Conn) {
					for {
						typ, _, err := readMsg(c)
						if err != nil || typ == protocol.MsgTerminate {
							return
						}
						if typ == protocol.MsgQuery {
							_ = writeCommandComplete(c, "SELECT 1")
							_ = writeReadyForQuery(c, 'I')
						}
					}
				})
			})
			host, port, _ := net.SplitHostPort(addr)
			dsn := DSN{Host: host, Port: port, User: "u", Database: "d",
				Options: Options{SSLMode: "disable", StatementCacheSize: 4, ConnectTimeout: 150 * time.Millisecond},
				Params:  map[string]string{}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := m.dial(t, ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if err := c.Ping(ctx); err != nil {
				t.Fatalf("Ping right after the startup: %v", err)
			}
			time.Sleep(300 * time.Millisecond) // past ConnectTimeout
			if err := c.Ping(ctx); err != nil {
				t.Fatalf("Ping after ConnectTimeout passed: %v", err)
			}
		})
	}
}

// The pool's acquire path: Open with connect_timeout=1 against a server that never
// completes the startup. Ping, with a context without a deadline, fails after about
// a second, and the pool is left with no open conn.
func TestPoolAcquireIsBoundedByConnectTimeout1212(t *testing.T) {
	srv := startStalledPG(t, "silent")
	host, port, _ := net.SplitHostPort(srv.addr)
	p, err := Open("postgres://u@" + host + ":" + port + "/d?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- p.Ping(context.Background()) }()
	select {
	case err := <-done:
		t.Logf("Ping against a stalled server: %v after %v", err, time.Since(start).Round(10*time.Millisecond))
		if err == nil {
			t.Fatal("Ping succeeded")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v does not match context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ping is still blocked after 2s: connect_timeout=1 does not bound the startup")
	}
	if n := p.Stats().Open; n != 0 {
		t.Errorf("the pool holds %d conns after the failed dial, want 0", n)
	}
}
