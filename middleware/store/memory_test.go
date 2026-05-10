package store

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryKVGetMissing(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	v, err := m.Get(context.Background(), "absent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: expected ErrNotFound, got %v", err)
	}
	if v != nil {
		t.Fatalf("Get missing: expected nil value, got %q", v)
	}
}

func TestMemoryKVSetGetDelete(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()
	if err := m.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := m.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(v) != "v" {
		t.Fatalf("Get: got %q want %q", v, "v")
	}
	if err := m.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = m.Get(ctx, "k")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Delete: expected ErrNotFound, got %v", err)
	}
}

func TestMemoryKVTTLExpiry(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()
	if err := m.Set(ctx, "k", []byte("v"), 20*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	_, err := m.Get(ctx, "k")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("after TTL: expected ErrNotFound, got %v", err)
	}
}

func TestMemoryKVGetAndDeleteAtomic(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()
	_ = m.Set(ctx, "k", []byte("v"), 0)

	v, err := m.GetAndDelete(ctx, "k")
	if err != nil {
		t.Fatalf("GetAndDelete: %v", err)
	}
	if string(v) != "v" {
		t.Fatalf("GetAndDelete: got %q want %q", v, "v")
	}
	_, err = m.Get(ctx, "k")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("after GetAndDelete: expected ErrNotFound, got %v", err)
	}

	_, err = m.GetAndDelete(ctx, "absent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetAndDelete missing: expected ErrNotFound, got %v", err)
	}
}

func TestMemoryKVScan(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()
	_ = m.Set(ctx, "sess:1", []byte("a"), 0)
	_ = m.Set(ctx, "sess:2", []byte("b"), 0)
	_ = m.Set(ctx, "csrf:1", []byte("c"), 0)

	got, err := m.Scan(ctx, "sess:")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "sess:1" || got[1] != "sess:2" {
		t.Fatalf("Scan sess:: got %v", got)
	}
	got, _ = m.Scan(ctx, "csrf:")
	if len(got) != 1 || got[0] != "csrf:1" {
		t.Fatalf("Scan csrf:: got %v", got)
	}
}

func TestMemoryKVDeletePrefix(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()
	_ = m.Set(ctx, "a:1", []byte("1"), 0)
	_ = m.Set(ctx, "a:2", []byte("2"), 0)
	_ = m.Set(ctx, "b:1", []byte("3"), 0)

	if err := m.DeletePrefix(ctx, "a:"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	got, _ := m.Scan(ctx, "a:")
	if len(got) != 0 {
		t.Fatalf("after DeletePrefix a:: got %v", got)
	}
	got, _ = m.Scan(ctx, "b:")
	if len(got) != 1 {
		t.Fatalf("b: should be intact, got %v", got)
	}
}

func TestMemoryKVSetNX(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()

	ok, err := m.SetNX(ctx, "lock", []byte("one"), 0)
	if err != nil || !ok {
		t.Fatalf("first SetNX: ok=%v err=%v", ok, err)
	}
	ok, err = m.SetNX(ctx, "lock", []byte("two"), 0)
	if err != nil || ok {
		t.Fatalf("second SetNX: expected contention, got ok=%v err=%v", ok, err)
	}
	v, _ := m.Get(ctx, "lock")
	if string(v) != "one" {
		t.Fatalf("value after contention: got %q want %q", v, "one")
	}
}

func TestMemoryKVSetNXAfterExpiry(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()
	_ = m.Set(ctx, "k", []byte("old"), 20*time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	ok, err := m.SetNX(ctx, "k", []byte("new"), 0)
	if err != nil || !ok {
		t.Fatalf("SetNX after expiry: ok=%v err=%v", ok, err)
	}
	v, _ := m.Get(ctx, "k")
	if string(v) != "new" {
		t.Fatalf("value after SetNX: got %q want %q", v, "new")
	}
}

func TestMemoryKVValueIsolation(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()
	src := []byte("hello")
	_ = m.Set(ctx, "k", src, 0)
	src[0] = 'H' // mutate input after Set
	v, _ := m.Get(ctx, "k")
	if string(v) != "hello" {
		t.Fatalf("Set should defensively copy input: got %q", v)
	}
	v[0] = 'X'
	v2, _ := m.Get(ctx, "k")
	if string(v2) != "hello" {
		t.Fatalf("Get should defensively copy output: got %q", v2)
	}
}

func TestMemoryKVConcurrentSetGetDelete(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()

	const N = 64
	const iters = 500
	var wg sync.WaitGroup
	var hits atomic.Int64
	for g := 0; g < N; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				k := "shared"
				_ = m.Set(ctx, k, []byte("v"), 0)
				if _, err := m.Get(ctx, k); err == nil {
					hits.Add(1)
				}
				_ = m.Delete(ctx, k)
			}
		}(g)
	}
	wg.Wait()
	_ = hits.Load()
}

