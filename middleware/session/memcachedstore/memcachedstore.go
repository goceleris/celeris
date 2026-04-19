// Package memcachedstore provides a [store.KV] session backend built on
// the native celeris [driver/memcached] client. A drop-in rival to the
// Redis-backed [middleware/session/redisstore] for deployments that
// prefer memcached.
//
// Memcached has no key-enumeration protocol (no SCAN), so the returned
// store implements [store.KV] only — not [store.Scanner] or
// [store.PrefixDeleter]. The session middleware's Reset path becomes a
// documented no-op with memcached; callers who need bulk-expiry can
// call [memcached.Client.Flush] directly or shard sessions across a
// short-lived prefix they retire by rotating the prefix.
package memcachedstore

import (
	"context"
	"errors"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/middleware/store"
)

// Options configure the memcached-backed session store.
type Options struct {
	// KeyPrefix is prepended to every session id. Memcached keys must
	// be 1..250 bytes with no whitespace/control bytes, so callers
	// should pick a short prefix. Default: "sess:".
	KeyPrefix string
}

// Store is a [store.KV] backed by a [*celmc.Client].
type Store struct {
	client *celmc.Client
	prefix string
}

// New returns a new memcached-backed session store.
func New(client *celmc.Client, opts ...Options) *Store {
	o := Options{KeyPrefix: "sess:"}
	if len(opts) > 0 && opts[0].KeyPrefix != "" {
		o.KeyPrefix = opts[0].KeyPrefix
	}
	return &Store{client: client, prefix: o.KeyPrefix}
}

// Get implements [store.KV].
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	v, err := s.client.GetBytes(ctx, s.prefix+key)
	if errors.Is(err, celmc.ErrCacheMiss) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

// Set implements [store.KV]. ttl <= 0 means "no expiry" from
// memcached's perspective (0 is the magic "never expire" value).
func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return s.client.Set(ctx, s.prefix+key, value, ttl)
}

// Delete implements [store.KV]. Memcached's DELETE returns
// ErrCacheMiss when the key is absent; we treat that as a successful
// no-op to match the [store.KV] contract ("Delete returning nil
// means the key is not present regardless of whether it was present
// before the call").
func (s *Store) Delete(ctx context.Context, key string) error {
	err := s.client.Delete(ctx, s.prefix+key)
	if errors.Is(err, celmc.ErrCacheMiss) {
		return nil
	}
	return err
}
