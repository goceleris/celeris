// Package redisstore provides a [store.KV] session backend built on
// the native celeris [driver/redis] client. Sessions are persisted as
// JSON payloads under a configurable key prefix (default "sess:").
//
// The returned store also implements [store.Scanner] and
// [store.PrefixDeleter] via SCAN + DEL, which the session middleware
// uses for Reset operations.
package redisstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/store"
)

// Options configure the Redis-backed session store.
type Options struct {
	// KeyPrefix is prepended to every session id when reading/writing
	// Redis. Default: "sess:".
	KeyPrefix string

	// ScanCount is the COUNT hint passed to SCAN on Reset/DeletePrefix
	// iterations. Default: 100.
	ScanCount int64
}

// Store is a [store.KV] backed by a [*redis.Client]. It also satisfies
// [store.Scanner] and [store.PrefixDeleter].
type Store struct {
	client *redis.Client
	prefix string
	count  int64
}

// New returns a new Redis-backed session store.
func New(client *redis.Client, opts ...Options) *Store {
	o := Options{KeyPrefix: "sess:", ScanCount: 100}
	if len(opts) > 0 {
		if opts[0].KeyPrefix != "" {
			o.KeyPrefix = opts[0].KeyPrefix
		}
		if opts[0].ScanCount > 0 {
			o.ScanCount = opts[0].ScanCount
		}
	}
	if o.ScanCount <= 0 {
		o.ScanCount = 100
	}
	return &Store{client: client, prefix: o.KeyPrefix, count: o.ScanCount}
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

// Scan implements [store.Scanner].
func (s *Store) Scan(ctx context.Context, prefix string) ([]string, error) {
	pattern := s.prefix + prefix + "*"
	it := s.client.Scan(ctx, pattern, s.count)
	var out []string
	for {
		k, ok := it.Next(ctx)
		if !ok {
			break
		}
		out = append(out, strings.TrimPrefix(k, s.prefix))
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeletePrefix implements [store.PrefixDeleter] via SCAN + DEL pipelines.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) error {
	pattern := s.prefix + prefix + "*"
	it := s.client.Scan(ctx, pattern, s.count)
	batch := make([]string, 0, 64)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, err := s.client.Del(ctx, batch...)
		batch = batch[:0]
		return err
	}
	for {
		k, ok := it.Next(ctx)
		if !ok {
			break
		}
		batch = append(batch, k)
		if len(batch) >= 256 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := it.Err(); err != nil {
		return err
	}
	return flush()
}
