//go:build redis

package redis_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

// panicOf runs f and returns what it panicked with (nil when it did not).
func panicOf(f func()) (pv any) {
	defer func() { pv = recover() }()
	f()
	return nil
}

// resultErrors reads Result() off every typed handle and reports a panic or a
// missing error. wantErr says whether every handle must report an error.
func resultErrors(t *testing.T, what string, handles map[string]func() error) {
	t.Helper()
	for name, read := range handles {
		var err error
		if pv := panicOf(func() { err = read() }); pv != nil {
			t.Errorf("%s: %s.Result() panicked: %v", what, name, pv)
			continue
		}
		if err == nil {
			t.Errorf("%s: %s.Result() returned a nil error for a command that never ran", what, name)
		}
	}
}

func TestPipeline_ResultBeforeExec_ReturnsError(t *testing.T) {
	c := connectClient(t)
	p := c.Pipeline()
	defer p.Release()
	key := uniqueKey(t, "k")
	g := p.Get(key)
	s := p.Set(key, "v", 0)
	i := p.Incr(key + ":n")
	b := p.Expire(key, time.Minute)
	resultErrors(t, "before Exec", map[string]func() error{
		"StringCmd": func() error { _, e := g.Result(); return e },
		"StatusCmd": func() error { _, e := s.Result(); return e },
		"IntCmd":    func() error { _, e := i.Result(); return e },
		"BoolCmd":   func() error { _, e := b.Result(); return e },
	})
}

func TestPipeline_ResultAfterFailedExec_ReturnsError(t *testing.T) {
	c := connectClient(t)
	p := c.Pipeline()
	key := uniqueKey(t, "k")
	g := p.Get(key)
	i := p.Incr(key + ":n")
	_ = c.Close() // every later Exec fails to acquire a conn
	if err := p.Exec(ctxWithTimeout(t, 3*time.Second)); err == nil {
		t.Fatal("Exec on a closed client succeeded")
	}
	resultErrors(t, "after a failed Exec", map[string]func() error{
		"StringCmd": func() error { _, e := g.Result(); return e },
		"IntCmd":    func() error { _, e := i.Result(); return e },
	})
}

func TestTx_ResultBeforeExec_ReturnsError(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	tx, err := c.TxPipeline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Discard() }()
	key := uniqueKey(t, "k")
	g := tx.Get(key)
	s := tx.Set(key, "v", 0)
	i := tx.Incr(key + ":n")
	b := tx.Expire(key, time.Minute)
	resultErrors(t, "Tx before Exec", map[string]func() error{
		"StringCmd": func() error { _, e := g.Result(); return e },
		"StatusCmd": func() error { _, e := s.Result(); return e },
		"IntCmd":    func() error { _, e := i.Result(); return e },
		"BoolCmd":   func() error { _, e := b.Result(); return e },
	})
}

func TestTx_EnqueueAfterExec_ReturnsError(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "k")
	cleanupKeys(t, c, key, key+":late")
	tx, err := c.TxPipeline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	set := tx.Set(key, "v", 0)
	if err := tx.Exec(ctx); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if _, err := set.Result(); err != nil {
		t.Fatalf("set.Result: %v", err)
	}
	late := tx.Set(key+":late", "x", 0)
	lateGet := tx.Get(key)
	resultErrors(t, "Tx enqueue after Exec", map[string]func() error{
		"late StatusCmd": func() error { _, e := late.Result(); return e },
		"late StringCmd": func() error { _, e := lateGet.Result(); return e },
	})
	if _, err := c.Get(ctx, key+":late"); err == nil {
		t.Fatal("a command enqueued after Exec was written")
	}
	if st := c.Stats(); st.InUse != 0 {
		t.Fatalf("conn still pinned after Exec: %+v", st)
	}
}

func TestPipeline_ExecTwice_ReturnsError(t *testing.T) {
	c := connectClient(t)
	key := uniqueKey(t, "k")
	cleanupKeys(t, c, key)
	if err := c.Set(ctxWithTimeout(t, 5*time.Second), key, "v", 0); err != nil {
		t.Fatal(err)
	}
	p := c.Pipeline()
	defer p.Release()
	g := p.Get(key)
	if err := p.Exec(context.Background()); err != nil {
		t.Fatalf("first Exec: %v", err)
	}
	// context.Background: today the second Exec waits for replies that were
	// never requested, forever, holding a pool conn.
	done := make(chan error, 1)
	go func() { done <- p.Exec(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a second Exec returned nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("a second Exec blocked for 3s (pool %+v)", c.Stats())
	}
	if st := c.Stats(); st.InUse != 0 {
		t.Fatalf("second Exec pinned a conn: %+v", st)
	}
	// The first Exec's results stay readable.
	if v, err := g.Result(); err != nil || v != "v" {
		t.Fatalf("first Exec's result = %q, %v; want v", v, err)
	}
}

func TestPipeline_ExecAfterDiscard_StillReusable(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "k")
	cleanupKeys(t, c, key)
	p := c.Pipeline()
	defer p.Release()
	p.Set(key, "1", 0)
	if err := p.Exec(ctx); err != nil {
		t.Fatal(err)
	}
	p.Discard()
	p.Set(key, "2", 0)
	g := p.Get(key)
	if err := p.Exec(ctx); err != nil {
		t.Fatalf("Exec after Discard: %v", err)
	}
	if v, err := g.Result(); err != nil || v != "2" {
		t.Fatalf("GET = %q, %v; want 2", v, err)
	}
}

// TestClient_Watch_NoUseAfterRelease drives many concurrent Watch calls: the
// WATCH reply's error used to be read after the request went back to its
// pool, where the next command overwrites it (a data race, and a WATCH error
// that could be missed or foreign).
func TestClient_Watch_NoUseAfterRelease(t *testing.T) {
	c := connectClient(t)
	key := uniqueKey(t, "w")
	keys := []string{key}
	for g := 0; g < 8; g++ {
		keys = append(keys, fmt.Sprintf("%s:%d", key, g))
	}
	cleanupKeys(t, c, keys...)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := c.Watch(ctx, func(tx *celredis.Tx) error { return nil }, key)
				if err == nil {
					_, err = c.Incr(ctx, fmt.Sprintf("%s:%d", key, g))
				}
				cancel()
				if err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Watch/Incr: %v", err)
	}
}
