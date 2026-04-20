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
	"sync"
	"time"
	"unsafe"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/store"
)

// keyBufSlot carries a reusable []byte slab for `prefix+key` assembly so
// the per-call s.prefix+key string concat can be avoided. A slot's buf
// is owned by the *keyBufSlot pointer; Get/Put round-trips the same
// pointer so no slice-header alloc happens per release. The full key is
// handed to the Redis client as an unsafe.String view — safe because
// driver/redis writes the string bytes to its RESP buffer synchronously
// and does not retain them past the call.
type keyBufSlot struct {
	buf []byte
}

var keyBufPool = sync.Pool{
	New: func() any { return &keyBufSlot{buf: make([]byte, 0, 64)} },
}

// acquireFullKey assembles prefix+key into a pooled slab and returns
// both the view-into-slab string and the slot pointer for release.
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
