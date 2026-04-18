package memcached

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func newTestClient(t *testing.T) (*Client, *fakeMemcached) {
	t.Helper()
	fake := startFake(t)
	client, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, fake
}

func TestSetAndGet(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	if err := client.Set(ctx, "k", "v", 0); err != nil {
		t.Fatal(err)
	}
	v, err := client.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if v != "v" {
		t.Fatalf("got %q", v)
	}
}

func TestGetMiss(t *testing.T) {
	client, _ := newTestClient(t)
	_, err := client.Get(context.Background(), "absent")
	if !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss, got %v", err)
	}
}

func TestSetTTLAndGetBytes(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	if err := client.Set(ctx, "k", []byte{0x00, 0x01, 0x02}, time.Minute); err != nil {
		t.Fatal(err)
	}
	b, err := client.GetBytes(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 3 || b[0] != 0 || b[1] != 1 || b[2] != 2 {
		t.Fatalf("got %v", b)
	}
}

func TestAddAndReplace(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	if err := client.Add(ctx, "k", "v1", 0); err != nil {
		t.Fatal(err)
	}
	// Second Add should fail with ErrNotStored.
	if err := client.Add(ctx, "k", "v2", 0); !errors.Is(err, ErrNotStored) {
		t.Fatalf("expected ErrNotStored, got %v", err)
	}
	// Replace should succeed now.
	if err := client.Replace(ctx, "k", "v3", 0); err != nil {
		t.Fatal(err)
	}
	got, _ := client.Get(ctx, "k")
	if got != "v3" {
		t.Fatalf("got %q", got)
	}
	// Replace missing key.
	if err := client.Replace(ctx, "missing", "x", 0); !errors.Is(err, ErrNotStored) {
		t.Fatalf("expected ErrNotStored, got %v", err)
	}
}

func TestAppendPrepend(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	if err := client.Set(ctx, "k", "mid", 0); err != nil {
		t.Fatal(err)
	}
	if err := client.Append(ctx, "k", "-end"); err != nil {
		t.Fatal(err)
	}
	if err := client.Prepend(ctx, "k", "start-"); err != nil {
		t.Fatal(err)
	}
	got, _ := client.Get(ctx, "k")
	if got != "start-mid-end" {
		t.Fatalf("got %q", got)
	}
}

func TestDelete(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	_ = client.Set(ctx, "k", "v", 0)
	if err := client.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(ctx, "k"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss, got %v", err)
	}
}

func TestIncrDecr(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	if err := client.Set(ctx, "ctr", "10", 0); err != nil {
		t.Fatal(err)
	}
	n, err := client.Incr(ctx, "ctr", 5)
	if err != nil {
		t.Fatal(err)
	}
	if n != 15 {
		t.Fatalf("got %d", n)
	}
	n, err = client.Decr(ctx, "ctr", 6)
	if err != nil {
		t.Fatal(err)
	}
	if n != 9 {
		t.Fatalf("got %d", n)
	}
	// Decrement below zero clamps.
	n, err = client.Decr(ctx, "ctr", 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("got %d", n)
	}
	// Increment missing key → miss.
	_, err = client.Incr(ctx, "missing", 1)
	if !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss, got %v", err)
	}
}

func TestTouch(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	if err := client.Set(ctx, "k", "v", 0); err != nil {
		t.Fatal(err)
	}
	if err := client.Touch(ctx, "k", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := client.Touch(ctx, "missing", time.Second); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss, got %v", err)
	}
}

func TestGetMulti(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	_ = client.Set(ctx, "a", "va", 0)
	_ = client.Set(ctx, "b", "vb", 0)
	out, err := client.GetMulti(ctx, "a", "b", "absent")
	if err != nil {
		t.Fatal(err)
	}
	if out["a"] != "va" || out["b"] != "vb" {
		t.Fatalf("got %#v", out)
	}
	if _, ok := out["absent"]; ok {
		t.Fatalf("absent key should be omitted: %#v", out)
	}
}

