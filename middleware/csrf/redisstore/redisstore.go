// Package redisstore provides a [store.KV] CSRF token backend built on
// the native celeris [driver/redis] client. Tokens are persisted as
// their UTF-8 bytes under a configurable key prefix (default "csrf:").
//
// The returned store implements [store.GetAndDeleter] via Redis GETDEL
// (Redis 6.2+). Callers with older Redis deployments can opt into a
// non-atomic GET+DEL fallback via [Options.OldRedisCompat].
package redisstore

import (
	"context"
	"errors"
	"sync"
	"time"
	"unsafe"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/store"
)

// keyBufSlot carries a reusable []byte slab for prefix+key assembly so
// each Get/Set/Delete can skip the per-call heap alloc from the
// `s.prefix+key` string concat. Hand the Redis client an unsafe.String
// view — safe because driver/redis writes the key bytes to its RESP
// buffer synchronously and does not retain them.
type keyBufSlot struct {
	buf []byte
}

var keyBufPool = sync.Pool{
	New: func() any { return &keyBufSlot{buf: make([]byte, 0, 64)} },
}

func acquireFullKey(prefix, key string) (string, *keyBufSlot) {
	slot := keyBufPool.Get().(*keyBufSlot)
	slot.buf = append(slot.buf[:0], prefix...)
	slot.buf = append(slot.buf, key...)
	return unsafe.String(unsafe.SliceData(slot.buf), len(slot.buf)), slot
}

func releaseFullKey(slot *keyBufSlot) {
	if slot == nil {
		return
	}
	slot.buf = slot.buf[:0]
	keyBufPool.Put(slot)
}

// Options configure the Redis-backed CSRF store.
type Options struct {
	// KeyPrefix is prepended to every token key. Default: "csrf:".
	KeyPrefix string

	// OldRedisCompat enables a non-atomic GET+DEL fallback for
	// GetAndDelete on Redis versions that do not support GETDEL
	// (< 6.2). When true, single-use token validation is TOCTOU-
	// susceptible under concurrent requests with the same token.
	OldRedisCompat bool
}

// Store is a [store.KV] + [store.GetAndDeleter] backed by a
// [*redis.Client].
type Store struct {
	client  *redis.Client
	prefix  string
	oldMode bool
}

// New returns a new Redis-backed CSRF store.
func New(client *redis.Client, opts ...Options) *Store {
	o := Options{KeyPrefix: "csrf:"}
	if len(opts) > 0 {
		if opts[0].KeyPrefix != "" {
			o.KeyPrefix = opts[0].KeyPrefix
		}
		o.OldRedisCompat = opts[0].OldRedisCompat
	}
	return &Store{client: client, prefix: o.KeyPrefix, oldMode: o.OldRedisCompat}
}

// Get implements [store.KV].
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	full, slot := acquireFullKey(s.prefix, key)
	v, err := s.client.GetBytes(ctx, full)
	releaseFullKey(slot)
	if errors.Is(err, redis.ErrNil) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

// Set implements [store.KV].
func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	full, slot := acquireFullKey(s.prefix, key)
	err := s.client.SetBytes(ctx, full, value, ttl)
	releaseFullKey(slot)
	return err
}

// Delete implements [store.KV].
func (s *Store) Delete(ctx context.Context, key string) error {
	full, slot := acquireFullKey(s.prefix, key)
	_, err := s.client.Del(ctx, full)
	releaseFullKey(slot)
	return err
}

// GetAndDelete implements [store.GetAndDeleter] atomically via GETDEL
// (Redis 6.2+). When [Options.OldRedisCompat] is true, falls back to
// a non-atomic GET+DEL pair.
func (s *Store) GetAndDelete(ctx context.Context, key string) ([]byte, error) {
	full, slot := acquireFullKey(s.prefix, key)
	if s.oldMode {
		v, err := s.client.GetBytes(ctx, full)
		if errors.Is(err, redis.ErrNil) {
			releaseFullKey(slot)
			return nil, store.ErrNotFound
		}
		if err != nil {
			releaseFullKey(slot)
			return nil, err
		}
		_, _ = s.client.Del(ctx, full)
		releaseFullKey(slot)
		return v, nil
	}
	v, err := s.client.GetDelBytes(ctx, full)
	releaseFullKey(slot)
	if errors.Is(err, redis.ErrNil) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}
