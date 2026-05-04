package websocket

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hubPair is one (Conn, drain) pair: a *Conn ready for Hub registration
// and a goroutine draining the client end of the net.Pipe so writes do
// not stall on the unbuffered pipe.
type hubPair struct {
	conn   *Conn
	client net.Conn
	stop   chan struct{}
	done   chan struct{}
}

func (p *hubPair) closeAll() {
	close(p.stop)
	<-p.done
	_ = p.client.Close()
	_ = p.conn.Close()
}

// newHubPair creates a server-side *Conn whose writes are drained by a
// background goroutine on the client end. Mirrors the closeduringwrite
// test harness pattern (see closeduringwrite_test.go:18).
func newHubPair(t *testing.T) *hubPair {
	t.Helper()
	clientPipe, serverPipe := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	srv := newConn(ctx, cancel, serverPipe, 1024, 1024)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = clientPipe.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			_, err := clientPipe.Read(buf)
			if err != nil {
				if err == io.EOF {
					return
				}
				if isTimeout(err) {
					continue
				}
				return
			}
		}
	}()
	return &hubPair{conn: srv, client: clientPipe, stop: stop, done: done}
}

// TestHubRegisterUnregister — basic Register/unregister ergonomics. The
// returned unregister func is idempotent (issue exit criterion).
func TestHubRegisterUnregister(t *testing.T) {
	h := NewHub(HubConfig{})
	defer h.Close()

	p := newHubPair(t)
	defer p.closeAll()

	if h.Len() != 0 {
		t.Errorf("fresh hub Len = %d, want 0", h.Len())
	}
	unreg := h.Register(p.conn)
	if h.Len() != 1 {
		t.Errorf("after Register Len = %d, want 1", h.Len())
	}
	unreg()
	if h.Len() != 0 {
		t.Errorf("after unregister Len = %d, want 0", h.Len())
	}
	unreg() // idempotent — must not panic
}

// TestHubBroadcastReachesAllConns delivers a single message to N conns
// and asserts the delivered count + every conn unaffected.
func TestHubBroadcastReachesAllConns(t *testing.T) {
	const n = 16
	h := NewHub(HubConfig{})
	defer h.Close()

	pairs := make([]*hubPair, n)
	for i := range pairs {
		pairs[i] = newHubPair(t)
		h.Register(pairs[i].conn)
	}
	defer func() {
		for _, p := range pairs {
			p.closeAll()
		}
	}()

	delivered, err := h.Broadcast(OpText, []byte("hello"))
	if err != nil {
		t.Fatalf("Broadcast err = %v", err)
	}
	if delivered != n {
		t.Errorf("delivered = %d, want %d", delivered, n)
	}
}

