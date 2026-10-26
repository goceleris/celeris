//go:build linux

package postgres

// celeris#887 item 22 / 23, Opus r1 of #1117 (MAJOR-1): OnRow holds doneMu
// across the buffered append. A panic in that region on the sync path unwinds
// through pollSync's deferred failAll, whose fail() takes doneMu again: with a
// plain unlock the caller goroutine deadlocks and its own recover never runs,
// which is worse than the wedge the change set out to cure.
//
// The panic is a real one inside appendBuffered: OnRow is handed a field slice
// that lives in a PROT_NONE page, so the append's first read of it faults, and
// debug.SetPanicOnFault turns the fault into a recoverable runtime panic.

import (
	"context"
	"runtime"
	"runtime/debug"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// onRowFault is a sync round tripper that feeds the pending head a DataRow
// whose field slice cannot be read.
type onRowFault struct {
	c   *pgConn
	bad [][]byte
}

func (f onRowFault) WriteAndPoll(int, []byte, []byte, func([]byte)) (bool, error) {
	f.c.pendingMu.Lock()
	head := f.c.pending[len(f.c.pending)-1]
	f.c.pendingMu.Unlock()
	head.OnRow(f.bad)
	return true, nil
}

func TestPanicInsideTheLockedRowAppendIsRecoverable887(t *testing.T) {
	mem, err := syscall.Mmap(-1, 0, syscall.Getpagesize(), syscall.PROT_NONE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Skipf("mmap: %v", err)
	}
	defer func() { _ = syscall.Munmap(mem) }()
	bad := unsafe.Slice((*[]byte)(unsafe.Pointer(&mem[0])), 1)

	c := newSyncFaultConn(nil)
	c.syncLoop = &pgSyncLoop{sync: onRowFault{c: c, bad: bad}}
	got := make(chan any, 1)
	go func() {
		debug.SetPanicOnFault(true)
		defer func() { got <- recover() }()
		_, _ = c.simpleQuery(context.Background(), "SELECT 1")
	}()
	select {
	case v := <-got:
		if _, ok := v.(runtime.Error); !ok {
			t.Fatalf("recovered %v (%T), want the runtime fault", v, v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the calling goroutine hung: a panic under OnRow's doneMu deadlocks pollSync's failAll, so the caller's recover never runs")
	}
	if !c.writerMu.TryLock() {
		t.Fatal("writerMu still locked after the recovered panic")
	}
	c.writerMu.Unlock()
	if !c.closed.Load() {
		t.Error("conn not marked broken")
	}
}
