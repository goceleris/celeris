package redisstore

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/internal/fakeredis"
)

func newTestClient(t *testing.T) *redis.Client {
	t.Helper()
	srv := fakeredis.Start(t)
	c, err := redis.NewClient(srv.Addr(), redis.WithForceRESP2())
	if err != nil {
		t.Fatalf("redis.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestAllowUnderLimit(t *testing.T) {
	client := newTestClient(t)
	// RPS=1 → refill is ~1ms per thousand; five rapid Allow calls plus
	// the sixth check complete in far less than that, so the bucket
	// stays empty and the sixth must be denied. Higher RPS values
	// have flaked on slower runners when TCP roundtrips accidentally
	// let the bucket refill mid-test.
	s, err := New(context.Background(), client, Options{RPS: 1, Burst: 5})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 5; i++ {
		allowed, _, _, err := s.Allow("user:alice")
		if err != nil {
			t.Fatalf("Allow %d: %v", i, err)
		}
		if !allowed {
			t.Fatalf("Allow %d: expected true, got false", i)
		}
	}
	allowed, _, _, err := s.Allow("user:alice")
	if err != nil {
		t.Fatalf("Allow 6: %v", err)
	}
	if allowed {
		t.Fatal("Allow 6 after burst exhausted: expected false")
	}
}

func TestAllowRefill(t *testing.T) {
	client := newTestClient(t)
	// RPS=10 → refill interval 100 ms. High enough that the "immediate
	// second Allow" step cannot be pre-empted by a natural refill even
	// on loaded CI runners (macos-latest observed ~7 ms latency between
	// calls — at RPS=1000 that was enough for a refill to land, so the
	// test flaked). 100 ms gives an order-of-magnitude margin; the
	// waited "Allow after refill" step just waits 150 ms instead.
	s, err := New(context.Background(), client, Options{RPS: 10, Burst: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	allowed, _, _, _ := s.Allow("u")
	if !allowed {
		t.Fatal("first Allow: expected true")
	}
	allowed, _, _, _ = s.Allow("u")
	if allowed {
		t.Fatal("immediate second Allow: expected false")
	}
	// 150 ms ≥ 1 refill interval (100 ms); the next Allow must succeed.
	time.Sleep(150 * time.Millisecond)
	allowed, _, _, _ = s.Allow("u")
	if !allowed {
		t.Fatal("Allow after refill: expected true")
	}
}

func TestUndo(t *testing.T) {
	client := newTestClient(t)
	s, err := New(context.Background(), client, Options{RPS: 10, Burst: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	allowed, rem, _, err := s.Allow("k")
	if err != nil {
		t.Fatalf("first Allow: %v", err)
	}
	if !allowed {
		t.Fatalf("first Allow failed; rem=%d", rem)
	}
	if err := s.Undo("k"); err != nil {
		t.Fatalf("Undo: %v", err)
	}
	allowed, rem, _, err = s.Allow("k")
	if err != nil {
		t.Fatalf("second Allow: %v", err)
	}
	if !allowed {
		t.Fatalf("after Undo, Allow expected true; rem=%d", rem)
	}
}

func TestPerKeyIsolation(t *testing.T) {
	client := newTestClient(t)
	s, err := New(context.Background(), client, Options{RPS: 1, Burst: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	okA, _, _, _ := s.Allow("a")
	okB, _, _, _ := s.Allow("b")
	if !okA || !okB {
		t.Fatal("independent keys should each get their burst")
	}
	okA, _, _, _ = s.Allow("a")
	if okA {
		t.Fatal("second Allow on same key should deny")
	}
}
