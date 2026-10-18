//go:build linux

package iouring

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/resource"
)

// BenchmarkCheckTimeouts888 is the cost of one checkTimeouts sweep over n live
// conns, WriteTimeout at its default of 60 s and nothing queued (nothing
// closes), the steady state of a worker between sweeps. celeris#888 adds the
// stall look at a detached conn (sweepDetachedStall: a lock attempt on the
// conn's send state, and its clock). "attached" is the ordinary population,
// which pays for a branch it does not take; "detached-idle" is a population of
// streams with nothing queued, with the look due on every sweep (the worst
// case: lastStallLook is reset, as it would be after the floor of
// stallLookDue); "detached-idle-limited" is the same population in a run of
// sweeps back to back, where the floor (250 ms at this WriteTimeout) lets
// almost none take the look: the cost of the branch that skips it. One op is one whole sweep. Compare against an A/A twin of the same
// binary (RULE 74).
func BenchmarkCheckTimeouts888(b *testing.B) {
	for _, mode := range []string{"attached", "detached-idle", "detached-idle-limited"} {
		for _, n := range []int{256, 4096} {
			b.Run(fmt.Sprintf("%s/%d", mode, n), func(b *testing.B) {
				w := &Worker{
					conns:       make([]*connState, n+16),
					liveConns:   make([]int, 0, n),
					errs:        &errclass.Counters{},
					activeConns: &atomic.Int64{},
					closeCount:  &atomic.Uint64{},
					cfg:         resource.Config{WriteTimeout: 60 * time.Second},
				}
				for i := 0; i < n; i++ {
					fd := i + 3
					cs := &connState{fd: fd, liveIdx: -1, detected: true}
					cs.h1State = conn.NewH1State()
					cs.detachMu = &sync.Mutex{}
					if mode != "attached" {
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
					if mode == "detached-idle" {
						w.lastStallLook = 0
					}
					w.checkTimeouts()
				}
			})
		}
	}
}
