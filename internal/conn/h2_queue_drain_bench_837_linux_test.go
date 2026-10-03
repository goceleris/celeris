//go:build linux

package conn

import (
	"encoding/binary"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/wakefd"
)

// BenchmarkH2QueueEnqueueWhileDraining837 measures what celeris#837's fix
// costs. An Enqueue that lands while DrainTo runs now finds pending cleared:
// it signals the loop's eventfd, and the loop runs one more drain. Before the
// fix that Enqueue signalled nothing, and when its shard had already been
// drained its frame was stranded until a later Enqueue.
//
// A "loop" goroutine polls a real eventfd and drains the queue while pending
// is set, as the engines do. GOMAXPROCS producers enqueue b.N frames in all,
// over the four shards. After the producers finish, the loop does one last
// drain whatever pending says, so a run on a tree without the fix ends too.
// Reported per frame: eventfd writes (signals/frame), drains (drains/frame),
// and frames that only that last unconditional drain found (stranded/frame,
// 0 with the fix).
func BenchmarkH2QueueEnqueueWhileDraining837(b *testing.B) {
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		b.Fatalf("eventfd: %v", err)
	}
	wake := wakefd.New(efd)
	defer wake.Close()
	var q h2ShardedQueue
	q.wake = wake

	var drained, drains, signals, stranded atomic.Int64
	final := make(chan struct{})
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		var buf [8]byte
		fds := []unix.PollFd{{Fd: int32(efd), Events: unix.POLLIN}}
		for {
			select {
			case <-final:
				var n int64
				q.DrainTo(func([]byte) { n++ })
				drained.Add(n)
				stranded.Add(n)
				return
			default:
			}
			if _, err := unix.Poll(fds, 1); err != nil && err != unix.EINTR {
				b.Errorf("poll: %v", err)
				return
			}
			if n, _ := unix.Read(efd, buf[:]); n == 8 {
				signals.Add(int64(binary.LittleEndian.Uint64(buf[:])))
			}
			if q.pending.Load() {
				drains.Add(1)
				var n int64
				q.DrainTo(func([]byte) { n++ })
				drained.Add(n)
			}
		}
	}()

	producers := runtime.GOMAXPROCS(0)
	per, extra := b.N/producers, b.N%producers
	b.ResetTimer()
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		n := per
		if p < extra {
			n++
		}
		wg.Add(1)
		go func(p, n int) {
			defer wg.Done()
			for i := 0; i < n; i++ {
				buf := getH2FrameBuf()
				*buf = append((*buf)[:0], 0, 0, 0, 0, 0, 0, 0, 0, 0)
				q.Enqueue(uint32(2*(p*h2QueueShards+i%h2QueueShards)+1), buf)
			}
		}(p, n)
	}
	wg.Wait()
	// Let the loop take what the producers' last signals announced, then
	// end it with the unconditional drain.
	for spins := 0; q.pending.Load() && spins < 1_000_000; spins++ {
		runtime.Gosched()
	}
	b.StopTimer()
	close(final)
	wake.Signal()
	<-loopDone
	if got := drained.Load(); got != int64(b.N) {
		b.Fatalf("drained %d frames, enqueued %d", got, b.N)
	}
	b.ReportMetric(float64(signals.Load())/float64(b.N), "signals/frame")
	b.ReportMetric(float64(drains.Load())/float64(b.N), "drains/frame")
	b.ReportMetric(float64(stranded.Load())/float64(b.N), "stranded/frame")
}
