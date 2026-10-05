//go:build linux

package eventloop

// The cost of celeris#862's fix on the path it changes: a Write that leaves
// bytes pending wakes the worker (enqueueFlush, then wake), and the wake now
// goes through the eventfd's handle, which takes a read lock around its
// write(2). The conn's send buffer is full and its peer never reads, so every
// op is one Write of one byte whose flush stops at EAGAIN: one write(2) of
// the conn, then one of the eventfd. The worker has no goroutine of its own
// (newWorker, no run), so nothing drains the eventfd or the pending list; the
// benchmark empties the conn's buffer and the pending list every 1<<16 ops
// with the timer stopped. The same file runs on the base, so benchstat
// compares the two directly.

import (
	"testing"

	"golang.org/x/sys/unix"
)

func BenchmarkWritePending862(b *testing.B) {
	w, fd, _ := bench784Worker(b)
	chunk := make([]byte, 64<<10)
	for {
		if _, err := unix.Write(fd, chunk); err == unix.EAGAIN {
			break
		} else if err != nil {
			b.Fatalf("fill send buffer: %v", err)
		}
	}
	w.mu.RLock()
	c := w.conns[fd]
	w.mu.RUnlock()
	one := []byte{'w'}
	b.ReportAllocs()
	b.ResetTimer()
	for i, n := 0, 0; i < b.N; i++ {
		if err := w.Write(fd, one); err != nil {
			b.Fatalf("Write: %v", err)
		}
		if n++; n == 1<<16 {
			b.StopTimer()
			c.mu.Lock()
			c.writeBuf, c.writePos = c.writeBuf[:0], 0
			c.mu.Unlock()
			w.pendingMu.Lock()
			w.pending = w.pending[:0]
			w.pendingMu.Unlock()
			n = 0
			b.StartTimer()
		}
	}
}
