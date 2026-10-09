//go:build linux

package epoll

import (
	"net"
	"sync"
	"testing"
	"time"
)

// BenchmarkCloseConnSlotClear isolates what celeris#775 adds to closeConn and
// to shutdown's phase 3: the slot write goes from bare to one uncontended
// driverMu write lock. The two arms run the same slice write.
func BenchmarkCloseConnSlotClear(b *testing.B) {
	cs := &connState{}
	l := &Loop{conns: make([]*connState, 1024)}
	b.Run("bare", func(b *testing.B) {
		for i := 0; b.Loop(); i++ {
			l.conns[i&1023] = cs
			l.conns[i&1023] = nil
		}
	})
	b.Run("locked", func(b *testing.B) {
		for i := 0; b.Loop(); i++ {
			l.conns[i&1023] = cs
			l.driverMu.Lock()
			l.conns[i&1023] = nil
			l.driverMu.Unlock()
		}
	})
}

// BenchmarkCloseChurn is the close-heavy end-to-end measure of the same
// change: connections are dialled and closed by the client, in parallel, and
// the engine's workers accept them and close them (EOF, closeConn). One op is
// one connection through accept and close; the clock runs until the engine
// has counted every close. The kernel's connect and close dominate each op, so
// the figure is read against an A/A floor; BenchmarkCloseConnSlotClear
// isolates the lock itself.
func BenchmarkCloseChurn(b *testing.B) {
	eng, stop := newTestEngine(b)
	defer stop()
	addr := eng.Addr().String()
	closes := eng.loops[0].closeCount
	base := closes.Load()
	b.ReportAllocs()
	b.ResetTimer()
	var wg sync.WaitGroup
	next := make(chan struct{}, 256)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range next {
				c, err := net.Dial("tcp", addr)
				if err != nil {
					b.Errorf("dial: %v", err)
					return
				}
				_ = c.Close()
			}
		}()
	}
	for range b.N {
		next <- struct{}{}
	}
	close(next)
	wg.Wait()
	deadline := time.Now().Add(30 * time.Second)
	for closes.Load()-base < uint64(b.N) {
		if time.Now().After(deadline) {
			b.Fatalf("the engine closed %d of %d connections", closes.Load()-base, b.N)
		}
		time.Sleep(50 * time.Microsecond)
	}
	b.StopTimer()
}
