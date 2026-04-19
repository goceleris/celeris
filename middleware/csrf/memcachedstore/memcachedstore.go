// Package memcachedstore provides a [store.KV] CSRF token backend
// built on the native celeris [driver/memcached] client. Drop-in
// rival to [middleware/csrf/redisstore] for deployments that prefer
// memcached.
//
// Memcached has no atomic GETDEL. Single-use token validation
// (Config.SingleUseToken=true) falls back to a [Client.Gets] +
// [Client.CAS] zero-set sequence: Gets returns a CAS token, then
// we attempt a CAS-guarded Delete. Under concurrent single-use
// attempts, only one delete wins — the other observes CAS conflict
// and returns [store.ErrNotFound], which is exactly the TOCTOU-safe
// behavior csrf.go already expects.
package memcachedstore

import (
	"context"
	"errors"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/middleware/store"
)

// Options configure the memcached-backed CSRF store.
type Options struct {
	// KeyPrefix is prepended to every token key. Default: "csrf:".
	KeyPrefix string
}

// Store is a [store.KV] + [store.GetAndDeleter] backed by memcached.
type Store struct {
	client *celmc.Client
	prefix string
}

var _ store.GetAndDeleter = (*Store)(nil)

// New returns a new memcached-backed CSRF store.
func New(client *celmc.Client, opts ...Options) *Store {
	o := Options{KeyPrefix: "csrf:"}
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

// Set implements [store.KV].
func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return s.client.Set(ctx, s.prefix+key, value, ttl)
}

// Delete implements [store.KV].
func (s *Store) Delete(ctx context.Context, key string) error {
	err := s.client.Delete(ctx, s.prefix+key)
	if errors.Is(err, celmc.ErrCacheMiss) {
		return nil
	}
	return err
}

// GetAndDelete implements [store.GetAndDeleter] with CAS so concurrent
// single-use token redemptions cannot both observe success.
//
// Flow: Gets → CAS a sentinel with the captured token + ttl=1s so the
// value visibly disappears shortly, then issue a best-effort Delete to
// free the slot immediately. If the CAS fails, another redemption won
// the race; we return [store.ErrNotFound] so the caller rejects the
// token as already-consumed (same semantic as a true atomic GETDEL).
func (s *Store) GetAndDelete(ctx context.Context, key string) ([]byte, error) {
	full := s.prefix + key
	item, err := s.client.Gets(ctx, full)
	if errors.Is(err, celmc.ErrCacheMiss) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// CAS a single-byte sentinel with 1 second TTL — the CAS protects
	// against a concurrent winner; the short TTL ensures the slot is
	// eventually freed even if the follow-up Delete is never reached.
	ok, err := s.client.CAS(ctx, full, []byte{0}, item.CAS, time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, store.ErrNotFound
	}
	_ = s.client.Delete(ctx, full)
	out := make([]byte, len(item.Value))
	copy(out, item.Value)
	return out, nil
}
