package async

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeConn struct {
	worker   int
	closed   atomic.Bool
	created  time.Time
	lastUsed time.Time
	maxLife  time.Duration
	maxIdle  time.Duration
	pingErr  error
}

func (c *fakeConn) Ping(ctx context.Context) error {
	return c.pingErr
}
func (c *fakeConn) Close() error {
	c.closed.Store(true)
	return nil
}
func (c *fakeConn) Worker() int { return c.worker }
func (c *fakeConn) IsExpired(now time.Time) bool {
	if c.maxLife <= 0 {
		return false
	}
	return now.Sub(c.created) >= c.maxLife
}
func (c *fakeConn) IsIdleTooLong(now time.Time) bool {
	if c.maxIdle <= 0 {
		return false
	}
	return now.Sub(c.lastUsed) >= c.maxIdle
}

func newFake(w int) *fakeConn {
	now := time.Now()
	return &fakeConn{worker: w, created: now, lastUsed: now}
}

func TestPoolAcquireRelease(t *testing.T) {
	cfg := PoolConfig{NumWorkers: 2, MaxOpen: 4, MaxIdlePerWorker: 2}
	dials := int32(0)
	p := NewPool(cfg, func(ctx context.Context, w int) (*fakeConn, error) {
		atomic.AddInt32(&dials, 1)
		return newFake(w), nil
	})
	defer p.Close()

	c, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.Worker() != 0 {
		t.Fatalf("worker=%d, want 0", c.Worker())
	}
	if atomic.LoadInt32(&dials) != 1 {
		t.Fatalf("dials=%d, want 1", dials)
	}
	p.Release(c)

	c2, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if c2 != c {
		t.Fatalf("expected conn reuse")
	}
	if atomic.LoadInt32(&dials) != 1 {
		t.Fatalf("dials=%d, want still 1", dials)
	}
	p.Release(c2)
}

func TestPoolMaxOpen(t *testing.T) {
	cfg := PoolConfig{NumWorkers: 1, MaxOpen: 2}
	p := NewPool(cfg, func(ctx context.Context, w int) (*fakeConn, error) {
		return newFake(w), nil
	})
	defer p.Close()

	c1, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}

	// Third Acquire blocks; with a short deadline it returns ctx.Err().
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = p.Acquire(ctx, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	p.Release(c1)
	p.Release(c2)

	// After release, Acquire must succeed.
	c3, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	p.Release(c3)
}

func TestPoolCrossWorkerFallback(t *testing.T) {
	cfg := PoolConfig{NumWorkers: 2, MaxOpen: 4, MaxIdlePerWorker: 4}
	p := NewPool(cfg, func(ctx context.Context, w int) (*fakeConn, error) {
		return newFake(w), nil
	})
	defer p.Close()

	c, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	p.Release(c)

	// Request worker 1; there is none idle on worker 1 but worker 0 has
	// an idle conn — pool should reuse it.
	c2, err := p.Acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if c2 != c {
		t.Fatalf("expected cross-worker reuse")
	}
	p.Release(c2)
}

func TestPoolReleaseEvictsExpired(t *testing.T) {
	cfg := PoolConfig{NumWorkers: 1, MaxOpen: 2, MaxIdlePerWorker: 2}
	p := NewPool(cfg, func(ctx context.Context, w int) (*fakeConn, error) {
		c := newFake(w)
		c.maxLife = time.Nanosecond
		time.Sleep(time.Millisecond)
		return c, nil
	})
	defer p.Close()

	c, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	p.Release(c)
	if !c.closed.Load() {
		t.Fatalf("expired conn not closed on release")
	}
	stats := p.Stats()
	if stats.Open != 0 {
		t.Fatalf("open=%d after evict, want 0", stats.Open)
	}
}

func TestPoolClose(t *testing.T) {
	cfg := PoolConfig{NumWorkers: 1, MaxOpen: 2, MaxIdlePerWorker: 2}
	p := NewPool(cfg, func(ctx context.Context, w int) (*fakeConn, error) {
		return newFake(w), nil
	})
	c, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	p.Release(c)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if !c.closed.Load() {
		t.Fatalf("close did not close idle conn")
	}
	if _, err := p.Acquire(context.Background(), 0); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("want ErrPoolClosed, got %v", err)
	}
}

