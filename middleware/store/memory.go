package store

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/goceleris/celeris/middleware/internal/fnv1a"
)

// MemoryKVConfig configures the in-memory [MemoryKV] store.
type MemoryKVConfig struct {
	// Shards is the number of lock shards. Default: runtime.NumCPU(),
	// rounded up to the next power of two.
	Shards int

	// CleanupInterval is how often expired entries are evicted.
	// Default: 1 minute. Zero is replaced with the default.
	CleanupInterval time.Duration

	// CleanupContext, if set, controls the cleanup goroutine lifetime.
	// When the context is cancelled, the goroutine stops. If nil,
	// [MemoryKV.Close] is the only way to stop the goroutine.
	CleanupContext context.Context
}

// MemoryKV is an in-memory, sharded implementation of [KV]. It also
// implements [GetAndDeleter], [Scanner], [PrefixDeleter], and [SetNXer].
//
// MemoryKV is intended as the default/test backend and for single-process
// deployments. For multi-instance deployments, use a driver-backed adapter
// (middleware/session/redisstore, etc.).
type MemoryKV struct {
	shards []memShard
	mask   uint64
	cancel context.CancelFunc
}

type memShard struct {
	mu    sync.Mutex
	items map[string]*memItem
}

type memItem struct {
	value  []byte
	expiry int64 // UnixNano; 0 means no expiry
}

// NewMemoryKV returns a new [MemoryKV] with a running cleanup goroutine.
// Call [MemoryKV.Close] when the store is no longer needed, or pass a
// cancellable [MemoryKVConfig.CleanupContext].
func NewMemoryKV(config ...MemoryKVConfig) *MemoryKV {
	var cfg MemoryKVConfig
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

	m := &MemoryKV{
		shards: make([]memShard, n),
		mask:   uint64(n - 1),
		cancel: cancel,
	}
	for i := range m.shards {
		m.shards[i].items = make(map[string]*memItem)
	}

	go m.cleanup(ctx, cfg.CleanupInterval)

	return m
}

// Close stops the cleanup goroutine. Safe to call multiple times. After
// Close, Get/Set/Delete still work but expired entries are not reaped.
func (m *MemoryKV) Close() {
	m.cancel()
}

func (m *MemoryKV) shard(key string) *memShard {
	return &m.shards[fnv1a.Hash(key)&m.mask]
}

// Get implements [KV].
func (m *MemoryKV) Get(_ context.Context, key string) ([]byte, error) {
	s := m.shard(key)
	s.mu.Lock()
	item, ok := s.items[key]
	if !ok {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	if item.expiry > 0 && time.Now().UnixNano() > item.expiry {
		delete(s.items, key)
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	out := make([]byte, len(item.value))
	copy(out, item.value)
	s.mu.Unlock()
	return out, nil
}

// Set implements [KV]. A ttl <= 0 stores the value with no expiry.
func (m *MemoryKV) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	cp := make([]byte, len(value))
	copy(cp, value)
	var exp int64
	if ttl > 0 {
		exp = time.Now().Add(ttl).UnixNano()
	}
	s := m.shard(key)
	s.mu.Lock()
	s.items[key] = &memItem{value: cp, expiry: exp}
	s.mu.Unlock()
	return nil
}

// Delete implements [KV].
func (m *MemoryKV) Delete(_ context.Context, key string) error {
	s := m.shard(key)
	s.mu.Lock()
	delete(s.items, key)
	s.mu.Unlock()
	return nil
}

// GetAndDelete implements [GetAndDeleter] atomically under the shard lock.
// Returns nil, [ErrNotFound] on a missing or expired key.
func (m *MemoryKV) GetAndDelete(_ context.Context, key string) ([]byte, error) {
	s := m.shard(key)
	s.mu.Lock()
	item, ok := s.items[key]
	if !ok {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	delete(s.items, key)
	if item.expiry > 0 && time.Now().UnixNano() > item.expiry {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	out := make([]byte, len(item.value))
	copy(out, item.value)
	s.mu.Unlock()
	return out, nil
}

// Scan implements [Scanner]. Returns all non-expired keys with the given
// prefix. Scan is a point-in-time snapshot; concurrent Set/Delete may or
// may not be reflected.
func (m *MemoryKV) Scan(_ context.Context, prefix string) ([]string, error) {
	var out []string
	nowNano := time.Now().UnixNano()
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.Lock()
		for k, it := range s.items {
			if it.expiry > 0 && nowNano > it.expiry {
				continue
			}
			if strings.HasPrefix(k, prefix) {
				out = append(out, k)
			}
		}
		s.mu.Unlock()
	}
	return out, nil
}

// DeletePrefix implements [PrefixDeleter].
func (m *MemoryKV) DeletePrefix(_ context.Context, prefix string) error {
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.Lock()
		for k := range s.items {
			if strings.HasPrefix(k, prefix) {
				delete(s.items, k)
			}
		}
		s.mu.Unlock()
	}
	return nil
}

// SetNX implements [SetNXer]. Acquires the key only if it does not
// already exist (or has expired). Returns (true, nil) on acquisition,
// (false, nil) on contention.
func (m *MemoryKV) SetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Check contention under the lock BEFORE copying — a contended
	// SetNX otherwise pays for a defensive value copy it throws away.
	// Idempotency middleware's lock-vs-replay contention path hits
	// this frequently.
	if it, ok := s.items[key]; ok {
		if it.expiry == 0 || time.Now().UnixNano() <= it.expiry {
			return false, nil
		}
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	var exp int64
	if ttl > 0 {
		exp = time.Now().Add(ttl).UnixNano()
	}
	s.items[key] = &memItem{value: cp, expiry: exp}
	return true, nil
}

func (m *MemoryKV) cleanup(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	shardIdx := 0
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
				for k, it := range s.items {
					if it.expiry > 0 && nowNano > it.expiry {
						delete(s.items, k)
					}
				}
				s.mu.Unlock()
			}
		}
	}
}
