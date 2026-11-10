//go:build linux

package iouring

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/mono"
	"github.com/goceleris/celeris/internal/resource"
)

// BenchmarkCheckTimeoutsStaged1173 is one checkTimeouts sweep over 4096 conns
// with a real SQ ring holding `staged` SEND SQEs of as many of them, that the
// kernel has not consumed (celeris#1173: the sweep restamps their conns). 0 is a
// sweep after a wait that submitted everything; 32 and 256 are what a busy
// worker's iteration leaves staged when it sweeps. One op is one whole sweep.
// The same file compiles on main (the staged SQEs are then only ballast): run it
// on main and on the head, interleaved, with an A/A twin of the same binary.
func BenchmarkCheckTimeoutsStaged1173(b *testing.B) {
	const n = 4096
	for _, staged := range []int{0, 32, 256} {
		b.Run(fmt.Sprintf("staged=%d", staged), func(b *testing.B) {
			ring, err := NewRing(512, 0, 0)
			if err != nil {
				b.Skipf("io_uring unavailable: %v", err)
			}
			defer func() { _ = ring.Close() }()
			w := &Worker{
				ring:        ring,
				conns:       make([]*connState, n+16),
				liveConns:   make([]int, 0, n),
				errs:        &errclass.Counters{},
				activeConns: &atomic.Int64{},
				closeCount:  &atomic.Uint64{},
				cfg:         resource.Config{WriteTimeout: time.Hour},
			}
			now := mono.Now()
			for i := 0; i < n; i++ {
				fd := i + 3
				cs := &connState{fd: fd, liveIdx: -1, detected: true, lastActivity: now}
				cs.h1State = conn.NewH1State()
				w.conns[fd] = cs
				w.addLiveConn(cs)
				if fd > w.maxFD {
					w.maxFD = fd
				}
				if i < staged {
					cs.sending = true
					sqe := ring.GetSQE()
					if sqe == nil {
						b.Fatal("SQ ring full")
					}
					prepSendPlain(sqe, fd, []byte("x"), false)
					setSQEUserData(sqe, encodeUserDataGen(udSend, fd, cs.generation))
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				w.checkTimeouts()
			}
		})
	}
}
