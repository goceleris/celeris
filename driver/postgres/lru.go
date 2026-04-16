package postgres

import (
	"container/list"
	"sync"

	"github.com/goceleris/celeris/driver/postgres/protocol"
)

// lru is a small string-keyed LRU cache for prepared statements. Zero cap
// disables the cache (every get returns miss, put is a no-op).
type lru struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List
	items map[string]*list.Element
}

type lruEntry struct {
	key string
	val *protocol.PreparedStmt
}

func newLRU(cap int) *lru {
	return &lru{
		cap:   cap,
		ll:    list.New(),
		items: map[string]*list.Element{},
	}
}

func (l *lru) get(k string) (*protocol.PreparedStmt, bool) {
	if l == nil || l.cap <= 0 {
		return nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.items[k]
	if !ok {
		return nil, false
	}
	l.ll.MoveToFront(e)
	return e.Value.(*lruEntry).val, true
}

// put installs k -> v and returns any statements evicted by the insertion.
// Callers typically send Close 'S' + Sync for each evicted name.
func (l *lru) put(k string, v *protocol.PreparedStmt) []*protocol.PreparedStmt {
	if l == nil || l.cap <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.items[k]; ok {
		e.Value.(*lruEntry).val = v
		l.ll.MoveToFront(e)
		return nil
	}
	e := l.ll.PushFront(&lruEntry{key: k, val: v})
	l.items[k] = e
	var evicted []*protocol.PreparedStmt
	for l.ll.Len() > l.cap {
		back := l.ll.Back()
		if back == nil {
			break
		}
		entry := back.Value.(*lruEntry)
		delete(l.items, entry.key)
		l.ll.Remove(back)
		evicted = append(evicted, entry.val)
	}
	return evicted
}

// remove deletes the entry for k from the cache. Returns true if k was
// present.
func (l *lru) remove(k string) bool {
	if l == nil || l.cap <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.items[k]
	if !ok {
		return false
	}
	delete(l.items, k)
	l.ll.Remove(e)
	return true
}

// empty reports whether the cache holds zero entries.
func (l *lru) empty() bool {
	if l == nil || l.cap <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ll.Len() == 0
}

// reset clears the cache. Called on reconnect — named statements do not
// survive backend restarts.
func (l *lru) reset() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.ll.Init()
	l.items = map[string]*list.Element{}
	l.mu.Unlock()
}
