package async

import (
	"testing"
	"time"
)

func TestBackoffMonotonicAndCapped(t *testing.T) {
	b := Backoff{Base: 10 * time.Millisecond, Cap: 80 * time.Millisecond, Jitter: 0}
	expected := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond,
		80 * time.Millisecond, // capped
		80 * time.Millisecond, // capped
	}
	for i, want := range expected {
		got := b.Next(i)
		if got != want {
			t.Fatalf("attempt %d: want %v got %v", i, want, got)
		}
	}
}

func TestBackoffDefaults(t *testing.T) {
	b := NewBackoff(0, 0)
	d := b.Next(0)
	// NewBackoff defaults: base=50ms, cap=5s, jitter=0.2 → range [40ms, 60ms].
	if d < 40*time.Millisecond || d > 60*time.Millisecond {
		t.Fatalf("default Next(0) out of range: %v", d)
	}
}

func TestBackoffJitterInRange(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Cap: time.Second, Jitter: 0.5}
	for i := 0; i < 50; i++ {
		got := b.Next(2) // base * 4 = 400ms
		low := time.Duration(float64(400*time.Millisecond) * 0.5)
		high := time.Duration(float64(400*time.Millisecond) * 1.5)
		if got < low || got > high {
			t.Fatalf("jittered Next out of range: %v (want [%v,%v])", got, low, high)
		}
	}
}

func TestBackoffReset(t *testing.T) {
	b := Backoff{Base: 10 * time.Millisecond, Cap: time.Second, Jitter: 0}
	b.Next(3)
	if b.Attempts() != 4 {
		t.Fatalf("Attempts=%d", b.Attempts())
	}
	b.Reset()
	if b.Attempts() != 0 {
		t.Fatalf("Attempts after reset=%d", b.Attempts())
	}
}

func TestBackoffLargeAttempt(t *testing.T) {
	// Very large attempts must not overflow; cap must be honored.
	b := Backoff{Base: time.Millisecond, Cap: time.Second, Jitter: 0}
	if got := b.Next(1000); got != time.Second {
		t.Fatalf("large attempt: want cap %v got %v", time.Second, got)
	}
}
