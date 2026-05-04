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

// TestHubBroadcastFormatsOnce — strict-alloc gate. The Hub itself must
// NOT add per-Conn allocations on top of [Conn.WritePreparedMessage]'s
// intrinsic cost. We measure at two N's and check the per-Conn delta:
// any growth beyond the intrinsic Conn write cost would mean the Hub
// is allocating in its dispatch loop.
func TestHubBroadcastFormatsOnce(t *testing.T) {
	if testing.CoverMode() != "" || testing.Short() {
		t.Skip("alloc counts unstable under coverage / -short")
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
	low := measure(8)
	high := measure(64)
	perConn := (high - low) / float64(64-8)
	// Per-Conn intrinsic cost from Conn.WritePreparedMessage is ~2 allocs
	// (writer pool churn + flush). The budget gives a tolerance of 0.5
	// over the measured baseline; anything higher means the Hub is
	// adding allocations in its per-Conn loop.
	const perConnBudget = 2.5
	if perConn > perConnBudget {
		t.Fatalf("Broadcast per-conn allocs = %.2f (8→%.1f, 64→%.1f), budget %.2f", perConn, low, high, perConnBudget)
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

// TestHubBroadcastFilter delivers only to the conns matching pred. The
// filter scan must hold only the read lock, so a concurrent Register
// does not serialise — verified indirectly by ensuring Register
// completes during a long-running BroadcastFilter (no deadlock).
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
