package cache

import (
	"context"
	"testing"
	"unsafe"
)

// TestMemoryStoreKeepsItsOwnKeys pins the cache store's site of celeris#719.
//
// The cache middleware hands its store the key its KeyGenerator returns. The
// default generator builds a fresh string, but a custom one can return a
// request string (c.Path(), a header), which on epoll and io_uring is a view
// of the connection's receive buffer that the engine overwrites with the
// connection's next request. MemoryStore keeps the key as its map key and
// in its LRU node (the eviction deletes by it), so it must keep a copy. The
// key is stored under a view whose bytes then change; the entry must still
// be found under its original key, Scan must report that key, and evicting
// the entry must remove it from the map.
func TestMemoryStoreKeepsItsOwnKeys(t *testing.T) {
	ctx := context.Background()
	const orig, other = "GET /c/aaaa", "GET /c/bbbb"
	m := NewMemoryStore(MemoryStoreConfig{Shards: 1, MaxEntries: 1})
	defer m.Close()

	buf := []byte(orig)
	if err := m.Set(ctx, unsafe.String(unsafe.SliceData(buf), len(buf)), []byte("body"), 0); err != nil {
		t.Fatal(err)
	}
	copy(buf, other) // the engine receives the next request
	keys, _ := m.Scan(ctx, "GET ")
	v, err := m.Get(ctx, orig)
	t.Logf("MW719CACHESTORE get(%s) err=%v scan=%q", orig, err, keys)
	if err != nil || string(v) != "body" {
		t.Errorf("Get(%q) after the caller's key bytes changed: %q, %v; want %q", orig, v, err, "body")
	}
	if len(keys) != 1 || keys[0] != orig {
		t.Errorf("Scan reports keys %q; want [%q]", keys, orig)
	}

	// A second key evicts the first (MaxEntries 1): the eviction deletes by
	// the node's key, which must be the one the map holds.
	if err := m.Set(ctx, "GET /c/cccc", []byte("body"), 0); err != nil {
		t.Fatal(err)
	}
	n := 0
	for i := range m.shards {
		n += len(m.shards[i].items)
	}
	t.Logf("MW719CACHESTORE after eviction map entries=%d", n)
	if n != 1 {
		t.Errorf("after the eviction the store's map holds %d entries; want 1 (the evicted key was not the map's)", n)
	}
}
