//go:build linux

package epoll

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// h2OpenStream889 builds an HTTP/2 conn state with one stream open: the
// client preface, SETTINGS and the HEADERS of a POST without END_STREAM, so the
// request stays open and no handler runs. It stands for an SSE stream for the
// sweep, which only reads the stream count (an "h2-exempt" run that closed its
// conns says the stream was not open).
func h2OpenStream889(tb testing.TB) *conn.H2State {
	tb.Helper()
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
	st := conn.NewH2State(nil, conn.H2Config{}, func([]byte) {}, nil)
	if err := conn.ProcessH2(context.Background(), wire.Bytes(), st, nil, func([]byte) {}, conn.H2Config{}); err != nil {
		tb.Fatal(err)
	}
	return st
}

// BenchmarkCheckTimeoutsH2889 is the cost of one checkTimeouts sweep over n
// live conns with ReadTimeout 60 s and IdleTimeout 600 s (the defaults), for
// the populations the celeris#889 change touches. "h1-within" is the ordinary
// population, last read just now: no timeout is due, so the conn does not reach
// the h2 look and pays only the shape of the condition; "h2-within" is the same
// for n HTTP/2 conns with a stream open (it runs on main too). "h2-exempt" is n HTTP/2
// conns with a stream open whose last read is long past both timeouts, as an
// SSE stream's conn is on every sweep for its whole life: each pays one
// h2StreamOpen (a detachMu TryLock/Unlock pair in async mode, a nil check
// otherwise, and an atomic load). One op is one whole sweep; ns/op divided by n
// is per conn. "h2-exempt" is the branch only (main closes those conns, and
// the benchmark says so); read it against "h2-within". Compare against an A/A
// twin of the same code (RULE 74).
func BenchmarkCheckTimeoutsH2889(b *testing.B) {
	for _, mode := range []string{"h1-within", "h2-within", "h2-exempt"} {
		for _, async := range []bool{false, true} {
			for _, n := range []int{256, 4096} {
				name := "sync"
				if async {
					name = "async"
				}
				b.Run(fmt.Sprintf("%s/%s/%d", mode, name, n), func(b *testing.B) {
					l := &Loop{
						conns:       make([]*connState, 8),
						liveConns:   make([]*connState, 0, n),
						activeConns: &atomic.Int64{},
						closeCount:  &atomic.Uint64{},
						timerFD:     -1,
						cfg:         resource.Config{ReadTimeout: 60 * time.Second, IdleTimeout: 600 * time.Second},
					}
					for i := 0; i < n; i++ {
						cs := acquireConnState(context.Background(), 3, 64, async)
						cs.liveIdx = -1
						cs.detected = true
						if mode != "h1-within" {
							cs.protocol = engine.H2C
							cs.h2State = h2OpenStream889(b)
							cs.lastActivity = 1
							if mode == "h2-within" {
								cs.lastActivity = time.Now().UnixNano()
							}
						} else {
							cs.protocol = engine.HTTP1
							cs.h1State = conn.NewH1State()
							cs.lastActivity = time.Now().UnixNano()
						}
						l.addLiveConn(cs)
					}
					if len(l.liveConns) != n {
						b.Fatalf("live conns = %d, want %d: the sweep closed some", len(l.liveConns), n)
					}
					b.ReportAllocs()
					for b.Loop() {
						l.checkTimeouts()
					}
					if len(l.liveConns) != n {
						b.Fatalf("live conns = %d after the sweeps, want %d", len(l.liveConns), n)
					}
				})
			}
		}
	}
}
