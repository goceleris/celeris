//go:build linux

package iouring

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/resource"
)

// BenchmarkCheckTimeouts888 is the cost of one checkTimeouts sweep over n live
// conns with no timeout configured and nothing queued (nothing closes), the
// steady state of a worker between sweeps. celeris#888 adds a branch for a
// detached conn (its stall clock) and one field to the snapshot of every conn;
// "attached" is the ordinary population, which only pays for the snapshot's
// size, and "detached-idle" is a population of streams with nothing queued,
// which also reads the send state under the snapshot's lock and finds nothing.
// One op is one whole sweep. Compare against an A/A twin of the same binary
// (RULE 74).
func BenchmarkCheckTimeouts888(b *testing.B) {
	for _, mode := range []string{"attached", "detached-idle"} {
		for _, n := range []int{256, 4096} {
			b.Run(fmt.Sprintf("%s/%d", mode, n), func(b *testing.B) {
				w := &Worker{
					conns:       make([]*connState, n+16),
					liveConns:   make([]int, 0, n),
					errs:        &errclass.Counters{},
					activeConns: &atomic.Int64{},
					closeCount:  &atomic.Uint64{},
					cfg:         resource.Config{},
				}
				for i := 0; i < n; i++ {
					fd := i + 3
					cs := &connState{fd: fd, liveIdx: -1, detected: true}
					cs.h1State = conn.NewH1State()
					cs.detachMu = &sync.Mutex{}
					if mode == "detached-idle" {
						cs.h1State.Detached.Store(true)
						cs.h1State.WSReady.Store(true)
					}
					w.conns[fd] = cs
					w.addLiveConn(cs)
					if fd > w.maxFD {
						w.maxFD = fd
					}
				}
				b.ReportAllocs()
				for b.Loop() {
					w.checkTimeouts()
				}
			})
		}
	}
}
