//go:build linux

package eventloop

// BenchmarkPushDeliveryWhileCalls931 is the cost side of celeris#931: what the
// worker's TryLock-and-requeue does to the delivery of bytes that race with
// WriteAndPoll* calls. It uses no symbol the fix adds, so the same file runs on
// the base, where the worker waits for the caller's recvMu instead.
//
// Four conns each have a peer that pushes an 8-byte frame carrying its send time
// every 100 to 300 us, and an owner that calls WriteAndPollMulti back to back
// (the call returns after the first data it reads, or after a 1 ms poll with
// none), so the events of a conn are collected while a call holds its recvMu,
// between calls and in the middle of the re-arm. One op is one call. The
// metrics are the delivery latency of a pushed frame, from the peer's write to
// the onRecv that completes it, whoever delivers it (the call or the worker),
// and late/frame, the share of frames delivered more than 500 us after the push.

import (
	"encoding/binary"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func BenchmarkPushDeliveryWhileCalls931(b *testing.B) {
	l, err := New(1)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer func() { _ = l.Close() }()
	w := l.WorkerLoop(0).(*worker)

	const conns = 4
	rigs := make([]*b931Rig, conns)
	for i := range rigs {
		r := &b931Rig{}
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			b.Fatalf("socketpair: %v", err)
		}
		for _, fd := range fds {
			if err := unix.SetNonblock(fd, true); err != nil {
				b.Fatalf("nonblock: %v", err)
			}
		}
		r.fd, r.peer = fds[0], fds[1]
		rigs[i] = r
		if err := w.RegisterConn(r.fd, r.onRecv, func(error) {}); err != nil {
			b.Fatalf("RegisterConn: %v", err)
		}
		defer func() { _ = w.UnregisterConn(r.fd); _ = unix.Close(r.fd); _ = unix.Close(r.peer) }()
	}
	stop := make(chan struct{})
	var pushers sync.WaitGroup
	for i, r := range rigs {
		pushers.Add(1)
		go func() {
			defer pushers.Done()
			rng := uint32(i*7919 + 1)
			sink := make([]byte, 256)
			var frame [8]byte
			for {
				select {
				case <-stop:
					return
				default:
				}
				binary.LittleEndian.PutUint64(frame[:], uint64(time.Now().UnixNano()))
				_, _ = unix.Write(r.peer, frame[:])
				for {
					if m, _ := unix.Read(r.peer, sink); m <= 0 { // what the owner sends
						break
					}
				}
				rng = rng*1664525 + 1013904223
				time.Sleep(time.Duration(100+int(rng>>8)%200) * time.Microsecond)
			}
		}()
	}
	var next atomic.Int64
	var owners sync.WaitGroup
	b.ResetTimer()
	for _, r := range rigs {
		owners.Add(1)
		go func() {
			defer owners.Done()
			rbuf := make([]byte, 1024)
			for next.Add(1) <= int64(b.N) {
				_, _ = w.WriteAndPollMulti(r.fd, []byte("req"), rbuf, r.onRecv, func() bool { return true }, nil)
			}
		}()
	}
	owners.Wait()
	b.StopTimer()
	close(stop)
	pushers.Wait()
	time.Sleep(20 * time.Millisecond) // the worker delivers the tail
	var all []time.Duration
	for _, r := range rigs {
		r.mu.Lock()
		all = append(all, r.lat...)
		r.mu.Unlock()
	}
	if len(all) == 0 {
		b.Fatal("no frame was delivered")
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	pick := func(q float64) float64 { return float64(all[int(q*float64(len(all)-1))].Nanoseconds()) }
	late := 0
	for _, d := range all {
		if d > 500*time.Microsecond {
			late++
		}
	}
	b.ReportMetric(float64(late)/float64(len(all)), "late/frame") // frames delivered more than 500 us after the push
	b.ReportMetric(pick(0.5), "p50-ns")
	b.ReportMetric(pick(0.99), "p99-ns")
	b.ReportMetric(pick(0.9999), "p9999-ns")
	b.ReportMetric(float64(all[len(all)-1].Nanoseconds()), "max-ns")
	b.ReportMetric(float64(len(all))/b.Elapsed().Seconds(), "frames/s")
}

// b931Rig is one conn of the benchmark and the frames it has delivered.
type b931Rig struct {
	fd, peer int
	// mu: the call and the worker deliver one at a time; the lock is for the
	// race detector.
	mu   sync.Mutex
	part []byte
	lat  []time.Duration
}

// onRecv assembles the 8-byte frames and records each one's delivery latency.
func (r *b931Rig) onRecv(p []byte) {
	now := time.Now().UnixNano()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.part = append(r.part, p...)
	for len(r.part) >= 8 {
		r.lat = append(r.lat, time.Duration(now-int64(binary.LittleEndian.Uint64(r.part[:8]))))
		r.part = r.part[8:]
	}
}