// TestHubBroadcastFormatsOnce — strict-alloc gate. The Hub must NOT
// add per-Conn allocations on top of [Conn.WritePreparedMessage]'s
// intrinsic cost, AND must keep its own per-broadcast overhead below a
// hard ceiling (snapshot slice + per-call wgAdd + a few words).
//
// We measure at two N's and pin two separate properties:
//
//	hubOverhead = allocs(N=1) − 1 × intrinsic_per_conn
//	perConnΔ    = (allocs(N=64) − allocs(N=8)) / 56
//
// Bounding both is what catches a regression: tightening only the
// slope hides any constant per-broadcast alloc growth, and tightening
// only the intercept hides per-conn growth.
func TestHubBroadcastFormatsOnce(t *testing.T) {
	if raceEnabled || testing.CoverMode() != "" || testing.Short() {
		t.Skip("alloc counts unstable under -race / coverage / -short")
	}
	measure := func(n int) float64 {
		h := NewHub(HubConfig{})
		defer h.Close()
		pairs := make([]*hubPair, n)
		for i := range pairs {
			pairs[i] = newHubPair(t)
			h.Register(pairs[i].conn)
		}
		defer func() {
			for _, p := range pairs {
				p.closeAll()
			}
		}()
		pm, _ := NewPreparedMessage(OpText, []byte("x"))
		return testing.AllocsPerRun(20, func() {
			_, _ = h.BroadcastPrepared(pm)
		})
	}
	one := measure(1)
	low := measure(8)
	high := measure(64)

	// Intrinsic Conn.WritePreparedMessage cost: derive from the slope
	// between (1,8) — they share whatever per-broadcast overhead the
	// Hub adds, so the slope cleanly extracts the per-Conn cost.
	intrinsic := (low - one) / float64(8-1)
	hubOverhead := one - intrinsic // per-broadcast cost at N=1
	perConnDelta := (high - low) / float64(64-8)

	// Hub itself: 1 alloc for the snapshot slice + 1 for the
	// PreparedMessage cache miss = effectively 2 in the worst case.
	// Anything above 4 means a regression introduced new per-broadcast
	// allocations.
	const hubOverheadBudget = 4.0
	if hubOverhead > hubOverheadBudget {
		t.Fatalf("hub per-broadcast overhead = %.2f (intrinsic=%.2f, N=1→%.1f), budget %.2f", hubOverhead, intrinsic, one, hubOverheadBudget)
	}
	// Per-Conn cost must not grow with N. perConnDelta should match
	// intrinsic within 0.25 alloc/op of slack.
	if perConnDelta-intrinsic > 0.25 {
		t.Fatalf("per-conn allocs grow with N: intrinsic=%.2f, slope(8→64)=%.2f", intrinsic, perConnDelta)
	}
}

// TestHubOnSlowConnPolicies — slow conn drives the policy hook through
// each of its three values. Slow simulation: use a net.Pipe pair where
// the client side is NEVER drained, so writes time out immediately
// after we set a tight deadline.
func TestHubOnSlowConnPolicies(t *testing.T) {
	cases := []struct {
		name        string
		policy      HubPolicy
		expectInHub bool
		expectClose bool
	}{
		{"drop keeps registered", HubPolicyDrop, true, false},
		{"remove unregisters", HubPolicyRemove, false, false},
		{"close unregisters and closes", HubPolicyClose, false, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h := NewHub(HubConfig{
				OnSlowConn: func(_ *Conn, _ error) HubPolicy { return tc.policy },
			})
			defer h.Close()

			// Slow conn: no drain on the client side; net.Pipe is
			// unbuffered, so any write that exceeds the WriteDeadline
			// fails. Register; arm a tight write deadline; broadcast.
			clientPipe, serverPipe := net.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			slow := newConn(ctx, cancel, serverPipe, 1024, 1024)
			defer func() { _ = clientPipe.Close() }()
			defer func() { _ = slow.Close() }()
			_ = slow.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))

			h.Register(slow)
			_, err := h.Broadcast(OpText, []byte("x"))
			if err == nil {
				t.Errorf("expected per-Conn error for slow conn, got nil")
			}

			inHub := h.Len() == 1
			if inHub != tc.expectInHub {
				t.Errorf("policy %v: in-hub = %v, want %v", tc.policy, inHub, tc.expectInHub)
			}
			if tc.expectClose && !slow.closed.Load() {
				t.Errorf("policy %v: conn not closed", tc.policy)
			}
		})
	}
}

// TestHubBroadcastFilter delivers only to the conns matching pred.
// The membership snapshot is taken under the read lock; pred itself
// runs lock-free against the snapshot.
func TestHubBroadcastFilter(t *testing.T) {
	h := NewHub(HubConfig{})
	defer h.Close()

	a := newHubPair(t)
	b := newHubPair(t)
	defer a.closeAll()
	defer b.closeAll()

	a.conn.SetLocals("room", "general")
	b.conn.SetLocals("room", "off-topic")
	h.Register(a.conn)
	h.Register(b.conn)

	delivered, err := h.BroadcastFilter(OpText, []byte("hi"), func(c *Conn) bool {
		return c.Locals("room") == "general"
	})
	if err != nil {
		t.Fatalf("BroadcastFilter err = %v", err)
	}
	if delivered != 1 {
		t.Errorf("delivered = %d, want 1", delivered)
	}
}

