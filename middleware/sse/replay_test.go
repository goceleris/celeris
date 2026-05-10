package sse

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/store"
)

// TestRingBufferAppendThenSince — basic Append/Since round-trip on the
// in-memory store. Exit-criterion: Since returns events strictly after
// lastID in append order.
func TestRingBufferAppendThenSince(t *testing.T) {
	r := NewRingBuffer(8)
	ctx := context.Background()

	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		id, err := r.Append(ctx, Event{Data: strings.Repeat("x", i+1)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	got, err := r.Since(ctx, ids[1])
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("Since(ids[1]) returned %d events, want 3", len(got))
	}
	for i, e := range got {
		if e.ID != ids[i+2] {
			t.Errorf("event[%d].ID = %q, want %q", i, e.ID, ids[i+2])
		}
	}
}

// TestRingBufferRollover — issue #250 exit criterion: dropping the
// oldest event once N+1 is appended. Since("") returns the full
// retained tail; Since on the cursor of a still-retained event returns
// only the events strictly after it.
func TestRingBufferRollover(t *testing.T) {
	const ringSize = 4
	r := NewRingBuffer(ringSize)
	ctx := context.Background()

	for i := 0; i < ringSize+2; i++ {
		if _, err := r.Append(ctx, Event{Data: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := r.Since(ctx, "")
	if err != nil {
		t.Fatalf("Since(\"\"): %v", err)
	}
	if len(got) != ringSize {
		t.Errorf("Since(\"\") returned %d, want %d (cap of retained tail)", len(got), ringSize)
	}
	if got[0].ID != "3" {
		t.Errorf("oldest retained event ID = %q, want \"3\"", got[0].ID)
	}

	got, err = r.Since(ctx, "4")
	if err != nil {
		t.Fatalf("Since(\"4\"): %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Since(\"4\") returned %d events, want 2", len(got))
	}
	if got[0].ID != "5" {
		t.Errorf("Since(\"4\")[0].ID = %q, want \"5\"", got[0].ID)
	}
}

// TestRingBufferUnknownLastID — cursor older than retention returns
// ErrLastIDUnknown so the middleware can fall through to a fresh
// handler invocation.
func TestRingBufferUnknownLastID(t *testing.T) {
	r := NewRingBuffer(2)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, _ = r.Append(ctx, Event{Data: "x"})
	}

	_, err := r.Since(ctx, "1")
	if !errors.Is(err, ErrLastIDUnknown) {
		t.Errorf("Since(out-of-window) err = %v, want ErrLastIDUnknown", err)
	}

	_, err = r.Since(ctx, "not-a-number")
	if !errors.Is(err, ErrLastIDUnknown) {
		t.Errorf("Since(garbage) err = %v, want ErrLastIDUnknown", err)
	}
}

// TestRingBufferAppendAllocs — strict-alloc gate from issue #250: ≤ 1
// alloc/op. The single permitted alloc covers the canonical ID string.
func TestRingBufferAppendAllocs(t *testing.T) {
	if raceEnabled || testing.CoverMode() != "" || testing.Short() {
		t.Skip("alloc counts unstable under -race / coverage / -short")
	}
	r := NewRingBuffer(1024)
	ctx := context.Background()
	e := Event{Data: "x"}
	allocs := testing.AllocsPerRun(200, func() {
		_, _ = r.Append(ctx, e)
	})
	const budget = 1.0
	if allocs > budget {
		t.Fatalf("RingBuffer.Append: %.1f allocs/op (budget %.1f)", allocs, budget)
	}
}

// TestKVReplayStoreRoundTrip — Append+Since round-trip via the
// in-memory KV. Mirrors the production Redis path without needing the
// integration build tag.
func TestKVReplayStoreRoundTrip(t *testing.T) {
	kv := store.NewMemoryKV()
	r, err := NewKVReplayStore(KVReplayStoreConfig{KV: kv, Prefix: "test:"})
	if err != nil {
		t.Fatalf("NewKVReplayStore: %v", err)
	}
	ctx := context.Background()

	ids := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		id, err := r.Append(ctx, Event{Data: "v"})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		ids = append(ids, id)
	}

	got, err := r.Since(ctx, ids[1])
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Since returned %d events, want 2", len(got))
	}
	if len(got) >= 2 && (got[0].ID != ids[2] || got[1].ID != ids[3]) {
		t.Errorf("Since returned ids %v, want %v", []string{got[0].ID, got[1].ID}, ids[2:])
	}
}

// TestNewKVReplayStoreReturnsErrOnNilKV — defensive guard. Constructors
// that need a backing store return an error instead of panicking, so
// the misuse surfaces at the call site rather than as a confusing
// nil-pointer panic on the first Append.
func TestNewKVReplayStoreReturnsErrOnNilKV(t *testing.T) {
	r, err := NewKVReplayStore(KVReplayStoreConfig{Prefix: "x:"})
	if err == nil {
		t.Errorf("NewKVReplayStore{KV:nil} returned nil error")
	}
	if r != nil {
		t.Errorf("NewKVReplayStore{KV:nil} returned non-nil store: %T", r)
	}
}

// TestReplayMiddlewareReplaysMissedEvents — end-to-end exit criterion
// from issue #250: client connects with Last-Event-ID, receives only
// the events appended AFTER that ID, then any new events from Send.
func TestReplayMiddlewareReplaysMissedEvents(t *testing.T) {
	r := NewRingBuffer(16)
	ctx := context.Background()

	// Pre-populate the store as if a previous session had sent these.
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		id, err := r.Append(ctx, Event{Data: "old-" + string(rune('a'+i))})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	reqCtx, ms := newSSEContext(t, celeristest.WithHeader("last-event-id", ids[1]))

	handler := New(Config{
		HeartbeatInterval: -1,
		ReplayStore:       r,
		Handler: func(client *Client) {
			_ = client.Send(Event{Data: "fresh-1"})
		},
	})
	if err := handler(reqCtx); err != nil {
		t.Fatal(err)
	}
	wire := ms.allData()
	// Replay phase: only id 3 should appear (events appended after id 2).
	if !strings.Contains(wire, "data: old-c\n\n") {
		t.Errorf("missing replay event %q in wire %q", "old-c", wire)
	}
	if strings.Contains(wire, "data: old-a\n\n") || strings.Contains(wire, "data: old-b\n\n") {
		t.Errorf("wire replayed already-acked events: %q", wire)
	}
	// Fresh send: must carry the next sequential ID.
	if !strings.Contains(wire, "data: fresh-1\n\n") {
		t.Errorf("missing fresh event in wire %q", wire)
	}
	if !strings.Contains(wire, "id: 4\n") {
		t.Errorf("fresh event missing canonical id 4 in wire %q", wire)
	}
}

// TestReplayMiddlewareUnknownLastIDFallsThrough — the ErrLastIDUnknown
// path: middleware does NOT abort, it still invokes Handler. The
// original header value remains observable via client.LastEventID().
func TestReplayMiddlewareUnknownLastIDFallsThrough(t *testing.T) {
	r := NewRingBuffer(2)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		_, _ = r.Append(ctx, Event{Data: "x"})
	}

	reqCtx, ms := newSSEContext(t, celeristest.WithHeader("last-event-id", "1"))

	var sawHandlerCall bool
	var sawHeader string
	handler := New(Config{
		HeartbeatInterval: -1,
		ReplayStore:       r,
		Handler: func(client *Client) {
			sawHandlerCall = true
			sawHeader = client.LastEventID()
			_ = client.Send(Event{Data: "fresh"})
		},
	})
	if err := handler(reqCtx); err != nil {
		t.Fatal(err)
	}
	if !sawHandlerCall {
		t.Errorf("Handler not invoked after ErrLastIDUnknown")
	}
	if sawHeader != "1" {
		t.Errorf("Client.LastEventID() = %q, want %q", sawHeader, "1")
	}
	if !strings.Contains(ms.allData(), "data: fresh\n\n") {
		t.Errorf("fresh event not on wire: %q", ms.allData())
	}
}

