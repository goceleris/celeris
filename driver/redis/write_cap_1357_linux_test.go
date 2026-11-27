//go:build linux

package redis

// celeris#1357: a Redis SET of 4 MiB or more failed on Linux with "worker
// write queue full" when the client ran on the standalone driver loop (no
// WithEngine), whose per-conn cap was 4 MiB. The engine-hosted paths take
// 64 MiB. Above 64 MiB the write fails on every path, and the error names
// the limit.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

func TestSetOf8MiBOnTheStandaloneLoop1357(t *testing.T) {
	m := newMem()
	srv := startFakeRedis(t, m.handler)
	c, err := NewClient(srv.Addr(), WithDialTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	val := strings.Repeat("v", 8<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Set(ctx, "big", val, 0); err != nil {
		t.Fatalf("SET of an 8 MiB value: %v", err)
	}
	got, err := c.Get(ctx, "big")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if got != val {
		t.Fatalf("GET returned %d bytes, want %d", len(got), len(val))
	}
}

func TestSetAbove64MiBNamesTheLimit1357(t *testing.T) {
	m := newMem()
	srv := startFakeRedis(t, m.handler)
	c, err := NewClient(srv.Addr(), WithDialTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	val := strings.Repeat("v", 65<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = c.Set(ctx, "huge", val, 0)
	if err == nil {
		t.Fatal("SET of a 65 MiB value succeeded; the limit is 64 MiB on every Linux path")
	}
	if !errors.Is(err, ErrQueueFull) || !errors.Is(err, engine.ErrQueueFull) {
		t.Fatalf("SET of 65 MiB: err = %v, want a match for ErrQueueFull", err)
	}
	if !strings.Contains(err.Error(), "exceeds the per-write limit of 64 MiB") {
		t.Fatalf("SET of 65 MiB: %q does not name the limit", err)
	}
}
