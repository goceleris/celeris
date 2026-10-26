//go:build linux

package iouring

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#884 item 70 (found in the ADOPT lane): handleRecv's direct-body
// block hands the bytes that follow a body received straight into
// H1State.bodyBuf (the "tail") to ProcessH1. On an AsyncHandlers engine that
// call ran without InlineMode, so an async-marked route pipelined behind the
// body ran its handler on the worker thread, where the first-recv path stops
// at the route (ErrAsyncDispatch) and promotes the conn to its dispatch
// goroutine. The worker, and every other conn of its ring, was parked for as
// long as that handler ran (celeris#704's class).
//
// The conn is still inline-first at that point: a fixed-length body split
// across recvs does not promote it (HasPendingDispatchState excludes
// bodyNeeded), and the scenario of celeris#908's tests reaches the block: a
// 20000-byte POST first so bodyBuf has room, then a 2000-byte POST in two
// writes, the last carrying the tail.

// startAsyncStall884 starts an AsyncHandlers io_uring engine whose handler is
// stallHandler704 (/slow is the async, gated route; every other path is
// answered inline with the serving worker's id).
func startAsyncStall884(t *testing.T, gate *stallGate704) (*Engine, string) {
	t.Helper()
	addr := freeAddr639(t)
	e, cancel, done := startRingRetried662(t, func() (*Engine, error) {
		return New(resource.Config{
			Addr: addr, Protocol: engine.HTTP1, AsyncHandlers: true,
			Resources: resource.Resources{Workers: 2},
		}, stream.Handler(stallHandler704{gate: gate}))
	})
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("engine did not stop within 5s")
		}
	})
	return e, addr
}

func TestIouringDirectBodyTailRunsAnAsyncRouteOnItsGoroutine884(t *testing.T) {
	for _, tc := range []struct {
		name string
		// tail1 rides behind the second half of the small POST's body; tail2,
		// when set, goes out in a recv of its own after the engine has read tail1.
		tail1, tail2 string
		// inline is how many responses precede /slow's: the POST's own, and
		// the pipelined sync requests the tail serves before the async route.
		inline int
		// promoted: the connection is promoted by the tail (the route is
		// parsed in the tail). The split-head arm promotes in the next recv's
		// own path, as it always has: it pins that the fix leaves it alone.
		promotes bool
	}{
		{"async-route", "GET /slow HTTP/1.1\r\nHost: x\r\n\r\n", "", 1, true},
		{"sync-then-async", "GET /fast HTTP/1.1\r\nHost: x\r\n\r\nGET /slow HTTP/1.1\r\nHost: x\r\n\r\n", "", 2, true},
		{"split-head", "GET /slow HTTP/1.1\r\nHo", "st: x\r\n\r\n", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			singleShot908(t)
			gate := newStallGate704(t)
			e, addr := startAsyncStall884(t, gate)
			gate.openAtCleanup(t)
			slow := stallDial704(t, addr)
			fast, worker := colocate704(t, addr, slow)
			base := e.Metrics().BytesRead
			m0 := e.Metrics()
			sent := uint64(0)
			write := func(s string) {
				t.Helper()
				if _, err := slow.c.Write([]byte(s)); err != nil {
					t.Fatalf("write: %v", err)
				}
				sent += uint64(len(s))
			}
			_ = slow.c.SetDeadline(time.Now().Add(30 * time.Second))
			readResp := func(d time.Duration) (string, error) {
				_ = slow.c.SetReadDeadline(time.Now().Add(d))
				resp, err := http.ReadResponse(slow.br, nil)
				if err != nil {
					return "", err
				}
				b, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				return string(b), err
			}

			// A large body first so bodyBuf has room, then the small POST
			// whose second half is received straight into it.
			big := strings.Repeat("x", 20000)
			write(postHead908("/p", len(big)) + big[:10000])
			waitServerRead908(t, e, base+sent)
			write(big[10000:])
			if b, err := readResp(5 * time.Second); err != nil || b != worker {
				t.Fatalf("apparatus: the large POST: %q %v", b, err)
			}
			small := strings.Repeat("y", 2000)
			write(postHead908("/q", len(small)) + small[:1000])
			waitServerRead908(t, e, base+sent)
			write(small[1000:] + tc.tail1)
			if tc.tail2 != "" {
				waitServerRead908(t, e, base+sent)
				write(tc.tail2)
			}

			// The async handler is running. Wherever it runs, the worker must
			// keep serving the conns of its ring and the responses that
			// precede /slow must be on their way.
			gate.waitEntered(t)
			failed := false
			for i := 0; i < tc.inline; i++ {
				if b, err := readResp(2 * time.Second); err != nil || !strings.HasPrefix(b, "w=") {
					t.Errorf("celeris#884 item 70: response %d of %d before /slow's did not arrive while /slow's handler was gated: %q %v "+
						"(the worker ran the handler itself, with the response still unflushed)", i+1, tc.inline, b, err)
					failed = true
					break
				}
			}
			if _, err := pingFast704(fast, 10); err != nil {
				t.Errorf("celeris#884 item 70: a fast request on worker %s did not complete while the pipelined async route's handler was gated: "+
					"the worker was parked in the handler: %v", worker, err)
				failed = true
			}
			m := e.Metrics()
			if tc.promotes && m.AsyncPromotedConns != m0.AsyncPromotedConns+1 {
				t.Errorf("celeris#884 item 70: AsyncPromotedConns moved by %d, want 1: the conn was not promoted for the async route",
					int64(m.AsyncPromotedConns)-int64(m0.AsyncPromotedConns))
				failed = true
			}
			gate.open()
			if failed {
				return
			}
			if b, err := readResp(5 * time.Second); err != nil || b != "slow" {
				t.Errorf("the async route's response: %q %v, want %q", b, err, "slow")
			}
			// The conn goes on: a request after the async one is answered, in order.
			if b, err := slow.get("/fast", 5*time.Second); err != nil || b != worker {
				t.Errorf("a request after the async route: %q %v, want %q", b, err, worker)
			}
		})
	}
}
