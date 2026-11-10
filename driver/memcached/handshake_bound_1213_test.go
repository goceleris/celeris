package memcached

// celeris#1213: DialTimeout (documented as the TCP dial timeout) bounded only
// net.Dialer.DialContext. The binary-protocol handshake (a VERSION round trip
// that confirms the server speaks binary) ran on the caller's context, so a
// server that accepts TCP and never answers it held the dial, and every command
// that needed a new conn, until the caller's context ended: for ever under
// context.Background. Same class as celeris#1205 (redis), found by the review
// of PR #1209.

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

// startSilentServer accepts TCP conns, reads what the client sends and never
// answers. It counts the conns the client has closed (the server read their end of
// stream).
func startSilentServer(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	closed := new(atomic.Int32)
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
				_, _ = io.Copy(io.Discard, c)
				closed.Add(1)
			}()
		}
	}()
	return ln.Addr().String(), closed
}

// within runs call with a context that has no deadline (the case that hung for
// ever) and fails the test if call is still running after limit, so the failure
// is a message and not a test timeout.
func within(t *testing.T, limit time.Duration, what string, call func(ctx context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- call(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("%s succeeded against a server that never answers the handshake", what)
		}
		if d := time.Since(start); d > limit {
			t.Fatalf("%s returned after %v, want within %v", what, d, limit)
		}
		return err
	case <-time.After(limit):
		// Not waited for: a direct-mode conn does not watch the context's
		// cancellation, only its deadline, so the call may stay blocked until
		// the server's cleanup closes its conns.
		cancel()
		t.Fatalf("%s still blocked after %v: the handshake is not bounded by DialTimeout", what, limit)
		return nil
	}
}

// wantTimeout fails the test unless err is the driver's handshake bound: an
// error that matches context.DeadlineExceeded and names the handshake, once.
func wantTimeout(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want one matching context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "handshake timed out") {
		t.Fatalf("err = %v, want it to say the handshake timed out", err)
	}
	if strings.Count(err.Error(), "context deadline exceeded") > 1 {
		t.Fatalf("err = %q repeats the context error", err)
	}
}

// dialPaths are the three dials, as the tests call them: direct, the unix event
// loop (the net.Conn loop on platforms without descriptors), and the net.Conn
// loop itself. Each closes the conn it got.
func dialPaths(t *testing.T) []struct {
	name string
	dial func(ctx context.Context, cfg Config) error
} {
	t.Helper()
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eventloop.Release(prov) })
	portable := netConnProvider(t)
	closeIt := func(c *memcachedConn, err error) error {
		if c != nil {
			_ = c.Close()
		}
		return err
	}
	return []struct {
		name string
		dial func(ctx context.Context, cfg Config) error
	}{
		{"direct", func(ctx context.Context, cfg Config) error {
			return closeIt(dialDirectMemcachedConn(ctx, cfg))
		}},
		{"eventloop", func(ctx context.Context, cfg Config) error {
			return closeIt(dialMemcachedConn(ctx, prov, cfg, -1))
		}},
		{"netconn", func(ctx context.Context, cfg Config) error {
			var p engine.EventLoopProvider = portable
			return closeIt(dialMemcachedConnNetConn(ctx, p, cfg, -1))
		}},
	}
}

// The three dial paths, called directly: each must give up on a server that
// never answers the binary handshake within about DialTimeout.
func TestBinaryHandshakeIsBoundedByDialTimeout1213(t *testing.T) {
	addr, _ := startSilentServer(t)
	cfg := Config{Addr: addr, Protocol: ProtocolBinary, DialTimeout: 500 * time.Millisecond}
	for _, p := range dialPaths(t) {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			err := within(t, 2*time.Second, p.name+" dial", func(ctx context.Context) error { return p.dial(ctx, cfg) })
			t.Logf("%s: %v after %v", p.name, err, time.Since(start).Round(10*time.Millisecond))
			wantTimeout(t, err)
			if el := time.Since(start); el < cfg.DialTimeout/2 {
				t.Fatalf("returned after %v, before the %v bound", el, cfg.DialTimeout)
			}
		})
	}
}

