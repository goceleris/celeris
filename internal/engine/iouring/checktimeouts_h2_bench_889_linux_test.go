//go:build linux

package iouring

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/mono"
	"github.com/goceleris/celeris/internal/resource"
)

// h2OpenStream889 builds an HTTP/2 conn state with one stream open, its request
// whole (the answer is the server's to give: an SSE stream for the sweep) or
// not (a POST whose body the client never sends).
func h2OpenStream889(tb testing.TB, requestWhole bool) *conn.H2State {
	tb.Helper()
	st := conn.NewH2State(nil, conn.H2Config{}, func([]byte) {}, nil)
	st.OpenStreamForTest(1, requestWhole)
	return st
}

// BenchmarkCheckTimeoutsH2889 is the cost of one checkTimeouts sweep over n
// live conns with ReadTimeout 60 s and IdleTimeout 600 s (the defaults), for
// the populations the celeris#889 change touches. "h1-within" is the ordinary
// population, last read just now: no timeout is due, so the conn does not reach
// the h2 look and pays only the shape of the condition; "h2-within" is the same
// for n HTTP/2 conns with a stream open (it runs on main too). "h2-exempt" is n
// HTTP/2 conns with a stream open whose last read is long past both timeouts,
// as an SSE stream's conn is on every sweep for its whole life: each pays one
// h2ServerOwes (a detachMu TryLock/Unlock pair and an atomic load; every conn
// here has a detachMu, as the 888 benchmark's). One op is one whole sweep; ns/op
// divided by n is per conn. "h2-exempt" is the branch only (main closes those
// conns, and the benchmark says so); read it against "h2-within". Compare
// against an A/A twin of the same code (RULE 74).
func BenchmarkCheckTimeoutsH2889(b *testing.B) {
	for _, mode := range []string{"h1-within", "h2-within", "h2-exempt"} {
		for _, n := range []int{256, 4096} {
			b.Run(fmt.Sprintf("%s/%d", mode, n), func(b *testing.B) {
				w := &Worker{
					conns:       make([]*connState, n+16),
					liveConns:   make([]int, 0, n),
					errs:        &errclass.Counters{},
					activeConns: &atomic.Int64{},
					closeCount:  &atomic.Uint64{},
					cfg:         resource.Config{ReadTimeout: 60 * time.Second, IdleTimeout: 600 * time.Second},
				}
				for i := 0; i < n; i++ {
					fd := i + 3
					cs := &connState{fd: fd, liveIdx: -1, detected: true}
					cs.detachMu = &sync.Mutex{}
					if mode != "h1-within" {
						cs.protocol.Store(int32(engine.H2C))
						cs.h2State = h2OpenStream889(b, true)
						cs.lastActivity = 1
						if mode == "h2-within" {
							cs.lastActivity = mono.Now()
						}
					} else {
						cs.protocol.Store(int32(engine.HTTP1))
						cs.h1State = conn.NewH1State()
						cs.lastActivity = mono.Now()
					}
					w.conns[fd] = cs
					w.addLiveConn(cs)
					if fd > w.maxFD {
						w.maxFD = fd
					}
				}
				if len(w.liveConns) != n {
					b.Fatalf("live conns = %d, want %d", len(w.liveConns), n)
				}
				b.ReportAllocs()
				for b.Loop() {
					w.checkTimeouts()
				}
				if len(w.liveConns) != n {
					b.Fatalf("live conns = %d after the sweeps, want %d: the sweep closed some", len(w.liveConns), n)
				}
			})
		}
	}
}