// TestHubBroadcastFilterZeroMatch — pred matching nobody returns
// (delivered=0, err=nil). Pinned because a "no matches" path that
// returns an error or panics would surface as a silent regression.
func TestHubBroadcastFilterZeroMatch(t *testing.T) {
	h := NewHub(HubConfig{})
	defer h.Close()

	a := newHubPair(t)
	defer a.closeAll()
	h.Register(a.conn)

	delivered, err := h.BroadcastFilter(OpText, []byte("x"), func(*Conn) bool { return false })
	if err != nil {
		t.Errorf("BroadcastFilter err = %v, want nil", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0", delivered)
	}
}

// TestHubOneSlowMany Fast — issue #253 spec: a single slow conn must
// not block the dispatch to the rest. Slow conn has a tight write
// deadline; fast conns drain normally. Default policy
// (HubPolicyClose) evicts the slow conn; fast conns all see the
// message in roughly the slow-conn timeout window.
func TestHubOneSlowManyFast(t *testing.T) {
	const fastN = 32 // small enough to keep the test fast; the issue
	// spec calls out 1+999, but the property we want — slow-doesn't-
	// gate-fast — is testable at any N where the wall time of fast
	// dispatches is bounded above by the slow timeout.
	h := NewHub(HubConfig{})
	defer h.Close()

	// Fast cohort: drained pipes.
	fast := make([]*hubPair, fastN)
	for i := range fast {
		fast[i] = newHubPair(t)
		h.Register(fast[i].conn)
	}
	defer func() {
		for _, p := range fast {
			p.closeAll()
		}
	}()

	// Slow conn: client side is NOT drained; net.Pipe is unbuffered
	// so any write that exceeds the deadline fails fast.
	clientPipe, serverPipe := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slow := newConn(ctx, cancel, serverPipe, 1024, 1024)
	defer func() { _ = clientPipe.Close() }()
	defer func() { _ = slow.Close() }()
	_ = slow.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
	h.Register(slow)

	start := time.Now()
	delivered, err := h.Broadcast(OpText, []byte("payload"))
	elapsed := time.Since(start)

	if err == nil {
		t.Errorf("expected first-error to surface from slow conn, got nil")
	}
	if delivered != fastN {
		t.Errorf("delivered = %d, want %d (fastN; slow conn evicted)", delivered, fastN)
	}
	// The whole broadcast must finish within ~slow-deadline + slack;
	// if a regression were to serialise dispatch behind the slow
	// write we'd see far more than the deadline here.
	if elapsed > 500*time.Millisecond {
		t.Errorf("broadcast wall = %v exceeds slow-deadline ceiling — fast cohort may have been gated", elapsed)
	}
	// Default HubPolicyClose evicts the slow conn.
	if h.Len() != fastN {
		t.Errorf("Len after slow eviction = %d, want %d", h.Len(), fastN)
	}
}

// TestHubCloseRacingBroadcast spawns broadcasters that race a Close.
// The Close ordering guarantee says any Broadcast that already
// snapshotted the conn set runs to completion; subsequent broadcasts
// return (0, nil). Run with -race.
func TestHubCloseRacingBroadcast(t *testing.T) {
	h := NewHub(HubConfig{})

	const fastN = 16
	pairs := make([]*hubPair, fastN)
	for i := range pairs {
		pairs[i] = newHubPair(t)
		h.Register(pairs[i].conn)
	}
	defer func() {
		for _, p := range pairs {
			p.closeAll()
		}
	}()

	const broadcasters = 8
	var wg sync.WaitGroup
	wg.Add(broadcasters)
	for range broadcasters {
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = h.Broadcast(OpText, []byte("racey"))
			}
		}()
	}
	time.Sleep(2 * time.Millisecond)
	h.Close()
	wg.Wait()

	delivered, err := h.Broadcast(OpText, []byte("after-close"))
	if err != nil {
		t.Errorf("Broadcast after Close err = %v, want nil", err)
	}
	if delivered != 0 {
		t.Errorf("Broadcast after Close delivered = %d, want 0", delivered)
	}
}

