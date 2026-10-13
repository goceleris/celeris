package cache

import (
	"context"
	"encoding/binary"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/middleware/store"
)

func gen888(g uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, g)
	return b
}

// TestMemoryStoreSetIfNewer888: the ordered write the cache's fills make.
func TestMemoryStoreSetIfNewer888(t *testing.T) {
	ctx := context.Background()
	base := genNow()
	get := func(m *MemoryStore) string {
		t.Helper()
		v, err := m.Get(ctx, "k")
		if err != nil {
			return "absent"
		}
		return string(v)
	}
	for _, tc := range []struct {
		name  string
		steps func(m *MemoryStore)
		want  string
	}{
		{"older-after-newer-is-skipped", func(m *MemoryStore) {
			_ = m.setIfNewer(ctx, "k", []byte("new"), time.Minute, base+10)
			_ = m.setIfNewer(ctx, "k", []byte("old"), time.Minute, base+5)
		}, "new"},
		{"newer-after-older-replaces", func(m *MemoryStore) {
			_ = m.setIfNewer(ctx, "k", []byte("old"), time.Minute, base+5)
			_ = m.setIfNewer(ctx, "k", []byte("new"), time.Minute, base+10)
		}, "new"},
		{"same-generation-replaces", func(m *MemoryStore) {
			_ = m.setIfNewer(ctx, "k", []byte("a"), time.Minute, base+5)
			_ = m.setIfNewer(ctx, "k", []byte("b"), time.Minute, base+5)
		}, "b"},
		{"absent-key-takes-any-generation", func(m *MemoryStore) {
			_ = m.setIfNewer(ctx, "k", []byte("only"), time.Minute, 1)
		}, "only"},
		{"plain-Set-after-a-fill-replaces-it", func(m *MemoryStore) {
			_ = m.setIfNewer(ctx, "k", []byte("fill"), time.Minute, base)
			_ = m.Set(ctx, "k", []byte("set"), time.Minute)
		}, "set"},
		{"expired-entry-does-not-block-an-older-generation", func(m *MemoryStore) {
			_ = m.setIfNewer(ctx, "k", []byte("new"), 20*time.Millisecond, base+10)
			time.Sleep(40 * time.Millisecond)
			_ = m.setIfNewer(ctx, "k", []byte("old"), time.Minute, base+5)
		}, "old"},
		{"no-expiry-entry-keeps-its-generation", func(m *MemoryStore) {
			_ = m.setIfNewer(ctx, "k", []byte("new"), 0, base+10)
			_ = m.setIfNewer(ctx, "k", []byte("old"), time.Minute, base+5)
		}, "new"},
		{"deleted-key-takes-any-generation", func(m *MemoryStore) {
			_ = m.setIfNewer(ctx, "k", []byte("new"), time.Minute, base+10)
			_ = m.Delete(ctx, "k")
			_ = m.setIfNewer(ctx, "k", []byte("old"), time.Minute, base+5)
		}, "old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMemoryStore()
			defer m.Close()
			tc.steps(m)
			if got := get(m); got != tc.want {
				t.Errorf("entry %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMemoryStoreSetIfNewerConvergesOnTheNewest888 (run with -race): writes
// of random generations race on one key and on a few keys; whatever the
// interleaving, the entry that remains is the one with the largest generation.
func TestMemoryStoreSetIfNewerConvergesOnTheNewest888(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore(MemoryStoreConfig{Shards: 2})
	defer m.Close()
	keys := []string{"a", "b", "c"}
	maxGen := map[string]uint64{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), 7))
			for range 2000 {
				k := keys[r.IntN(len(keys))]
				g := uint64(r.IntN(1_000_000)) + 1
				_ = m.setIfNewer(ctx, k, gen888(g), time.Minute, g)
				mu.Lock()
				if g > maxGen[k] {
					maxGen[k] = g
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	for _, k := range keys {
		v, err := m.Get(ctx, k)
		if err != nil || binary.BigEndian.Uint64(v) != maxGen[k] {
			t.Errorf("key %s: entry %v (err %v), want the largest generation %d", k, v, err, maxGen[k])
		}
	}
}

// TestFillsAreWrittenInOrderThroughTheStoreInterface888: a store that does
// not offer the ordered write gets a plain Set (the external stores).
func TestFillsAreWrittenInOrderThroughTheStoreInterface888(t *testing.T) {
	ctx := context.Background()
	plain := store.NewMemoryKV()
	defer plain.Close()
	if err := storeFill(ctx, plain, "k", []byte("v"), time.Minute, genNow()); err != nil {
		t.Fatal(err)
	}
	if v, err := plain.Get(ctx, "k"); err != nil || string(v) != "v" {
		t.Errorf("a store without the ordered write: %q, %v, want its plain Set", v, err)
	}
	m := NewMemoryStore()
	defer m.Close()
	g := genNow()
	_ = storeFill(ctx, m, "k", []byte("newer"), time.Minute, g+10)
	_ = storeFill(ctx, m, "k", []byte("older"), time.Minute, g)
	if v, _ := m.Get(ctx, "k"); string(v) != "newer" {
		t.Errorf("MemoryStore: %q, want the newer fill's entry", v)
	}
}
