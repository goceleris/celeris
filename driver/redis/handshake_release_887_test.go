package redis

// The contract behind the handshake fix, deterministic and on every OS (the
// stress test next to it, handshake_ctx_ended_887_unix_test.go, is the -race
// guard on linux and darwin): handshake gives its request back to the pool only
// when the server's reply was dispatched. A handshake that ends because the
// caller's context ended leaves the request alone, since the reader goroutine
// may still be filling it in.

import (
	"bufio"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHandshakeGivesBackNothingWhenTheContextEnds887(t *testing.T) {
	srv := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		// HELLO is never answered.
	})
	var released atomic.Int64
	hook := func(*redisRequest) { released.Add(1) }
	testHookHandshakeRelease.Store(&hook)
	t.Cleanup(func() { testHookHandshakeRelease.Store(nil) })

	c, err := NewClient(srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := c.Do(ctx, "PING"); err == nil {
		t.Fatal("Do against a server that never answers HELLO returned no error")
	}
	if n := released.Load(); n != 0 {
		t.Errorf("a handshake that ended with the caller's context gave %d request(s) back to the pool while its reader could still hold them", n)
	}
}

// The other side: an error reply (a server that does not know HELLO) was
// dispatched, so its request is given back and the handshake falls back to
// the legacy login.
func TestHandshakeGivesBackTheRequestOfAnErrorReply887(t *testing.T) {
	srv := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			writeError(w, "ERR unknown command 'HELLO'")
		case "PING":
			writeSimple(w, "PONG")
		default:
			writeSimple(w, "OK")
		}
	})
	var released atomic.Int64
	hook := func(*redisRequest) { released.Add(1) }
	testHookHandshakeRelease.Store(&hook)
	t.Cleanup(func() { testHookHandshakeRelease.Store(nil) })

	c, err := NewClient(srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping after the HELLO fallback: %v", err)
	}
	if n := released.Load(); n != 1 {
		t.Errorf("the handshake gave %d request(s) back, want 1 (the error reply's)", n)
	}
}
