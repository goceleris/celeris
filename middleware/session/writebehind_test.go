package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/internal/testutil"
	"github.com/goceleris/celeris/middleware/store"
)

// gateStore wraps a store.KV and blocks every Set until released, so a test
// can prove the post-handler write did or did not complete before the
// response returned.
type gateStore struct {
	inner   store.KV
	gate    chan struct{} // Set blocks on receive until released
	sets    atomic.Int64  // count of Set calls that have STARTED
	setDone atomic.Int64  // count of Set calls that have COMPLETED
}

func newGateStore() *gateStore {
	return &gateStore{inner: NewMemoryStore(), gate: make(chan struct{})}
}

func (g *gateStore) Get(ctx context.Context, id string) ([]byte, error) {
	return g.inner.Get(ctx, id)
}

func (g *gateStore) Set(ctx context.Context, id string, data []byte, exp time.Duration) error {
	g.sets.Add(1)
	<-g.gate
	err := g.inner.Set(ctx, id, data, exp)
	g.setDone.Add(1)
	return err
}

func (g *gateStore) Delete(ctx context.Context, id string) error {
	return g.inner.Delete(ctx, id)
}

// release unblocks all currently-blocked and future Set calls.
func (g *gateStore) release() { close(g.gate) }

// TestWriteBehindDefaultOffIsSynchronous proves the default (flag off) write
// is on the critical path: the response goroutine does not return until the
// store Set has completed.
func TestWriteBehindDefaultOffIsSynchronous(t *testing.T) {
	gs := newGateStore()
	gs.release()                                   // never block; we only assert completion ordering
	mw, closer := NewWithCloser(Config{Store: gs}) // WriteBehind defaults to false
	defer func() { _ = closer.Close() }()

	handler := func(c *celeris.Context) error {
		FromContext(c).Set("k", "v")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)

	// With the flag off, the write must already be durable the instant the
	// chain returns — no flush required.
	if gs.setDone.Load() != 1 {
		t.Fatalf("expected synchronous write (1 completed Set) before return, got %d", gs.setDone.Load())
	}
}

