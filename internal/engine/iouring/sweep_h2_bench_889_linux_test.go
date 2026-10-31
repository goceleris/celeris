//go:build linux

package iouring

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/mono"
	"github.com/goceleris/celeris/internal/resource"
)

// h2ArmedState889 is an h2 conn state with one request open and armed: the
// client preface, SETTINGS and the HEADERS of a POST without END_STREAM, through
// ProcessH2, with the defaults of the request deadline.
func h2ArmedState889(tb testing.TB) *conn.H2State {
	tb.Helper()
	cfg := sweepCfg889()
	st := conn.NewH2State(nil, cfg, func([]byte) {}, nil)
	var wire bytes.Buffer
	wire.WriteString(http2.ClientPreface)
	fr := http2.NewFramer(&wire, nil)
	if err := fr.WriteSettings(); err != nil {
		tb.Fatal(err)
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "POST"}, {":scheme", "http"}, {":authority", "x"}, {":path", "/"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndHeaders: true}); err != nil {
		tb.Fatal(err)
	}
	if err := conn.ProcessH2(context.Background(), wire.Bytes(), st, nil, func([]byte) {}, cfg); err != nil {
		tb.Fatal(err)
	}
	return st
}

// BenchmarkSweepH2Deadlines889 is the cost of one checkTimeouts sweep over n
// HTTP/2 conns that are on h2Conns, with the defaults of ReadTimeout (60 s) and
// IdleTimeout (600 s): the conns' own clocks are fresh, so the conn scan does
// nothing, and what is added by the pass over h2Conns for the per-stream
// deadlines (celeris#889) is the difference between main and the branch.
// "no-stream" is a conn with nothing open, "exempt" one whose request is whole
// (an SSE stream: no deadline), "armed" one whose request is open and armed (the
// deadline is not due). One op is one whole sweep; ns/op divided by n is per
// conn. Compare against an A/A twin of main (RULE 74).
func BenchmarkSweepH2Deadlines889(b *testing.B) {
	for _, mode := range []string{"no-stream", "exempt", "armed"} {
		for _, n := range []int{256, 4096} {
			b.Run(fmt.Sprintf("%s/%d", mode, n), func(b *testing.B) {
				w := &Worker{
					conns:       make([]*connState, n+16),
					liveConns:   make([]int, 0, n),
					h2Conns:     make([]int, 0, n),
					errs:        &errclass.Counters{},
					activeConns: &atomic.Int64{},
					closeCount:  &atomic.Uint64{},
					cfg:         resource.Config{ReadTimeout: 60 * time.Second, IdleTimeout: 600 * time.Second},
				}
				now := mono.Now()
				for i := 0; i < n; i++ {
					fd := i + 3
					cs := &connState{fd: fd, liveIdx: -1, detected: true}
					cs.detachMu = &sync.Mutex{}
					cs.protocol.Store(int32(engine.H2C))
					cs.lastActivity = now
					switch mode {
					case "no-stream":
						cs.h2State = conn.NewH2State(nil, sweepCfg889(), func([]byte) {}, nil)
					case "exempt":
						cs.h2State = conn.NewH2State(nil, sweepCfg889(), func([]byte) {}, nil)
						cs.h2State.OpenStreamForTest(1, true)
					default:
						cs.h2State = h2ArmedState889(b)
					}
					cs.writeFn = func([]byte) {}
					w.conns[fd] = cs
					w.addLiveConn(cs)
					if fd > w.maxFD {
						w.maxFD = fd
					}
					w.h2Conns = append(w.h2Conns, fd)
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