func TestPoolIdleCap(t *testing.T) {
	cfg := PoolConfig{NumWorkers: 1, MaxOpen: 10, MaxIdlePerWorker: 1}
	p := NewPool(cfg, func(ctx context.Context, w int) (*fakeConn, error) {
		return newFake(w), nil
	})
	defer p.Close()

	c1, _ := p.Acquire(context.Background(), 0)
	c2, _ := p.Acquire(context.Background(), 0)
	p.Release(c1)
	p.Release(c2)
	// Second release exceeds idle cap -> closed.
	if !c2.closed.Load() {
		t.Fatalf("expected second release to close; idle cap=1")
	}
	stats := p.Stats()
	if stats.Idle != 1 {
		t.Fatalf("idle=%d, want 1", stats.Idle)
	}
}

func TestPoolStatsShape(t *testing.T) {
	cfg := PoolConfig{NumWorkers: 3, MaxOpen: 10, MaxIdlePerWorker: 2}
	p := NewPool(cfg, func(ctx context.Context, w int) (*fakeConn, error) {
		return newFake(w), nil
	})
	defer p.Close()
	s := p.Stats()
	if len(s.PerWorker) != 3 {
		t.Fatalf("per-worker len=%d, want 3", len(s.PerWorker))
	}
}

func TestPoolHealthCheckEvicts(t *testing.T) {
	cfg := PoolConfig{
		NumWorkers:       1,
		MaxOpen:          4,
		MaxIdlePerWorker: 4,
		HealthCheck:      20 * time.Millisecond,
	}
	p := NewPool(cfg, func(ctx context.Context, w int) (*fakeConn, error) {
		c := newFake(w)
		c.pingErr = errors.New("bad")
		return c, nil
	})
	defer p.Close()
	c, _ := p.Acquire(context.Background(), 0)
	p.Release(c)
	// Wait for ~3 health ticks.
	time.Sleep(100 * time.Millisecond)
	if !c.closed.Load() {
		t.Fatalf("health check did not evict bad conn")
	}
	if p.Stats().Open != 0 {
		t.Fatalf("open=%d after eviction", p.Stats().Open)
	}
}

func TestPoolWaitQueue(t *testing.T) {
	cfg := PoolConfig{NumWorkers: 1, MaxOpen: 2, MaxIdlePerWorker: 2}
	p := NewPool(cfg, func(ctx context.Context, w int) (*fakeConn, error) {
		return newFake(w), nil
	})
	defer p.Close()

	c1, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}

	// Third Acquire blocks because both slots are in use.
	got := make(chan *fakeConn, 1)
	go func() {
		c, err := p.Acquire(context.Background(), 0)
		if err != nil {
			t.Errorf("waiter Acquire: %v", err)
			return
		}
		got <- c
	}()

	// Give the goroutine time to park on the semaphore.
	time.Sleep(20 * time.Millisecond)
	select {
	case <-got:
		t.Fatal("third Acquire returned before any release")
	default:
	}

	// Release one conn — the blocked Acquire should proceed.
	p.Release(c1)

	select {
	case c3 := <-got:
		p.Release(c3)
	case <-time.After(2 * time.Second):
		t.Fatal("third Acquire did not unblock after release")
	}
	p.Release(c2)
}

func TestPoolWaitQueueTimeout(t *testing.T) {
	cfg := PoolConfig{NumWorkers: 1, MaxOpen: 1}
	p := NewPool(cfg, func(ctx context.Context, w int) (*fakeConn, error) {
		return newFake(w), nil
	})
	defer p.Close()

	c, err := p.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = p.Acquire(ctx, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	p.Release(c)
}

func TestPoolWaitQueueUnlimited(t *testing.T) {
	cfg := PoolConfig{NumWorkers: 2, MaxOpen: 0, MaxIdlePerWorker: 10}
	p := NewPool(cfg, func(ctx context.Context, w int) (*fakeConn, error) {
		return newFake(w), nil
	})
	defer p.Close()

	const N = 100
	conns := make([]*fakeConn, N)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := p.Acquire(context.Background(), -1)
			if err != nil {
				t.Errorf("Acquire %d: %v", i, err)
				return
			}
			mu.Lock()
			conns[i] = c
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	for _, c := range conns {
		if c != nil {
			p.Release(c)
		}
	}
}