// TestWriteBehindDeferredOffCriticalPath proves that with WriteBehind on, the
// response returns BEFORE the store Set completes (the write is deferred),
// and that Close flushes it so no write is lost.
func TestWriteBehindDeferredOffCriticalPath(t *testing.T) {
	gs := newGateStore() // Set blocks until release()
	mw, closer := NewWithCloser(Config{Store: gs, WriteBehind: true})

	var sid string
	handler := func(c *celeris.Context) error {
		s := FromContext(c)
		s.Set("count", 7)
		sid = s.ID()
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	testutil.AssertNoError(t, err)

	// The response returned. The deferred Set may have STARTED (it's blocked
	// on the gate) but must NOT have completed — proving it was off the
	// critical path.
	if gs.setDone.Load() != 0 {
		t.Fatalf("expected deferred write to be incomplete at response return, got %d completed", gs.setDone.Load())
	}

	// Release the store and flush via Close: every queued/in-flight write
	// must be applied before Close returns (shutdown-flush guarantee).
	gs.release()
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if gs.setDone.Load() != 1 {
		t.Fatalf("expected 1 completed Set after Close flush, got %d", gs.setDone.Load())
	}

	data, ok := loadMap(t, gs.inner, sid)
	if !ok || data == nil {
		t.Fatal("expected session persisted after write-behind flush")
	}
	if asFloat(data["count"]) != 7 {
		t.Fatalf("expected count=7 after flush, got %v", data)
	}
}

// TestWriteBehindFlushNoLostWrite drives many requests through the write-behind
// path against a slow store, then Closes, and asserts every write landed.
// This is the no-lost-write-on-graceful-shutdown guarantee.
func TestWriteBehindFlushNoLostWrite(t *testing.T) {
	// A store whose Set is slow enough to keep writes in-flight at Close time.
	slow := &slowStore{inner: NewMemoryStore(), delay: time.Millisecond}
	mw, closer := NewWithCloser(Config{Store: slow, WriteBehind: true})

	const requests = 200
	ids := make([]string, requests)
	for i := range requests {
		idx := i
		handler := func(c *celeris.Context) error {
			s := FromContext(c)
			s.Set("i", idx)
			ids[idx] = s.ID()
			return nil
		}
		chain := []celeris.HandlerFunc{mw, handler}
		_, err := testutil.RunChain(t, chain, "GET", "/")
		testutil.AssertNoError(t, err)
	}

	// Graceful shutdown: must block until every deferred write is durable.
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for i, id := range ids {
		data, ok := loadMap(t, slow.inner, id)
		if !ok || data == nil {
			t.Fatalf("request %d: session %q lost after flush", i, id)
		}
		if asFloat(data["i"]) != float64(i) {
			t.Fatalf("request %d: got i=%v, want %d", i, data["i"], i)
		}
	}
	if slow.sets.Load() != requests {
		t.Fatalf("expected %d completed Set calls, got %d", requests, slow.sets.Load())
	}
}

// TestWriteBehindSnapshotNoRace hammers concurrent requests through the
// write-behind path while the background worker drains. The encoded body is a
// fresh json.Marshal snapshot, independent of the pooled session map, so the
// worker reading buf must never race the next request mutating its recycled
// map. Run under -race to exercise the guarantee.
func TestWriteBehindSnapshotNoRace(t *testing.T) {
	store := NewMemoryStore()
	mw, closer := NewWithCloser(Config{Store: store, WriteBehind: true})

	const goroutines = 24
	const perG = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(gid int) {
			defer wg.Done()
			for j := range perG {
				handler := func(c *celeris.Context) error {
					s := FromContext(c)
					// Mutate several keys so the map (and its recycled
					// backing) is actively written right up to release.
					s.Set("g", gid)
					s.Set("j", j)
					s.Set("payload", "abcdefghijklmnopqrstuvwxyz")
					return nil
				}
				chain := []celeris.HandlerFunc{mw, handler}
				if _, err := testutil.RunChain(t, chain, "GET", "/"); err != nil {
					t.Errorf("goroutine %d: %v", gid, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestWriteBehindEnqueueAfterCloseWritesSynchronously proves a request that
// races shutdown is not silently dropped: after Close, enqueue degrades to a
// synchronous store write rather than panicking on a closed channel.
func TestWriteBehindEnqueueAfterCloseWritesSynchronously(t *testing.T) {
	store := NewMemoryStore()
	wb := newWriteBehindWriter(store, nil)
	if err := wb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	buf, _ := EncodeForTest(map[string]any{"late": true})
	wb.enqueue("late-id", buf, time.Hour)

	data, ok := loadMap(t, store, "late-id")
	if !ok || data == nil || data["late"] != true {
		t.Fatalf("expected post-Close enqueue to write synchronously, got %v ok=%v", data, ok)
	}
}

// TestWriteBehindCloseIdempotent proves Close is safe to call multiple times
// and from multiple goroutines without panicking or blocking forever.
func TestWriteBehindCloseIdempotent(t *testing.T) {
	wb := newWriteBehindWriter(NewMemoryStore(), nil)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := wb.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestWriteBehindErrorHandlerReceivesFailure proves deferred-write failures
// reach the configured ErrorHandler (with a nil Context, since the request is
// gone) instead of vanishing.
func TestWriteBehindErrorHandlerReceivesFailure(t *testing.T) {
	fs := &failStore{saveErr: errors.New("store down")}
	var got atomic.Int64
	mw, closer := NewWithCloser(Config{
		Store:       fs,
		WriteBehind: true,
		ErrorHandler: func(c *celeris.Context, err error) error {
			if c != nil {
				t.Errorf("expected nil Context in deferred error handler, got %v", c)
			}
			if err != nil {
				got.Add(1)
			}
			return err
		},
	})

	handler := func(c *celeris.Context) error {
		FromContext(c).Set("k", "v")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}
	_, err := testutil.RunChain(t, chain, "GET", "/")
	// The request itself must succeed; the deferred failure is async.
	testutil.AssertNoError(t, err)

	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got.Load() != 1 {
		t.Fatalf("expected ErrorHandler to receive 1 deferred failure, got %d", got.Load())
	}
}

// slowStore delays every Set to keep writes in-flight while requests pour in.
type slowStore struct {
	inner store.KV
	delay time.Duration
	sets  atomic.Int64
}

func (s *slowStore) Get(ctx context.Context, id string) ([]byte, error) {
	return s.inner.Get(ctx, id)
}

func (s *slowStore) Set(ctx context.Context, id string, data []byte, exp time.Duration) error {
	time.Sleep(s.delay)
	err := s.inner.Set(ctx, id, data, exp)
	s.sets.Add(1)
	return err
}

func (s *slowStore) Delete(ctx context.Context, id string) error {
	return s.inner.Delete(ctx, id)
}

// EncodeForTest exposes the JSON encoding used by the middleware so tests can
// build a snapshot buffer without reaching into the store package directly.
func EncodeForTest(v map[string]any) ([]byte, error) { return store.EncodeJSON(v) }