// A caller's own, shorter deadline still wins, and is not reported as the
// driver's.
func TestBinaryHandshakeKeepsTheCallersShorterDeadline1213(t *testing.T) {
	addr, _ := startSilentServer(t)
	cfg := Config{Addr: addr, Protocol: ProtocolBinary, DialTimeout: 30 * time.Second}
	for _, p := range dialPaths(t) {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			start := time.Now()
			done := make(chan error, 1)
			go func() { done <- p.dial(ctx, cfg) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("still blocked after 2s: the caller's own deadline did not end the handshake")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want one matching context.DeadlineExceeded (also on the direct path)", err)
			}
			if strings.Contains(err.Error(), "handshake timed out") {
				t.Fatalf("err = %v: the caller's own deadline is reported as the driver's DialTimeout", err)
			}
			if d := time.Since(start); d > time.Second {
				t.Fatalf("returned after %v, want about the caller's 200ms", d)
			}
		})
	}
}

// The public entry point: a command that needs a new conn (the pool dials it
// lazily), with a context that has no deadline.
func TestClientBoundsTheBinaryHandshake1213(t *testing.T) {
	addr, _ := startSilentServer(t)
	c, err := NewClient(addr, WithProtocol(ProtocolBinary), WithDialTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	err = within(t, 2*time.Second, "Client.Version", func(ctx context.Context) error {
		_, err := c.Version(ctx)
		return err
	})
	wantTimeout(t, err)
}

// A caller that cancels its context during the handshake gets its own
// cancellation back, not relabelled as the driver's bound, and the half-open conn
// is closed (the server sees the end of stream). This includes the direct path,
// the one the pool uses: it ends at the cancel, not at DialTimeout, and the error
// matches context.Canceled, not a bare i/o timeout.
func TestBinaryHandshakeKeepsTheCallersCancellation1213(t *testing.T) {
	for _, p := range dialPaths(t) {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			addr, closed := startSilentServer(t)
			cfg := Config{Addr: addr, Protocol: ProtocolBinary, DialTimeout: 30 * time.Second}
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(200*time.Millisecond, cancel)
			start := time.Now()
			done := make(chan error, 1)
			go func() { done <- p.dial(ctx, cfg) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("still blocked 2s after the cancel: the caller's cancellation did not end the handshake")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if strings.Contains(err.Error(), "handshake timed out") {
				t.Fatalf("err = %v: the caller's cancellation is reported as the driver's bound", err)
			}
			if el := time.Since(start); el > time.Second {
				t.Fatalf("returned after %v, want about the cancel at 200ms", el)
			}
			deadline := time.Now().Add(2 * time.Second)
			for closed.Load() == 0 {
				if time.Now().After(deadline) {
					t.Fatal("the server never saw the client's socket close")
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

// What must not change: the text protocol has no handshake, so a dial against a
// silent server succeeds at once (a bound on a handshake that does not exist
// would fail it); and a binary server that answers is dialled and used as before,
// with DialTimeout set.
func TestBoundDoesNotTouchTextOrAnAnsweredHandshake1213(t *testing.T) {
	t.Run("text-silent-server", func(t *testing.T) {
		addr, _ := startSilentServer(t)
		cfg := Config{Addr: addr, Protocol: ProtocolText, DialTimeout: 300 * time.Millisecond}
		for _, p := range dialPaths(t) {
			if err := within2(t, p.name, func(ctx context.Context) error { return p.dial(ctx, cfg) }); err != nil {
				t.Errorf("%s: dial of the text protocol against a silent server: %v", p.name, err)
			}
		}
	})
	t.Run("binary-answered", func(t *testing.T) {
		f := startFakeBinary(t)
		cfg := Config{Addr: f.Addr(), Protocol: ProtocolBinary, DialTimeout: 300 * time.Millisecond}
		for _, p := range dialPaths(t) {
			if err := within2(t, p.name, func(ctx context.Context) error { return p.dial(ctx, cfg) }); err != nil {
				t.Errorf("%s: dial against a server that answers: %v", p.name, err)
			}
		}
		c, err := NewClient(f.Addr(), WithProtocol(ProtocolBinary), WithDialTimeout(300*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if _, err := c.Version(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond) // past DialTimeout: the conn is still good
		if _, err := c.Version(context.Background()); err != nil {
			t.Fatalf("Version after DialTimeout passed: %v", err)
		}
	})
}

// within2 runs call and returns its error; a dial that does not return in 2 s
// fails the test. nil means the dial succeeded (unlike within).
func within2(t *testing.T, what string, call func(ctx context.Context) error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call(context.Background()) }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: still blocked after 2s", what)
		return nil
	}
}
