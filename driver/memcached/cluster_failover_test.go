package memcached

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

// closeFake closes the listener and all live connections of f, so
// subsequent dials to its address fail and existing idle connections
// return I/O errors on first use.
func closeFake(f *fakeMemcached) {
	_ = f.ln.Close()
	f.mu.Lock()
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func TestClusterFailoverSingleNode(t *testing.T) {
	fakes, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:               addrs,
		DialTimeout:         300 * time.Millisecond,
		FailureThreshold:    2,
		HealthProbeInterval: 0, // disable probe for this test
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	// Pick a key we know maps to fake[1].
	var target string
	for i := 0; i < 500; i++ {
		k := "key-" + timeNowHex(i)
		if cc.NodeFor(k) == fakes[1].Addr() {
			target = k
			break
		}
	}
	if target == "" {
		t.Fatal("couldn't find key mapping to middle node")
	}

	// Warm up the target — sets onto fake[1].
	if err := cc.Set(context.Background(), target, "v", time.Minute); err != nil {
		t.Fatalf("warm-up Set: %v", err)
	}

	// Kill the middle node.
	closeFake(fakes[1])

	// Trigger failure marking: two attempts should hit the dead node
	// and mark it failing (threshold=2).
	for i := 0; i < 4; i++ {
		_, _ = cc.GetBytes(context.Background(), target)
	}

	health := cc.NodeHealth()
	if !health[fakes[1].Addr()] {
		t.Fatalf("expected fake[1] to be marked failing, got %v", health)
	}

	// Subsequent ops on `target` now route to a healthy successor.
	// Since the successor's store is empty, we expect ErrCacheMiss —
	// which is NOT an infra error and should succeed at the network level.
	if err := cc.Set(context.Background(), target, "v2", time.Minute); err != nil {
		t.Fatalf("Set after failover: %v (should succeed via successor)", err)
	}
	if got, err := cc.Get(context.Background(), target); err != nil || got != "v2" {
		t.Fatalf("Get after failover: got=%q err=%v", got, err)
	}
}

func TestClusterFailoverKeyStability(t *testing.T) {
	fakes, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:               addrs,
		DialTimeout:         300 * time.Millisecond,
		FailureThreshold:    2,
		HealthProbeInterval: 0,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	// Identify all keys that originally route to fake[1].
	const total = 5000
	onMiddle := make([]string, 0, total/3)
	for i := 0; i < total; i++ {
		k := "key-" + timeNowHex(i)
		if cc.NodeFor(k) == fakes[1].Addr() {
			onMiddle = append(onMiddle, k)
		}
	}
	if len(onMiddle) < 100 {
		t.Fatalf("too few keys on middle node: %d", len(onMiddle))
	}

	// Mark fake[1] as failing directly (bypass the threshold for this test).
	cc.nodes[1].failing.Store(true)

	// Every key formerly on fake[1] should now map to the SAME successor.
	var successor string
	for _, k := range onMiddle {
		got := cc.NodeFor(k)
		if successor == "" {
			successor = got
		} else if got != successor {
			t.Fatalf("key stability broken: key=%q went to %q, expected %q",
				k, got, successor)
		}
		if got == fakes[1].Addr() {
			t.Fatalf("key %q still routing to failing node", k)
		}
	}
}

func TestClusterFailoverAllDown(t *testing.T) {
	fakes, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:               addrs,
		DialTimeout:         200 * time.Millisecond,
		HealthProbeInterval: 0,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	// Kill every node.
	for _, f := range fakes {
		closeFake(f)
	}
	for _, n := range cc.nodes {
		n.failing.Store(true)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cc.Get(context.Background(), "doomed")
	}()
	select {
	case <-done:
		// OK — returned some error without hanging.
	case <-time.After(2 * time.Second):
		t.Fatal("Get hung with all nodes failing")
	}
}

func TestClusterFailoverHealthProbeRecovery(t *testing.T) {
	fakes, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:               addrs,
		DialTimeout:         300 * time.Millisecond,
		FailureThreshold:    2,
		HealthProbeInterval: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	// Mark fake[1] as failing directly (simulate recent failure).
	cc.nodes[1].failing.Store(true)
	cc.nodes[1].lastFailAt.Store(time.Now().Add(-2 * time.Second).UnixNano())

	// fake[1] is still alive — probe should clear the flag within a few
	// HealthProbeInterval ticks.
	_ = fakes
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !cc.NodeHealth()[cc.nodes[1].addr] {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("probe did not clear failing flag within deadline")
}

func TestClusterFailureThresholdHysteresis(t *testing.T) {
	fakes, addrs := startFakeCluster(t, 1)
	_ = fakes
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:               addrs,
		DialTimeout:         200 * time.Millisecond,
		FailureThreshold:    2,
		HealthProbeInterval: 0,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	n := cc.nodes[0]

	// Single infra error must NOT mark failing.
	n.recordResult(context.Canceled, cc.failureThreshold) // not infra
	if n.failing.Load() {
		t.Fatal("context.Canceled should not count as infra error")
	}
	// Two real infra errors → mark.
	n.recordResult(errFakeInfra, cc.failureThreshold)
	if n.failing.Load() {
		t.Fatal("single infra error should not trip threshold=2")
	}
	n.recordResult(errFakeInfra, cc.failureThreshold)
	if !n.failing.Load() {
		t.Fatal("two infra errors should trip threshold=2")
	}
	// Passive healing on next success.
	n.recordResult(nil, cc.failureThreshold)
	if n.failing.Load() {
		t.Fatal("success should clear failing flag (passive heal)")
	}
	if n.consecutiveFails.Load() != 0 {
		t.Fatalf("consecutiveFails should reset; got %d", n.consecutiveFails.Load())
	}
}

func TestClusterFailoverRace(t *testing.T) {
	_, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:               addrs,
		DialTimeout:         300 * time.Millisecond,
		FailureThreshold:    2,
		HealthProbeInterval: 0,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	const goroutines = 16
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Flipper: toggles failing flag on node 1.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				cc.nodes[1].failing.Store(i%2 == 0)
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Hammer ops.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := "race-" + timeNowHex(id)
			for {
				select {
				case <-stop:
					return
				default:
					_ = cc.Set(context.Background(), key, "v", time.Minute)
					_, _ = cc.Get(context.Background(), key)
				}
			}
		}(i)
	}
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestClusterFailoverLifecycle(t *testing.T) {
	_, addrs := startFakeCluster(t, 2)
	before := runtime.NumGoroutine()

	cc, err := NewClusterClient(ClusterConfig{
		Addrs:               addrs,
		HealthProbeInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	// Goroutine should be alive now.
	time.Sleep(20 * time.Millisecond)
	if err := cc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Give the runtime a moment to finalize.
	time.Sleep(30 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+1 { // allow small variance
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}
}

// Benchmark pickNode on all-healthy, one-failing, and two-failing cases.
// These should show zero allocations on all three paths since pickNode
// returns a pointer to a node slot — no new memory allocated.

func BenchmarkClusterPickNodeAllHealthy(b *testing.B) {
	_, addrs := startFakeClusterB(b, 3)
	cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, HealthProbeInterval: 0})
	if err != nil {
		b.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()
	key := "some-benchmark-key"
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = cc.pickNode(key)
	}
}

func BenchmarkClusterPickNodeOneFailing(b *testing.B) {
	_, addrs := startFakeClusterB(b, 3)
	cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, HealthProbeInterval: 0})
	if err != nil {
		b.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	// Find a key that routes to node[0], then mark it failing.
	key := "some-benchmark-key"
	for i := 0; ; i++ {
		if cc.pickNode(key) == cc.nodes[0] {
			break
		}
		key = "some-benchmark-key-" + timeNowHex(i)
	}
	cc.nodes[0].failing.Store(true)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = cc.pickNode(key)
	}
}

func startFakeClusterB(b *testing.B, n int) (fakes []*fakeMemcached, addrs []string) {
	b.Helper()
	fakes = make([]*fakeMemcached, n)
	addrs = make([]string, n)
	for i := 0; i < n; i++ {
		fakes[i] = startFakeB(b)
		addrs[i] = fakes[i].Addr()
	}
	return fakes, addrs
}

// --- helpers ---

// errFakeInfra is a sentinel that mimics the shape of a real network
// error for recordResult testing (not a protocol error).
var errFakeInfra = fakeInfraErr{}

type fakeInfraErr struct{}

func (fakeInfraErr) Error() string { return "fake infra error" }

// timeNowHex returns a short hex-ish string derived from the process
// and i so tests can mint many distinct keys without stdlib iteration.
func timeNowHex(i int) string {
	var b [8]byte
	u := uint64(time.Now().UnixNano()) ^ uint64(i)<<32
	for k := 0; k < 8; k++ {
		b[k] = "0123456789abcdef"[u&0xf]
		u >>= 4
	}
	return string(b[:])
}
