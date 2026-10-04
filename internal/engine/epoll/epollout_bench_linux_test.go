//go:build linux

package epoll

import (
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// BenchmarkEPOLLOUTArmDisarm measures one EPOLLOUT arm and disarm, the two
// EPOLL_CTL_MODs a backpressured flush costs the loop (armEpollOut when a
// flush stops short, disarmEpollOut once the socket drains). For an async
// conn, modEpollOut reads hijacked under driverMu.RLock before each MOD
// (celeris#668, review of #698); a sync conn takes no lock. The two
// sub-benchmarks differ by that lock alone.
func BenchmarkEPOLLOUTArmDisarm(b *testing.B) {
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
				l.armEpollOut(cs)
				l.disarmEpollOut(cs)
			}
			if cs.epollOut {
				b.Fatal("EPOLLOUT still armed after the disarm")
			}
		})
	}
}
