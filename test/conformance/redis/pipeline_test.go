//go:build redis

package redis_test

import (
	"fmt"
	"testing"
	"time"
)

func TestPipeline_Basic(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "pipe")
	cleanupKeys(t, c, key)

	p := c.Pipeline()
	set := p.Set(key, "v", 0)
	get := p.Get(key)
	if err := p.Exec(ctx); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if _, err := set.Result(); err != nil {
		t.Fatalf("set.Result: %v", err)
	}
	s, err := get.Result()
	if err != nil {
		t.Fatalf("get.Result: %v", err)
	}
	if s != "v" {
		t.Fatalf("get = %q, want v", s)
	}
}

func TestPipeline_LargeBatch(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 10*time.Second)
	keys := make([]string, 150)
	for i := range keys {
		keys[i] = uniqueKey(t, fmt.Sprintf("bulk_%d", i))
	}
	cleanupKeys(t, c, keys...)

	p := c.Pipeline()
	for i, k := range keys {
		p.Set(k, fmt.Sprintf("v%d", i), 0)
	}
	if err := p.Exec(ctx); err != nil {
		t.Fatalf("Exec batch: %v", err)
	}

	// Second pipeline reads everything back.
	p2 := c.Pipeline()
	gets := make([]interface {
		Result() (string, error)
	}, len(keys))
	for i, k := range keys {
		gets[i] = p2.Get(k)
	}
	if err := p2.Exec(ctx); err != nil {
		t.Fatalf("Exec reads: %v", err)
	}
	for i, g := range gets {
		s, err := g.Result()
		if err != nil {
			t.Fatalf("gets[%d].Result: %v", i, err)
		}
		want := fmt.Sprintf("v%d", i)
		if s != want {
			t.Fatalf("gets[%d] = %q, want %q", i, s, want)
		}
	}
}

func TestPipeline_ErrorMidBatch(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	good := uniqueKey(t, "good")
	list := uniqueKey(t, "list")
	cleanupKeys(t, c, good, list)

	// Pre-create a list so Incr against it will WRONGTYPE.
	if _, err := c.RPush(ctx, list, "a"); err != nil {
		t.Fatalf("RPush setup: %v", err)
	}

	p := c.Pipeline()
	ok := p.Set(good, "v", 0)
	bad := p.Incr(list) // WRONGTYPE
	after := p.Set(good, "v2", 0)
	err := p.Exec(ctx)
	// Exec reports first per-command error; that's expected when a pipelined
	// cmd fails. The good commands still executed.
	if err == nil {
		t.Fatalf("Exec = nil, expected first-command error for WRONGTYPE")
	}

	if _, e := ok.Result(); e != nil {
		t.Fatalf("ok.Result: %v", e)
	}
	if _, e := bad.Result(); e == nil {
		t.Fatalf("bad.Result = nil, want WRONGTYPE")
	}
	if _, e := after.Result(); e != nil {
		t.Fatalf("after.Result: %v", e)
	}

	// Verify "after" actually happened despite the mid-batch error (pipelines
	// in Redis don't short-circuit on the server side).
	got, err := c.Get(ctx, good)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "v2" {
		t.Fatalf("Get = %q, want v2 (post-error cmd should have executed)", got)
	}
}

func TestPipeline_Discard(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "discard")
	cleanupKeys(t, c, key)

	p := c.Pipeline()
	p.Set(key, "shouldnot", 0)
	p.Discard()
	// Exec on empty pipeline should be a no-op.
	if err := p.Exec(ctx); err != nil {
		t.Fatalf("Exec after Discard: %v", err)
	}
	// The key must not exist.
	n, _ := c.Exists(ctx, key)
	if n != 0 {
		t.Fatalf("Exists after Discard = %d, want 0", n)
	}
}
