//go:build linux

package iouring

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// The connState fields the celeris#704 tests set or read.

// setParked704 marks cs's dispatch goroutine parked (waiting for input, so
// not holding detachMu across a handler) or not. Caller holds asyncInMu.
func setParked704(cs *connState, parked bool) { cs.asyncParked = parked }

// relinkOwed704 reports whether cs's dispatch goroutine owes the worker a
// hand-back of a conn the dirty pass gave up.
func relinkOwed704(cs *connState) bool {
	cs.asyncInMu.Lock()
	defer cs.asyncInMu.Unlock()
	return cs.relinkOwed
}

// mutexWaiters reports how many goroutines are queued in a blocking Lock of
// mu: the waiter count in the high bits of sync.Mutex's state word (the low
// three are the locked, woken and starving flags). A goroutine registers
// there when it gives up spinning and is about to sleep, so "waiters >= 1" is
// the sync point "the caller reached its blocking Lock and waits": a failed
// TryLock does not register. It replaces a 200 ms timer in the arms that ask
// whether a site waits on a bounded holder (celeris#884 items 4, 5 and 22).
//
// It reads sync.Mutex's private layout through unsafe, as the standard
// library's own tests do not: a Go release that changes it must fail loudly,
// not read garbage. TestMutexWaiters884 checks the layout against real
// blocked goroutines and names the toolchain it ran on. Layout checked in
// internal/sync/mutex.go of go1.27.1 and go1.27.2: state int32 then sema
// uint32, mutexLocked=1, mutexWoken=2, mutexStarving=4, mutexWaiterShift=3.
func mutexWaiters(mu *sync.Mutex) int {
	return int(atomic.LoadInt32((*int32)(unsafe.Pointer(mu))) >> 3)
}

// waitMutexWaiters884 polls until at least n goroutines wait in mu's blocking
// Lock, or until stop reports true, or d passes. It returns the waiter count
// seen when it stopped.
func waitMutexWaiters884(mu *sync.Mutex, n int, d time.Duration, stop func() bool) int {
	for end := time.Now().Add(d); ; {
		if w := mutexWaiters(mu); w >= n || (stop != nil && stop()) || time.Now().After(end) {
			return w
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// TestMutexWaiters884 pins mutexWaiters to the layout of the running Go
// toolchain: an unlocked mutex reads 0, a locked one reads 0 waiters, each
// goroutine blocked in Lock adds one, and the count falls back as they get
// the lock. A change of sync.Mutex's representation fails here, by name, and
// not as a flaky wait in the arms that rely on the helper.
func TestMutexWaiters884(t *testing.T) {
	t.Logf("celeris884 MUTEXLAYOUT %s %s/%s Sizeof(sync.Mutex)=%d", runtime.Version(), runtime.GOOS, runtime.GOARCH,
		unsafe.Sizeof(sync.Mutex{}))
	if got := unsafe.Sizeof(sync.Mutex{}); got != 8 {
		t.Fatalf("sync.Mutex is %d bytes, want 8 (state int32, sema uint32): mutexWaiters reads a layout this "+
			"toolchain no longer has", got)
	}
	var mu sync.Mutex
	if w := mutexWaiters(&mu); w != 0 {
		t.Fatalf("an unlocked mutex reads %d waiters, want 0", w)
	}
	mu.Lock()
	if w := mutexWaiters(&mu); w != 0 {
		t.Fatalf("a locked mutex with no waiter reads %d waiters, want 0", w)
	}
	if mu.TryLock() {
		t.Fatal("TryLock took a locked mutex")
	}
	if w := mutexWaiters(&mu); w != 0 {
		t.Fatalf("a failed TryLock left %d waiters, want 0 (it must not register)", w)
	}
	var got sync.WaitGroup
	for i := 1; i <= 3; i++ {
		got.Add(1)
		go func() {
			defer got.Done()
			mu.Lock()
			mu.Unlock()
		}()
		if w := waitMutexWaiters884(&mu, i, 5*time.Second, nil); w != i {
			t.Fatalf("with %d goroutines blocked in Lock mutexWaiters reads %d (layout changed under %s?)",
				i, w, runtime.Version())
		}
	}
	mu.Unlock()
	got.Wait()
	if w := mutexWaiters(&mu); w != 0 {
		t.Fatalf("after every waiter got the lock mutexWaiters reads %d, want 0", w)
	}
}