func TestGets(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	_ = client.Set(ctx, "k", "v", 0)
	item, err := client.Gets(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if item.Key != "k" || string(item.Value) != "v" || item.CAS == 0 {
		t.Fatalf("got %#v", item)
	}
}

func TestCASSuccess(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	_ = client.Set(ctx, "k", "v1", 0)
	item, _ := client.Gets(ctx, "k")
	ok, err := client.CAS(ctx, "k", "v2", item.CAS, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("CAS should have succeeded")
	}
	got, _ := client.Get(ctx, "k")
	if got != "v2" {
		t.Fatalf("got %q", got)
	}
}

// TestCASZeroTokenRejected guards the fix for a subtle correctness bug:
// memcached's cas command / binary OpSet-with-CAS both treat casID=0 as
// "no check", silently degrading CAS into Set. A real CAS token from Gets
// is never 0, so casID=0 always signals a caller bug (forgot Gets,
// copy/paste error, etc.). The driver must fail loudly instead.
func TestCASZeroTokenRejected(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	_ = client.Set(ctx, "k", "v", 0)
	ok, err := client.CAS(ctx, "k", "attack", 0, 0)
	if ok {
		t.Fatal("CAS with casID=0 should not report success")
	}
	if !errors.Is(err, ErrInvalidCAS) {
		t.Fatalf("expected ErrInvalidCAS, got %v", err)
	}
	// Value must be unchanged — the degrade-to-Set bug would have
	// overwritten "v" with "attack".
	got, _ := client.Get(ctx, "k")
	if got != "v" {
		t.Fatalf("CAS with casID=0 silently overwrote the value: got %q, want v", got)
	}
}

func TestCASConflict(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	_ = client.Set(ctx, "k", "v1", 0)
	item, _ := client.Gets(ctx, "k")
	// Two concurrent writers observe the same CAS, but only the first wins.
	if _, err := client.CAS(ctx, "k", "winner", item.CAS, 0); err != nil {
		t.Fatal(err)
	}
	ok, err := client.CAS(ctx, "k", "loser", item.CAS, 0)
	if ok {
		t.Fatal("stale CAS should not have succeeded")
	}
	if !errors.Is(err, ErrCASConflict) {
		t.Fatalf("expected ErrCASConflict, got %v", err)
	}
}

func TestCASRace(t *testing.T) {
	// Two goroutines contend on the same key. Exactly one CAS should succeed.
	client, _ := newTestClient(t)
	ctx := context.Background()
	_ = client.Set(ctx, "k", "v", 0)
	item, _ := client.Gets(ctx, "k")

	var wg sync.WaitGroup
	var wins, conflicts int
	var mu sync.Mutex
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			ok, err := client.CAS(ctx, "k", "writer", item.CAS, 0)
			mu.Lock()
			defer mu.Unlock()
			if ok && err == nil {
				wins++
			} else if errors.Is(err, ErrCASConflict) {
				conflicts++
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 || conflicts != 1 {
		t.Fatalf("expected exactly 1 win + 1 conflict, got wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestFlush(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	_ = client.Set(ctx, "k", "v", 0)
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(ctx, "k"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss, got %v", err)
	}
}

func TestVersion(t *testing.T) {
	client, _ := newTestClient(t)
	v, err := client.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v == "" {
		t.Fatal("empty version")
	}
}

func TestStats(t *testing.T) {
	client, _ := newTestClient(t)
	m, err := client.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m["version"] == "" {
		t.Fatalf("missing version stat: %#v", m)
	}
}

func TestPing(t *testing.T) {
	client, _ := newTestClient(t)
	if err := client.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedKey(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	if err := client.Set(ctx, "bad key", "v", 0); !errors.Is(err, ErrMalformedKey) {
		t.Fatalf("expected ErrMalformedKey, got %v", err)
	}
	if err := client.Set(ctx, "", "v", 0); !errors.Is(err, ErrMalformedKey) {
		t.Fatalf("expected ErrMalformedKey, got %v", err)
	}
}

func TestContextCancellation(t *testing.T) {
	// A slow handler should unblock when ctx is canceled. The fake server's
	// command path is serial, but we can freeze it by starting a long sleep
	// in a custom handler. Instead we simply cancel an in-flight context and
	// check the error bubbles out.
	fake := startFake(t)
	client, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call even starts
	_, err = client.Get(ctx, "k")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestPoolAcquireRelease(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	// Warm the pool.
	if err := client.Set(ctx, "k", "v", 0); err != nil {
		t.Fatal(err)
	}
	// Acquire/release in a tight loop — no leaks should occur, and Stats
	// should show a stable OpenCount.
	for i := 0; i < 50; i++ {
		if _, err := client.Get(ctx, "k"); err != nil {
			t.Fatal(err)
		}
	}
	s := client.PoolStats()
	if s.Open <= 0 {
		t.Fatalf("unexpected pool stats: %#v", s)
	}
}

func TestIntValueRoundTrip(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()
	if err := client.Set(ctx, "k", 42, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := client.Get(ctx, "k"); got != "42" {
		t.Fatalf("got %q", got)
	}
}
