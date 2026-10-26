//go:build linux

package epoll

import (
	"bufio"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
)

// celeris#885 item 56. replayCarriedAsync, which replays a transplant's carried
// bytes on an AsyncHandlers loop, returns on conn.ErrHijacked before it writes
// cs.h1State.InlineMode = false, as drainRead does since celeris#774: a handler
// that hijacked inline has had cs released inside the Hijack call, so cs.h1State
// is nil by then, or the parser state of the connection that took cs from the
// pool. No test pinned the guard: moving the write above the return (the
// pre-#774 shape of drainRead) left the whole package green, because the
// in-tree transplant source never carries bytes under AsyncHandlers (it is
// reachable through the public engine.TransplantTarget API, AdoptConn, with a
// hand-built Carryover, which is how these tests reach it).
//
// A conn is adopted onto an AsyncHandlers loop with a carry whose first request
// hijacks inline. In testHookHijackReleased, between the release and the
// handler's return from Hijack, the hook takes the released connState out of the
// pool, as the next accept on the loop does, and gives it an h1State in
// InlineMode. Once the engine has stopped, every state the hook took must still
// be in InlineMode: nothing wrote to it from the replay of the hijacked
// connection. Rounds go on until the pool has handed the released state back
// five times, at most 200, and the test fails if it never does, so a pool change
// cannot make it vacuous. AsyncPromotedConns must stay 0: the carry's replay
// must not promote a conn it does not own any more.
func TestCarriedReplayHijackLeavesReissuedConnStateAlone885(t *testing.T) {
	const (
		wantReuse = 5
		maxRounds = 200
	)
	for _, asyncRoutes := range []bool{false, true} {
		name := "no-async-routes"
		if asyncRoutes {
			name = "async-route-elsewhere"
		}
		t.Run(name, func(t *testing.T) {
			var hijacked atomic.Int64
			e, _, stop := startInlineHijackEngine(t, 2, inlineHijackHandler{asyncRoutes: asyncRoutes, hijacked: &hijacked})

			var reused atomic.Int64
			var mu sync.Mutex
			var taken []*connState
			hook := func(l *Loop, cs *connState) {
				if !ownsLoop(e, l) {
					return
				}
				got := takeReleasedConnState(l, cs)
				if got == nil {
					return
				}
				got.h1State = conn.NewH1State()
				got.h1State.InlineMode = true
				reused.Add(1)
				mu.Lock()
				taken = append(taken, got)
				mu.Unlock()
			}
			testHookHijackReleased.Store(&hook)
			defer testHookHijackReleased.Store(nil)

			rounds := 0
			for ; rounds < maxRounds && reused.Load() < wantReuse; rounds++ {
				client, fd := adoptable543(t)
				if err := e.AdoptConn(fd, engine.Carryover{
					RemoteAddr: client.LocalAddr().String(),
					Buffered:   []byte("GET /hj HTTP/1.1\r\nHost: x\r\n\r\n"),
				}); err != nil {
					t.Fatalf("round %d: AdoptConn: %v", rounds, err)
				}
				_ = client.SetDeadline(time.Now().Add(5 * time.Second))
				resp, err := http.ReadResponse(bufio.NewReader(client), nil)
				if err != nil {
					t.Fatalf("round %d: the hijacked carried request got no answer: %v", rounds, err)
				}
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if string(b) != "hj" {
					t.Fatalf("round %d: body %q, want %q", rounds, b, "hj")
				}
				_ = client.Close()
			}
			if !stop() {
				t.Fatal("the engine did not stop")
			}
			t.Logf("EPTSTHIJACK carry replay: rounds=%d reuse=%d (want >= %d)", rounds, reused.Load(), wantReuse)
			if reused.Load() < wantReuse {
				t.Fatalf("witness: the pool handed the released connState back %d times in %d rounds, want %d: no accept could have met the defect's conditions and this test shows nothing", reused.Load(), rounds, wantReuse)
			}
			if got := hijacked.Load(); got != int64(rounds) {
				t.Errorf("hijacked %d connections in %d rounds", got, rounds)
			}
			mu.Lock()
			defer mu.Unlock()
			cleared := 0
			for _, st := range taken {
				if !st.h1State.InlineMode {
					cleared++
				}
			}
			if cleared > 0 {
				t.Errorf("InlineMode was cleared on %d of %d reissued connStates after the hijack of a carried request: replayCarriedAsync wrote the parser state of a connection that is not the hijacked one", cleared, len(taken))
			}
			if n := e.Metrics().AsyncPromotedConns; n != 0 {
				t.Errorf("AsyncPromotedConns = %d, want 0: the replay of a hijacked request promoted a conn", n)
			}
		})
	}
}
