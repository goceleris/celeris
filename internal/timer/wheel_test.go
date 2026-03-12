package timer

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestWheelScheduleAndExpire(t *testing.T) {
	var expired atomic.Int32
	var expiredFD int
	var expiredKind TimeoutKind

	w := New(func(fd int, kind TimeoutKind) {
		expiredFD = fd
		expiredKind = kind
		expired.Add(1)
	})

	w.Schedule(42, 150*time.Millisecond, IdleTimeout)

	// Should not expire immediately.
	n := w.Tick()
	if n != 0 {
		t.Fatalf("expected 0 expired, got %d", n)
	}

	time.Sleep(200 * time.Millisecond)
	n = w.Tick()
	if n != 1 {
		t.Fatalf("expected 1 expired, got %d", n)
	}
	if expiredFD != 42 {
		t.Fatalf("expected FD 42, got %d", expiredFD)
	}
	if expiredKind != IdleTimeout {
		t.Fatalf("expected IdleTimeout, got %d", expiredKind)
	}
}

func TestWheelCancel(t *testing.T) {
	var expired atomic.Int32
	w := New(func(_ int, _ TimeoutKind) {
		expired.Add(1)
	})

	w.Schedule(10, 150*time.Millisecond, ReadTimeout)
	w.Cancel(10)

	time.Sleep(200 * time.Millisecond)
	w.Tick()
	if expired.Load() != 0 {
		t.Fatalf("expected 0 expired after cancel, got %d", expired.Load())
	}
}

func TestWheelMultipleEntries(t *testing.T) {
	expired := make(map[int]bool)
	w := New(func(fd int, _ TimeoutKind) {
		expired[fd] = true
	})

	w.Schedule(1, 150*time.Millisecond, IdleTimeout)
	w.Schedule(2, 150*time.Millisecond, IdleTimeout)
	w.Schedule(3, 150*time.Millisecond, IdleTimeout)

	time.Sleep(200 * time.Millisecond)
	n := w.Tick()
	if n != 3 {
		t.Fatalf("expected 3 expired, got %d", n)
	}
	for _, fd := range []int{1, 2, 3} {
		if !expired[fd] {
			t.Fatalf("expected FD %d to expire", fd)
		}
	}
}

func TestWheelReschedule(t *testing.T) {
	var lastKind TimeoutKind
	count := 0
	w := New(func(_ int, kind TimeoutKind) {
		lastKind = kind
		count++
	})

	// Schedule then reschedule with different kind.
	w.Schedule(5, 150*time.Millisecond, ReadTimeout)
	w.Schedule(5, 150*time.Millisecond, WriteTimeout)

	time.Sleep(200 * time.Millisecond)
	w.Tick()
	// Only the latest schedule should fire.
	if count != 1 {
		t.Fatalf("expected 1 expiry, got %d", count)
	}
	if lastKind != WriteTimeout {
		t.Fatalf("expected WriteTimeout, got %d", lastKind)
	}
}

func TestWheelLen(t *testing.T) {
	w := New(func(_ int, _ TimeoutKind) {})
	w.Schedule(1, time.Second, IdleTimeout)
	w.Schedule(2, time.Second, IdleTimeout)
	if w.Len() != 2 {
		t.Fatalf("expected Len=2, got %d", w.Len())
	}
	w.Cancel(1)
	if w.Len() != 1 {
		t.Fatalf("expected Len=1, got %d", w.Len())
	}
}

func BenchmarkWheelSchedule(b *testing.B) {
	w := New(func(_ int, _ TimeoutKind) {})
	b.ResetTimer()
	for i := range b.N {
		w.Schedule(i%10000, 5*time.Second, IdleTimeout)
	}
}

func BenchmarkWheelTick(b *testing.B) {
	w := New(func(_ int, _ TimeoutKind) {})
	// Pre-fill with entries that won't expire.
	for i := range 1000 {
		w.Schedule(i, time.Hour, IdleTimeout)
	}
	b.ResetTimer()
	for range b.N {
		w.Tick()
	}
}
