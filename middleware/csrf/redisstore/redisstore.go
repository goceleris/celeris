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
	"time"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/store"
)

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
	v, err := s.client.GetBytes(ctx, s.prefix+key)
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
	return s.client.SetBytes(ctx, s.prefix+key, value, ttl)
}

// Delete implements [store.KV].
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.Del(ctx, s.prefix+key)
	return err
}

// GetAndDelete implements [store.GetAndDeleter] atomically via GETDEL
// (Redis 6.2+). When [Options.OldRedisCompat] is true, falls back to
// a non-atomic GET+DEL pair.
func (s *Store) GetAndDelete(ctx context.Context, key string) ([]byte, error) {
	full := s.prefix + key
	if s.oldMode {
		v, err := s.client.GetBytes(ctx, full)
		if errors.Is(err, redis.ErrNil) {
			return nil, store.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		_, _ = s.client.Del(ctx, full)
		return v, nil
	}
	v, err := s.client.GetDel(ctx, full)
	if errors.Is(err, redis.ErrNil) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return []byte(v), nil
}
