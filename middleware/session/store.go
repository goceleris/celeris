package session

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/goceleris/celeris/middleware/internal/fnv1a"
)

// Store defines the interface for session storage backends.
//
// All methods accept a [context.Context] to support cancellation and
// deadline propagation. The middleware passes the request context
// obtained from [celeris.Context.Context].
type Store interface {
	// Get retrieves session data by ID. Returns nil map and no error if the
	// session does not exist or has expired.
	Get(ctx context.Context, id string) (map[string]any, error)

	// Save persists session data with the given expiry duration.
	Save(ctx context.Context, id string, data map[string]any, expiry time.Duration) error

	// Delete removes a session by ID.
	Delete(ctx context.Context, id string) error

	// Reset wipes all sessions from the store.
	Reset(ctx context.Context) error
}

// MemoryStoreConfig configures the in-memory session store.
type MemoryStoreConfig struct {
	// Shards is the number of lock shards. Default: runtime.NumCPU().
	Shards int

	// CleanupInterval is how often expired entries are evicted.
	// Default: 1 minute.
	CleanupInterval time.Duration

	// CleanupContext, if set, controls cleanup goroutine lifetime.
	// When the context is cancelled, the cleanup goroutine stops.
	// If nil, cleanup runs until the process exits.
	CleanupContext context.Context
}

type memoryStore struct {
	shards []msShard
	mask   uint64
	cancel context.CancelFunc // cancels the cleanup goroutine
}

type msShard struct {
	mu    sync.Mutex
	items map[string]*msItem
}

type msItem struct {
	data   map[string]any
	expiry int64 // UnixNano; 0 means no expiry
}

// NewMemoryStore creates an in-memory session store backed by sharded maps.
// A background goroutine periodically evicts expired sessions.
//
// MemoryStore should be created once and reused for the lifetime of the
// application. Each call to NewMemoryStore spawns a cleanup goroutine;
// call [MemoryStore.Close] to stop it if you need deterministic shutdown
// (e.g., in tests). If you provide [MemoryStoreConfig].CleanupContext,
// cancelling that context also stops the goroutine.
func NewMemoryStore(config ...MemoryStoreConfig) Store {
	var cfg MemoryStoreConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	if cfg.Shards <= 0 {
		cfg.Shards = runtime.NumCPU()
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = time.Minute
	}

	n := fnv1a.NextPow2(cfg.Shards)

	ctx := cfg.CleanupContext
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)

	s := &memoryStore{
		shards: make([]msShard, n),
		mask:   uint64(n - 1),
		cancel: cancel,
	}
	for i := range s.shards {
		s.shards[i].items = make(map[string]*msItem)
	}

	go s.cleanup(ctx, cfg.CleanupInterval)

	return s
}

// Close stops the background cleanup goroutine. It is safe to call
// multiple times. After Close, the store is still usable for Get/Save/
// Delete/Reset, but expired entries will no longer be evicted automatically.
func (m *memoryStore) Close() {
	m.cancel()
}

func (m *memoryStore) shard(id string) *msShard {
	return &m.shards[fnv1a.Hash(id)&m.mask]
}

func (m *memoryStore) Get(_ context.Context, id string) (map[string]any, error) {
	s := m.shard(id)
	s.mu.Lock()
	item, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return nil, nil
	}
	if item.expiry > 0 && time.Now().UnixNano() > item.expiry {
		delete(s.items, id)
		s.mu.Unlock()
		return nil, nil
	}
	// Return a shallow copy so callers cannot mutate the store's map.
	cp := make(map[string]any, len(item.data))
	for k, v := range item.data {
		cp[k] = v
	}
	s.mu.Unlock()
	return cp, nil
}

func (m *memoryStore) Save(_ context.Context, id string, data map[string]any, expiry time.Duration) error {
	s := m.shard(id)
	cp := make(map[string]any, len(data))
	for k, v := range data {
		cp[k] = v
	}
	var exp int64
	if expiry > 0 {
		exp = time.Now().Add(expiry).UnixNano()
	}
	s.mu.Lock()
	s.items[id] = &msItem{data: cp, expiry: exp}
	s.mu.Unlock()
	return nil
}

func (m *memoryStore) Delete(_ context.Context, id string) error {
	s := m.shard(id)
	s.mu.Lock()
	delete(s.items, id)
	s.mu.Unlock()
	return nil
}

// Reset wipes all sessions. Note: this is not atomic across shards.
// Concurrent requests may observe a partially-reset state where some
// shards are cleared and others are not. For most use cases (admin
// logout-all, test cleanup) this is acceptable. If full atomicity is
// required, use an external lock or a store backend that supports
// transactional truncation.
func (m *memoryStore) Reset(_ context.Context) error {
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.Lock()
		clear(s.items)
		s.mu.Unlock()
	}
	return nil
}

func (m *memoryStore) cleanup(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	shardIdx := 0
	// Process min(4, numShards) shards per tick to amortize cleanup
	// across multiple ticks while making progress faster than one
	// shard at a time.
	perTick := 4
	if len(m.shards) < perTick {
		perTick = len(m.shards)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			nowNano := now.UnixNano()
			for range perTick {
				s := &m.shards[shardIdx%len(m.shards)]
				shardIdx++
				s.mu.Lock()
				for k, item := range s.items {
					if item.expiry > 0 && nowNano > item.expiry {
						delete(s.items, k)
					}
				}
				s.mu.Unlock()
			}
		}
	}
}
