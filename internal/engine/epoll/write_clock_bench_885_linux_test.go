//go:build linux

package epoll

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// The two benchmarks of the write clock (celeris#885 items 35 and 45). Lines
// ending in FIXONLY name the field the fix adds; the A/B script deletes them to
// build the same benchmark against main, where the conn simply has no clock.

// BenchmarkCheckTimeoutsParked885 is BenchmarkCheckTimeouts865 with every conn
// parked on EPOLLOUT: a response queued, the write clock set, WriteTimeout and
// IdleTimeout configured. It is the sweep of a loop full of stalled or
// slowly-read keep-alive responses, the worst case of the added branch. On main
// the same conns take the idle/read path from lastActivity.
func BenchmarkCheckTimeoutsParked885(b *testing.B) {
	for _, mode := range []string{"sync", "async"} {
		for _, n := range []int{256, 4096} {
			b.Run(fmt.Sprintf("%s/%d", mode, n), func(b *testing.B) {
				l := &Loop{
					conns:       make([]*connState, 8),
					liveConns:   make([]*connState, 0, n),
					activeConns: &atomic.Int64{},
					closeCount:  &atomic.Uint64{},
					timerFD:     -1,
					cfg:         resource.Config{WriteTimeout: time.Hour, IdleTimeout: time.Hour, ReadTimeout: time.Hour},
				}
				now := time.Now().UnixNano()
				for i := 0; i < n; i++ {
					cs := acquireConnState(context.Background(), 3, 64, mode == "async")
					cs.liveIdx = -1
					cs.protocol = engine.HTTP1
					cs.detected = true
					cs.h1State = conn.NewH1State()
					cs.writeBuf = append(cs.writeBuf[:0], make([]byte, 128)...)
					cs.epollOut = true
					cs.lastActivity = now
					cs.writeSince = now // FIXONLY
					l.addLiveConn(cs)
				}
				b.ReportAllocs()
				for b.Loop() {
					l.checkTimeouts()
				}
			})
		}
	}
}

// BenchmarkEPOLLOUTArmDisarmClocked885 is BenchmarkEPOLLOUTArmDisarm with the
// clock ended before each arm, as it is after every drained response: the cost
// of the one time.Now() armEpollOut adds to the first partial flush of a
// response.
func BenchmarkEPOLLOUTArmDisarmClocked885(b *testing.B) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		b.Run(name, func(b *testing.B) {
			epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
			if err != nil {
				b.Fatalf("epoll_create1: %v", err)
			}
			defer func() { _ = unix.Close(epfd) }()
			pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				b.Fatalf("socketpair: %v", err)
			}
			defer func() { _ = unix.Close(pair[0]); _ = unix.Close(pair[1]) }()
			if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, pair[0], &unix.EpollEvent{
				Events: unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP,
				Fd:     int32(pair[0]),
			}); err != nil {
				b.Fatalf("epoll_ctl ADD: %v", err)
			}
			l := &Loop{epollFD: epfd}
			cs := &connState{fd: pair[0]}
			if async {
				cs.detachMu = &sync.Mutex{}
			}
			b.ReportAllocs()
			for b.Loop() {
				cs.writeSince = 0 // FIXONLY
				l.armEpollOut(cs)
				l.disarmEpollOut(cs)
			}
			if cs.epollOut {
				b.Fatal("EPOLLOUT still armed after the disarm")
			}
		})
	}
}
