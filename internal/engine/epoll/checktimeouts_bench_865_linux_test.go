//go:build linux

package epoll

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
)

// BenchmarkCheckTimeouts865 is the cost of one checkTimeouts sweep over n live
// conns with no timeout configured and no deadline set (nothing closes), the
// steady state of a loop between timer ticks. celeris#865 adds, per conn of
// an async-mode loop, one uncontended detachMu TryLock/Unlock pair, and for
// every conn the call of snapshotH1Deadlines (a nil check of detachMu in sync
// mode, where there is no lock) in place of the loads checkTimeouts made
// inline. One op is one whole sweep, so ns/op is read per conn as ns/op
// divided by n; the sweep runs at most every 25 ms per loop.
func BenchmarkCheckTimeouts865(b *testing.B) {
	for _, mode := range []string{"sync", "async"} {
		for _, n := range []int{256, 4096} {
			b.Run(fmt.Sprintf("%s/%d", mode, n), func(b *testing.B) {
				l := &Loop{
					conns:       make([]*connState, 8),
					liveConns:   make([]*connState, 0, n),
					activeConns: &atomic.Int64{},
					closeCount:  &atomic.Uint64{},
					timerFD:     -1,
				}
				for i := 0; i < n; i++ {
					cs := acquireConnState(context.Background(), 3, 64, mode == "async")
					cs.liveIdx = -1
					cs.protocol = engine.HTTP1
					cs.detected = true
					cs.h1State = conn.NewH1State()
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