// TestHubRegisterUnregisterRace exercises 32 concurrent Register +
// Unregister + Broadcast cycles to flush out lock-ordering bugs. Run
// with -race.
func TestHubRegisterUnregisterRace(t *testing.T) {
	const workers = 32
	const iters = 50

	h := NewHub(HubConfig{})
	defer h.Close()

	pairs := make([]*hubPair, workers)
	for i := range pairs {
		pairs[i] = newHubPair(t)
	}
	defer func() {
		for _, p := range pairs {
			p.closeAll()
		}
	}()

	var wg sync.WaitGroup
	wg.Add(workers)
	var attempts atomic.Uint64
	for i := range workers {
		go func(p *hubPair) {
			defer wg.Done()
			for range iters {
				unreg := h.Register(p.conn)
				_, _ = h.Broadcast(OpText, []byte("x"))
				unreg()
				attempts.Add(1)
			}
		}(pairs[i])
	}
	wg.Wait()
	if attempts.Load() != workers*iters {
		t.Errorf("attempts = %d, want %d", attempts.Load(), workers*iters)
	}
	if h.Len() != 0 {
		t.Errorf("Len after race = %d, want 0", h.Len())
	}
}

// TestHubCloseIdempotent + TestHubRegisterAfterClose pin the terminal
// state semantics from the issue spec.
func TestHubCloseIdempotent(t *testing.T) {
	h := NewHub(HubConfig{})
	h.Close()
	h.Close()
	if h.Len() != 0 {
		t.Errorf("Len after Close = %d, want 0", h.Len())
	}
}

func TestHubRegisterAfterClose(t *testing.T) {
	h := NewHub(HubConfig{})
	h.Close()

	p := newHubPair(t)
	defer p.closeAll()
	unreg := h.Register(p.conn)
	if h.Len() != 0 {
		t.Errorf("Register after Close registered the Conn")
	}
	unreg()
}

// TestHubBroadcastErrSurface — when a Conn write fails for an unrelated
// reason (already closed), Broadcast returns the first error. Pinning
// this matters because metric collectors rely on err propagation.
func TestHubBroadcastErrSurface(t *testing.T) {
	h := NewHub(HubConfig{})
	defer h.Close()

	p := newHubPair(t)
	h.Register(p.conn)
	_ = p.conn.Close() // pre-close so WritePreparedMessage returns ErrWriteClosed

	_, err := h.Broadcast(OpText, []byte("x"))
	if !errors.Is(err, ErrWriteClosed) {
		t.Errorf("Broadcast err = %v, want ErrWriteClosed", err)
	}
	p.closeAll()
}

// BenchmarkWSHubBroadcast100 / 1000 — exit-criterion benches for the
// matrix integration. PreparedMessage cost is amortised across all
// conns in a single broadcast.
func BenchmarkWSHubBroadcast100(b *testing.B) { benchmarkHubBroadcast(b, 100) }
func BenchmarkWSHubBroadcast1000(b *testing.B) {
	benchmarkHubBroadcast(b, 1000)
}

func benchmarkHubBroadcast(b *testing.B, n int) {
	b.Helper()
	h := NewHub(HubConfig{})
	defer h.Close()

	type bench struct {
		conn   *Conn
		client net.Conn
		stop   chan struct{}
	}
	pairs := make([]bench, n)
	for i := range pairs {
		clientPipe, serverPipe := net.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		srv := newConn(ctx, cancel, serverPipe, 1024, 1024)
		stop := make(chan struct{})
		go func() {
			buf := make([]byte, 4096)
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = clientPipe.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
				_, _ = clientPipe.Read(buf)
			}
		}()
		pairs[i] = bench{conn: srv, client: clientPipe, stop: stop}
		h.Register(srv)
	}
	defer func() {
		for _, p := range pairs {
			close(p.stop)
			_ = p.client.Close()
			_ = p.conn.Close()
		}
	}()

	pm, _ := NewPreparedMessage(OpText, []byte("payload"))
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = h.BroadcastPrepared(pm)
	}
}
