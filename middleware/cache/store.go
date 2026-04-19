package cache

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/goceleris/celeris/middleware/internal/fnv1a"
	"github.com/goceleris/celeris/middleware/store"
)

// MemoryStoreConfig configures the in-memory cache store.
type MemoryStoreConfig struct {
	// Shards is the number of lock shards. Default: runtime.NumCPU(),
	// rounded up to the next power of two.
	Shards int

	// MaxEntries is the total cache entry count across all shards.
	// Zero means unlimited. Per-shard capacity is MaxEntries/Shards
	// rounded up.
	MaxEntries int

	// CleanupInterval is how often expired entries are evicted.
	// Default: 1 minute.
	CleanupInterval time.Duration

	// CleanupContext, when set, stops the cleanup goroutine when the
	// context is cancelled.
	CleanupContext context.Context
}

// MemoryStore is an in-memory, sharded LRU cache. It implements
// [store.KV], [store.PrefixDeleter], and [store.Scanner].
type MemoryStore struct {
	shards  []memShard
	mask    uint64
	perMax  int
	cancel  context.CancelFunc
}

type memShard struct {
	mu       sync.Mutex
	items    map[string]*lruNode
	head     *lruNode // most-recently-used
	tail     *lruNode // least-recently-used
	capacity int
}

type lruNode struct {
	key     string
	value   []byte
	expiry  int64 // UnixNano; 0 means no expiry
	prev    *lruNode
	next    *lruNode
}

// NewMemoryStore returns a new in-memory cache store.
func NewMemoryStore(config ...MemoryStoreConfig) *MemoryStore {
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
	perMax := 0
	if cfg.MaxEntries > 0 {
		perMax = (cfg.MaxEntries + n - 1) / n
	}

	ctx := cfg.CleanupContext
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)

	m := &MemoryStore{
		shards: make([]memShard, n),
		mask:   uint64(n - 1),
		perMax: perMax,
		cancel: cancel,
	}
	for i := range m.shards {
		m.shards[i].items = make(map[string]*lruNode)
		m.shards[i].capacity = perMax
	}

	go m.cleanup(ctx, cfg.CleanupInterval)

	return m
}

// Close stops the cleanup goroutine. Safe to call multiple times.
func (m *MemoryStore) Close() { m.cancel() }

func (m *MemoryStore) shard(key string) *memShard {
	return &m.shards[fnv1a.Hash(key)&m.mask]
}

// Get implements [store.KV]. Moves the accessed entry to the MRU end.
func (m *MemoryStore) Get(_ context.Context, key string) ([]byte, error) {
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.items[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	if n.expiry > 0 && time.Now().UnixNano() > n.expiry {
		s.removeNode(n)
		delete(s.items, key)
		return nil, store.ErrNotFound
	}
	s.moveToFront(n)
	out := make([]byte, len(n.value))
	copy(out, n.value)
	return out, nil
}

// Set implements [store.KV]. Evicts the LRU entry when the shard is
// over capacity.
func (m *MemoryStore) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	cp := make([]byte, len(value))
	copy(cp, value)
	var exp int64
	if ttl > 0 {
		exp = time.Now().Add(ttl).UnixNano()
	}
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.items[key]; ok {
		existing.value = cp
		existing.expiry = exp
		s.moveToFront(existing)
		return nil
	}
	n := &lruNode{key: key, value: cp, expiry: exp}
	s.items[key] = n
	s.pushFront(n)
	if s.capacity > 0 && len(s.items) > s.capacity {
		victim := s.tail
		if victim != nil {
			s.removeNode(victim)
			delete(s.items, victim.key)
		}
	}
	return nil
}

// Delete implements [store.KV].
func (m *MemoryStore) Delete(_ context.Context, key string) error {
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.items[key]; ok {
		s.removeNode(n)
		delete(s.items, key)
	}
	return nil
}

// DeletePrefix implements [store.PrefixDeleter].
func (m *MemoryStore) DeletePrefix(_ context.Context, prefix string) error {
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.Lock()
		for k, n := range s.items {
			if strings.HasPrefix(k, prefix) {
				s.removeNode(n)
				delete(s.items, k)
			}
		}
		s.mu.Unlock()
	}
	return nil
}

// Scan implements [store.Scanner].
func (m *MemoryStore) Scan(_ context.Context, prefix string) ([]string, error) {
	var out []string
	nowNano := time.Now().UnixNano()
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.Lock()
		for k, n := range s.items {
			if n.expiry > 0 && nowNano > n.expiry {
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

func (s *memShard) pushFront(n *lruNode) {
	n.prev = nil
	n.next = s.head
	if s.head != nil {
		s.head.prev = n
	}
	s.head = n
	if s.tail == nil {
		s.tail = n
	}
}

func (s *memShard) moveToFront(n *lruNode) {
	if s.head == n {
		return
	}
	s.removeNode(n)
	s.pushFront(n)
}

func (s *memShard) removeNode(n *lruNode) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		s.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		s.tail = n.prev
	}
	n.prev = nil
	n.next = nil
}

func (m *MemoryStore) cleanup(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	idx := 0
	perTick := 4
	if len(m.shards) < perTick {
		perTick = len(m.shards)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			nowNano := now.UnixNano()
			for range perTick {
				s := &m.shards[idx%len(m.shards)]
				idx++
				s.mu.Lock()
				for k, n := range s.items {
					if n.expiry > 0 && nowNano > n.expiry {
						s.removeNode(n)
						delete(s.items, k)
					}
				}
				s.mu.Unlock()
			}
		}
	}
}