// TestKVReplayStoreCounterFallback — when the supplied KV does not
// implement [store.Counter], NewKVReplayStore falls back to a per-
// process atomic counter; IDs still come from a strictly increasing
// integer source. Pins the no-Counter branch.
func TestKVReplayStoreCounterFallback(t *testing.T) {
	// kvWithoutCounter wraps a MemoryKV but explicitly does NOT
	// expose Increment, so the type assertion in NewKVReplayStore
	// fails over to localSeq.
	kv := &noCounterKV{inner: store.NewMemoryKV()}
	r, err := NewKVReplayStore(KVReplayStoreConfig{KV: kv, Prefix: "fb:"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for want := 1; want <= 5; want++ {
		id, err := r.Append(ctx, Event{Data: "x"})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		got := 0
		_, _ = fmt.Sscanf(id, "%d", &got)
		if got != want {
			t.Errorf("local fallback id #%d = %d, want %d", want, got, want)
		}
	}
}

type noCounterKV struct{ inner store.KV }

func (k *noCounterKV) Get(ctx context.Context, key string) ([]byte, error) {
	return k.inner.Get(ctx, key)
}
func (k *noCounterKV) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return k.inner.Set(ctx, key, value, ttl)
}
func (k *noCounterKV) Delete(ctx context.Context, key string) error {
	return k.inner.Delete(ctx, key)
}

// TestKVReplayStoreCounterUsesShared — when the KV implements
// [store.Counter], IDs come from that backend. Pin via a counting KV
// that records each Increment call.
func TestKVReplayStoreCounterUsesShared(t *testing.T) {
	cnt := &countingCounterKV{inner: store.NewMemoryKV()}
	r, err := NewKVReplayStore(KVReplayStoreConfig{KV: cnt, Prefix: "shared:"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 4 {
		if _, err := r.Append(ctx, Event{Data: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := cnt.incrCalls.Load(); got != 4 {
		t.Errorf("Counter.Increment calls = %d, want 4", got)
	}
}

type countingCounterKV struct {
	inner     store.KV
	incrCalls atomic.Int64
}

func (k *countingCounterKV) Get(ctx context.Context, key string) ([]byte, error) {
	return k.inner.Get(ctx, key)
}
func (k *countingCounterKV) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return k.inner.Set(ctx, key, value, ttl)
}
func (k *countingCounterKV) Delete(ctx context.Context, key string) error {
	return k.inner.Delete(ctx, key)
}
func (k *countingCounterKV) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	k.incrCalls.Add(1)
	return k.inner.(store.Counter).Increment(ctx, key, ttl)
}

// TestKVReplayStoreAsyncAppendBackpressure — when AsyncAppend is on,
// Append returns immediately while the KV.Set is in flight. With a
// gated KV the test asserts (a) the first N concurrent Appends return
// in microseconds even though the KV has not completed any Set, and
// (b) the (N+1)th Append blocks on the asyncSlots semaphore until a
// slot frees.
func TestKVReplayStoreAsyncAppendBackpressure(t *testing.T) {
	const slots = 4
	gate := make(chan struct{})
	gk := &gateKV{gate: gate}

	r, err := NewKVReplayStore(KVReplayStoreConfig{
		KV:                     gk,
		Prefix:                 "async:",
		AsyncAppend:            true,
		AsyncAppendConcurrency: slots,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	// First slots Appends fill the semaphore but each returns fast
	// because the goroutine takes the slot, not Append.
	start := time.Now()
	for range slots {
		if _, err := r.Append(ctx, Event{Data: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("first %d Appends took %v, want <50ms", slots, d)
	}

	// Now spawn the (slots+1)th Append in a goroutine; assert it
	// remains blocked until the gate releases.
	blockedDone := make(chan struct{})
	go func() {
		_, _ = r.Append(ctx, Event{Data: "x"})
		close(blockedDone)
	}()
	select {
	case <-blockedDone:
		t.Fatalf("Append #%d returned before gate released — semaphore not enforcing backpressure", slots+1)
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}
	close(gate) // release every gated KV.Set
	select {
	case <-blockedDone:
	case <-time.After(time.Second):
		t.Fatalf("Append #%d still blocked 1s after gate released", slots+1)
	}
}

// gateKV is a minimal store.KV whose Set blocks until gate is closed.
// Used to probe AsyncAppend backpressure deterministically.
type gateKV struct {
	gate chan struct{}
}

func (g *gateKV) Get(ctx context.Context, key string) ([]byte, error) { return nil, store.ErrNotFound }
func (g *gateKV) Set(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	<-g.gate
	return nil
}
func (g *gateKV) Delete(_ context.Context, _ string) error { return nil }

// TestRingBufferZeroSize — pin the clamp behavior when caller passes 0
// or negative. The constructor must produce a usable store, not panic.
func TestRingBufferZeroSize(t *testing.T) {
	for _, n := range []int{0, -1, -1000} {
		store := NewRingBuffer(n)
		// Should be usable: at least one Append + Since round-trips.
		id, err := store.Append(context.Background(), Event{Data: "x"})
		if err != nil {
			t.Errorf("NewRingBuffer(%d): Append errored: %v", n, err)
			continue
		}
		evs, err := store.Since(context.Background(), "")
		if err != nil {
			t.Errorf("NewRingBuffer(%d): Since errored: %v", n, err)
			continue
		}
		if len(evs) != 1 || evs[0].ID != id {
			t.Errorf("NewRingBuffer(%d): want 1 event id=%s, got %v", n, id, evs)
		}
	}
}

// TestKVReplayStoreSinceSkipsAgedOutEvents — when the underlying KV
// has aged out (or the test deletes) an event blob whose seq is still
// in the in-memory index, Since must skip it silently, not error.
// Pinned because the comment claims this behavior; without a test a
// regression that returns the error here would silently break
// resume-after-TTL.
func TestKVReplayStoreSinceSkipsAgedOutEvents(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	rs, err := NewKVReplayStore(KVReplayStoreConfig{KV: kv, Prefix: "agetest/"})
	if err != nil {
		t.Fatalf("NewKVReplayStore: %v", err)
	}
	ctx := context.Background()

	id1, _ := rs.Append(ctx, Event{Data: "a"})
	id2, _ := rs.Append(ctx, Event{Data: "b"})
	id3, _ := rs.Append(ctx, Event{Data: "c"})

	// Simulate id2 aging out from the KV.
	if err := kv.Delete(ctx, "agetest/events/"+id2); err != nil {
		t.Fatalf("kv.Delete: %v", err)
	}

	got, err := rs.Since(ctx, "0")
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 surviving events (id=%s skipped), got %d: %+v", id2, len(got), got)
	}
	if got[0].ID != id1 || got[1].ID != id3 {
		t.Errorf("survivors out of order: got [%s, %s], want [%s, %s]",
			got[0].ID, got[1].ID, id1, id3)
	}
}

// TestKVReplayStoreIndexRollover — when the in-memory index hits
// MaxIndex, the oldest 25 % drops. A cursor pointing inside the
// dropped quartile must produce ErrLastIDUnknown so the caller knows
// to start fresh.
func TestKVReplayStoreIndexRollover(t *testing.T) {
	kv := store.NewMemoryKV()
	defer kv.Close()
	const maxIdx = 16
	rs, err := NewKVReplayStore(KVReplayStoreConfig{
		KV:       kv,
		Prefix:   "roll/",
		MaxIndex: maxIdx,
	})
	if err != nil {
		t.Fatalf("NewKVReplayStore: %v", err)
	}
	ctx := context.Background()

	// Append maxIdx + 5 events. The first 25 % (4 events) get dropped
	// from the index. Cursor "1" (first event) is now in the dropped
	// region.
	var firstID string
	for i := range maxIdx + 5 {
		id, err := rs.Append(ctx, Event{Data: "ev"})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if firstID == "" {
			firstID = id
		}
	}

	// Cursor older than the oldest surviving seq → ErrLastIDUnknown.
	_, err = rs.Since(ctx, firstID)
	if !errors.Is(err, ErrLastIDUnknown) {
		t.Fatalf("Since(%q) after rollover: want ErrLastIDUnknown, got %v", firstID, err)
	}
}

// TestKVReplayStoreIndexMonotonicityUnderConcurrentAppend pins the
// invariant that even with N goroutines racing Append against a
// non-trivial KV.Set latency, the resulting index is still monotonic.
// Regression guard for the sorted-insertion fix in Append.
func TestKVReplayStoreIndexMonotonicityUnderConcurrentAppend(t *testing.T) {
	// gateKV defined in this file delays Set until released; in this
	// test we don't gate, just use plain MemoryKV (which has its own
	// per-shard lock contention to perturb ordering).
	kv := store.NewMemoryKV()
	defer kv.Close()
	rs, err := NewKVReplayStore(KVReplayStoreConfig{KV: kv, Prefix: "mono/"})
	if err != nil {
		t.Fatalf("NewKVReplayStore: %v", err)
	}
	ctx := context.Background()

	const N = 64
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			_, _ = rs.Append(ctx, Event{Data: "ev" + strconv.Itoa(i)})
		}()
	}
	wg.Wait()

	// Read back via Since. The IDs must be strictly increasing
	// (by integer seq, not lexicographic).
	got, err := rs.Since(ctx, "")
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(got) != N {
		t.Fatalf("want %d events, got %d", N, len(got))
	}
	var prev int64 = -1
	for i, e := range got {
		seq, perr := strconv.ParseInt(e.ID, 10, 64)
		if perr != nil {
			t.Fatalf("got[%d].ID=%q non-integer: %v", i, e.ID, perr)
		}
		if seq <= prev {
			t.Fatalf("non-monotonic: got[%d].seq=%d <= prev=%d (full=%v)", i, seq, prev, got)
		}
		prev = seq
	}
}

// TestKVReplayStoreCounterMonotonicAcrossInstances — two kvStore
// instances backed by the same KV must see strictly increasing IDs
// across both, when the KV implements [store.Counter]. Without the
// Counter feature-detect, IDs would collide on multi-instance reconnect.
func TestKVReplayStoreCounterMonotonicAcrossInstances(t *testing.T) {
	kv := store.NewMemoryKV() // implements Counter
	defer kv.Close()

	rs1, err := NewKVReplayStore(KVReplayStoreConfig{KV: kv, Prefix: "x/"})
	if err != nil {
		t.Fatalf("NewKVReplayStore #1: %v", err)
	}
	rs2, err := NewKVReplayStore(KVReplayStoreConfig{KV: kv, Prefix: "x/"})
	if err != nil {
		t.Fatalf("NewKVReplayStore #2: %v", err)
	}

	ctx := context.Background()
	seen := map[string]struct{}{}
	for range 32 {
		id1, err := rs1.Append(ctx, Event{Data: "a"})
		if err != nil {
			t.Fatalf("rs1.Append: %v", err)
		}
		id2, err := rs2.Append(ctx, Event{Data: "b"})
		if err != nil {
			t.Fatalf("rs2.Append: %v", err)
		}
		if _, dup := seen[id1]; dup {
			t.Fatalf("duplicate id %s from rs1 — Counter not shared", id1)
		}
		if _, dup := seen[id2]; dup {
			t.Fatalf("duplicate id %s from rs2 — Counter not shared", id2)
		}
		seen[id1] = struct{}{}
		seen[id2] = struct{}{}
	}
}
