package redis

// celeris#1205: DialTimeout bounded only net.Dialer.DialContext. The handshake
// (HELLO, then AUTH/SELECT) ran on the caller's context, so a node that accepts
// TCP and never answers HELLO held ClusterClient.Subscribe, ClusterClient.SSubscribe,
// Client.Subscribe and every Do that needed a new conn until the caller's context
// ended: for ever under context.Background. Found by the review of PR #1154; the
// code is older.

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

// startSilentNode accepts TCP conns and never reads or answers them.
func startSilentNode(t *testing.T) string {
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
		}
	}()
	return ln.Addr().String()
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
			t.Fatalf("%s succeeded against a node that never answers HELLO", what)
		}
		if d := time.Since(start); d > limit {
			t.Fatalf("%s returned after %v, want within %v", what, d, limit)
		}
		return err
	case <-time.After(limit):
		// Not waited for: a direct-mode conn does not watch the context's
		// cancellation, only its deadline, so the call may stay blocked until
		// the node's cleanup closes its conns.
		cancel()
		t.Fatalf("%s still blocked after %v: the handshake is not bounded by DialTimeout", what, limit)
		return nil
	}
}

// The three dial paths, called directly: each must give up on a silent node
// within about DialTimeout.
func TestDialHandshakeIsBoundedByDialTimeout1205(t *testing.T) {
	addr := startSilentNode(t)
	cfg := Config{Addr: addr, DialTimeout: time.Second}

	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eventloop.Release(prov) })
	portable := netConnProvider(t)

	cases := []struct {
		name string
		dial func(ctx context.Context) error
	}{
		{"direct", func(ctx context.Context) error {
			c, err := dialDirectRedisConn(ctx, cfg)
			if c != nil {
				_ = c.Close()
			}
			return err
		}},
		{"eventloop", func(ctx context.Context) error {
			c, err := dialRedisConn(ctx, prov, cfg, -1)
			if c != nil {
				_ = c.Close()
			}
			return err
		}},
		{"netconn", func(ctx context.Context) error {
			var p engine.EventLoopProvider = portable
			c, err := dialRedisConnNetConn(ctx, p, cfg, -1)
			if c != nil {
				_ = c.Close()
			}
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := within(t, 2*time.Second, tc.name+" dial", tc.dial)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want one matching context.DeadlineExceeded (the handshake timed out)", err)
			}
		})
	}
}

// A caller's own, shorter deadline still wins, and is not reported as the
// driver's.
func TestDialHandshakeKeepsTheCallersShorterDeadline1205(t *testing.T) {
	addr := startSilentNode(t)
	cfg := Config{Addr: addr, DialTimeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	c, err := dialDirectRedisConn(ctx, cfg)
	if c != nil {
		_ = c.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if errors.Is(err, context.DeadlineExceeded) && strings.Contains(err.Error(), "handshake timed out") {
		t.Fatalf("err = %v: the caller's own deadline is reported as the driver's DialTimeout", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("returned after %v, want about the caller's 200ms", d)
	}
}

// The public entry points: Client.Subscribe, and a Do that needs a new conn.
func TestClientSubscribeAndDoBoundTheHandshake1205(t *testing.T) {
	addr := startSilentNode(t)
	c, err := NewClient(addr, WithDialTimeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	t.Run("Subscribe", func(t *testing.T) {
		t.Parallel()
		_ = within(t, 2*time.Second, "Client.Subscribe", func(ctx context.Context) error {
			ps, err := c.Subscribe(ctx, "ch")
			if ps != nil {
				_ = ps.Close()
			}
			return err
		})
	})
	t.Run("Do", func(t *testing.T) {
		t.Parallel()
		_ = within(t, 2*time.Second, "Client.Do", func(ctx context.Context) error {
			_, err := c.Do(ctx, "PING")
			return err
		})
	})
}

// The cluster constructors: the node's command conn is up, a new (pubsub) conn
// to it is accepted and never answered.
func TestClusterSubscribeBoundsTheHandshake1205(t *testing.T) {
	cl := newRPCluster(t, 1)
	cc, err := NewClusterClient(ClusterConfig{Addrs: cl.addrs(), DialTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	n := cl.nodes[0]
	n.mu.Lock()
	n.hang = true
	n.mu.Unlock()

	t.Run("Subscribe", func(t *testing.T) {
		t.Parallel()
		_ = within(t, 2*time.Second, "ClusterClient.Subscribe", func(ctx context.Context) error {
			ps, err := cc.Subscribe(ctx, "ch")
			if ps != nil {
				_ = ps.Close()
			}
			return err
		})
	})
	t.Run("SSubscribe", func(t *testing.T) {
		t.Parallel()
		_ = within(t, 2*time.Second, "ClusterClient.SSubscribe", func(ctx context.Context) error {
			ps, err := cc.SSubscribe(ctx, "ch")
			if ps != nil {
				_ = ps.Close()
			}
			return err
		})
	})
}