func TestMemoryKVPrefixedWrapper(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()
	p := Prefixed(m, "foo:")

	_ = p.Set(ctx, "k", []byte("v"), 0)
	raw, _ := m.Get(ctx, "foo:k")
	if string(raw) != "v" {
		t.Fatalf("Prefixed Set: raw key should be foo:k, got %q", raw)
	}
	v, _ := p.Get(ctx, "k")
	if string(v) != "v" {
		t.Fatalf("Prefixed Get: got %q want %q", v, "v")
	}
	pg := p.(GetAndDeleter)
	v2, _ := pg.GetAndDelete(ctx, "k")
	if string(v2) != "v" {
		t.Fatalf("Prefixed GetAndDelete: got %q want %q", v2, "v")
	}
	if _, err := m.Get(ctx, "foo:k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Prefixed GetAndDelete: expected ErrNotFound, got %v", err)
	}
}

func TestEncodeDecodeResponse(t *testing.T) {
	r := EncodedResponse{
		Status: 200,
		Headers: [][2]string{
			{"content-type", "text/plain"},
			{"x-cache", "HIT"},
		},
		Body: []byte("hello"),
	}
	buf := r.Encode()
	got, err := DecodeResponse(buf)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if got.Status != r.Status {
		t.Fatalf("status: got %d want %d", got.Status, r.Status)
	}
	if len(got.Headers) != 2 || got.Headers[0][0] != "content-type" || got.Headers[1][1] != "HIT" {
		t.Fatalf("headers: got %v", got.Headers)
	}
	if string(got.Body) != "hello" {
		t.Fatalf("body: got %q want %q", got.Body, "hello")
	}
}

func TestDecodeResponseErrors(t *testing.T) {
	if _, err := DecodeResponse(nil); !errors.Is(err, ErrInvalidWireFormat) {
		t.Fatalf("nil buf: expected ErrInvalidWireFormat, got %v", err)
	}
	if _, err := DecodeResponse([]byte{99, 0, 0, 0, 0}); !errors.Is(err, ErrInvalidWireFormat) {
		t.Fatalf("bad version: expected ErrInvalidWireFormat, got %v", err)
	}
}

func TestEncodeJSONRoundtrip(t *testing.T) {
	type s struct {
		A int
		B string
	}
	in := s{A: 42, B: "hi"}
	buf, err := EncodeJSON(in)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	var out s
	if err := DecodeJSON(buf, &out); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if out != in {
		t.Fatalf("roundtrip: got %+v want %+v", out, in)
	}
}

func TestMemoryKVCleanupContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := NewMemoryKV(MemoryKVConfig{CleanupContext: ctx, CleanupInterval: time.Millisecond})
	cancel()
	// Give the goroutine a moment to observe the cancel.
	time.Sleep(10 * time.Millisecond)
	// Close should be a no-op after context cancel — verify it doesn't panic.
	m.Close()
}

// TestMemoryKVIncrementMonotonic — fresh counter starts at 1 and
// hands out monotonic increasing integers without gaps under sequential
// access. Pins the post-increment-value contract from store.Counter.
func TestMemoryKVIncrementMonotonic(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()

	for i := int64(1); i <= 10; i++ {
		got, err := m.Increment(ctx, "seq", 0)
		if err != nil {
			t.Fatalf("Increment: %v", err)
		}
		if got != i {
			t.Errorf("Increment #%d = %d, want %d", i, got, i)
		}
	}
}

// TestMemoryKVIncrementTTL — counter respects ttl. After expiry the
// next Increment sees a fresh "missing key" state and returns 1.
func TestMemoryKVIncrementTTL(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()

	if v, err := m.Increment(ctx, "k", 30*time.Millisecond); err != nil || v != 1 {
		t.Fatalf("Increment fresh = (%d, %v), want (1, nil)", v, err)
	}
	if v, err := m.Increment(ctx, "k", 30*time.Millisecond); err != nil || v != 2 {
		t.Fatalf("Increment fresh#2 = (%d, %v), want (2, nil)", v, err)
	}
	time.Sleep(60 * time.Millisecond)
	v, err := m.Increment(ctx, "k", 30*time.Millisecond)
	if err != nil {
		t.Fatalf("Increment post-expiry: %v", err)
	}
	if v != 1 {
		t.Errorf("Increment post-expiry = %d, want 1 (counter expired and reset)", v)
	}
}

// TestMemoryKVIncrementCorrupt — Set then Increment on a non-numeric
// value surfaces a parse error at Increment time, not silently returns
// a wrong value.
func TestMemoryKVIncrementCorrupt(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()

	if err := m.Set(ctx, "k", []byte("not-a-number"), 0); err != nil {
		t.Fatal(err)
	}
	v, err := m.Increment(ctx, "k", 0)
	if err == nil {
		t.Errorf("Increment on corrupt counter returned nil error, value=%d", v)
	}
}

// TestMemoryKVIncrementContended — 64 goroutines × 250 increments on
// a single shared counter must arrive at exactly 16000 with no lost
// updates. Pins atomicity under shard contention.
func TestMemoryKVIncrementContended(t *testing.T) {
	m := NewMemoryKV()
	defer m.Close()
	ctx := context.Background()

	const goroutines = 64
	const perG = 250
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perG {
				if _, err := m.Increment(ctx, "shared", 0); err != nil {
					t.Errorf("Increment: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	final, err := m.Increment(ctx, "shared", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(goroutines*perG + 1)
	if final != want {
		t.Errorf("final = %d, want %d (lost updates)", final, want)
	}
}
