//go:build linux

package wakefd

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

// celeris#655 cost measurement. The epoll event loop calls FD() for every
// event it dispatches, as the FIRST branch of `for i := range n`:
//
//	if efd := l.wakeFD.FD(); fd == efd && efd >= 0 {
//
// Before this change that field was a plain int load. Routing it through a
// handle must not make the engine's hottest branch pay for the producers'
// barrier, so these benchmarks compare the four shapes FD() could have:
//
//	plain  — main df1269c: a plain struct field, no barrier at all.
//	rwmutex — the first shape of this PR: RLock/RUnlock, two atomic RMWs on
//	          the SAME mutex every producer takes in Signal.
//	shared — a lock-free atomic load, but sharing a cache line with that
//	         mutex, so producers still dirty the line the loop reads.
//	padded — what ships: the atomic on its own cache line (WakeFD).
//
// Each runs with 0 and with 4 producer goroutines hammering Signal, which is
// the WS-broadcast / H2 fan-out shape this engine is tuned for.

// plainFD is main's shape: the descriptor number as a bare field.
type plainFD struct{ fd int }

func (p *plainFD) FD() int { return p.fd }

// lockedFD is this PR as first written: FD() under the producers' RWMutex.
type lockedFD struct {
	mu     sync.RWMutex
	fd     int
	closed bool
}

func (w *lockedFD) FD() int {
	w.mu.RLock()
	fd := w.fd
	w.mu.RUnlock()
	return fd
}

func (w *lockedFD) Signal() {
	w.mu.RLock()
	if !w.closed && w.fd >= 0 {
		var val [8]byte
		val[0] = 1
		_, _ = unix.Write(w.fd, val[:])
	}
	w.mu.RUnlock()
}

// sharedFD is the lock-free load WITHOUT the padding: same fix, but the
// atomic sits on the mutex's cache line. Isolates what the padding buys.
type sharedFD struct {
	num    atomic.Int32
	mu     sync.RWMutex
	fd     int
	closed bool
}

func (w *sharedFD) FD() int { return int(w.num.Load()) }

func (w *sharedFD) Signal() {
	w.mu.RLock()
	if !w.closed && w.fd >= 0 {
		var val [8]byte
		val[0] = 1
		_, _ = unix.Write(w.fd, val[:])
	}
	w.mu.RUnlock()
}

// benchEventFD is a real EFD_NONBLOCK eventfd, so the producers' Signal does
// the same write(2) the engines do. The counter saturates and later writes
// return EAGAIN; that is still a syscall, which is the point — the producer
// must be doing real work while the loop thread reads.
func benchEventFD(b *testing.B) int {
	b.Helper()
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		b.Fatalf("eventfd: %v", err)
	}
	b.Cleanup(func() { _ = unix.Close(efd) })
	return efd
}

// runFDBench times load() — one FD() call, the loop thread's hot branch —
// while `producers` goroutines call signal() without pause.
func runFDBench(b *testing.B, producers int, load func() int, signal func()) {
	var stop atomic.Bool
	var wg sync.WaitGroup
	for range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				signal()
			}
		}()
	}
	sink := 0
	b.ResetTimer()
	for range b.N {
		sink += load()
	}
	b.StopTimer()
	stop.Store(true)
	wg.Wait()
	runtime.KeepAlive(sink)
}

func benchAllContention(b *testing.B, mk func(b *testing.B) (load func() int, signal func())) {
	for _, producers := range []int{0, 4} {
		name := "idle"
		if producers > 0 {
			name = "4producers"
		}
		b.Run(name, func(b *testing.B) {
			load, signal := mk(b)
			runFDBench(b, producers, load, signal)
		})
	}
}

func BenchmarkFDPlainField(b *testing.B) {
	benchAllContention(b, func(b *testing.B) (func() int, func()) {
		efd := benchEventFD(b)
		p := &plainFD{fd: efd}
		// main had no barrier: its producers wrote the number directly.
		return p.FD, func() {
			var val [8]byte
			val[0] = 1
			_, _ = unix.Write(p.fd, val[:])
		}
	})
}

func BenchmarkFDRWMutex(b *testing.B) {
	benchAllContention(b, func(b *testing.B) (func() int, func()) {
		efd := benchEventFD(b)
		w := &lockedFD{fd: efd}
		return w.FD, w.Signal
	})
}

func BenchmarkFDAtomicSharedLine(b *testing.B) {
	benchAllContention(b, func(b *testing.B) (func() int, func()) {
		efd := benchEventFD(b)
		w := &sharedFD{fd: efd}
		w.num.Store(int32(efd))
		return w.FD, w.Signal
	})
}

// BenchmarkFDAtomicPadded is what ships.
func BenchmarkFDAtomicPadded(b *testing.B) {
	benchAllContention(b, func(b *testing.B) (func() int, func()) {
		efd := benchEventFD(b)
		w := New(efd)
		return w.FD, w.Signal
	})
}

// BenchmarkSignal measures the producer side, which DOES keep the barrier:
// one RLock/RUnlock beside a write(2) that was already there. This is the
// cost the PR body claims, on the path where the claim is true.
func BenchmarkSignal(b *testing.B) {
	efd := benchEventFD(b)
	b.Run("plain", func(b *testing.B) {
		p := &plainFD{fd: efd}
		var val [8]byte
		val[0] = 1
		for range b.N {
			_, _ = unix.Write(p.fd, val[:])
		}
	})
	b.Run("wakefd", func(b *testing.B) {
		w := New(efd)
		for range b.N {
			w.Signal()
		}
	})
}
