// Package memcachedstore provides a memcached-backed [ratelimit.Store]
// (token-bucket) built on the native celeris [driver/memcached]
// client. Drop-in rival to [middleware/ratelimit/redisstore] for
// deployments that prefer memcached.
//
// Memcached does not support server-side scripting (no Lua EVALSHA),
// so the atomic token-bucket update is implemented via a CAS loop:
// Gets retrieves state + CAS token, the client computes new state,
// then CAS writes it back. On CAS conflict (another goroutine won the
// race) we retry up to [maxCASRetries] times. In the uncontended
// common case this is a single Gets + CAS round trip.
//
// Atomicity is per-key: each key's bucket is updated atomically via
// its own CAS loop. Cross-key operations are independent.
package memcachedstore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/middleware/ratelimit"
)

// maxCASRetries caps the CAS-loop length per Allow/Undo call so
// pathological contention cannot spin indefinitely.
//
// CAS-based rate limiting has an unavoidable contention edge:
// when N goroutines race for a burst of B tokens, losers retry
// through every winning CAS before they see an empty bucket, so
// retries scale with N, not with B. 24 retries absorbs ~20×-burst
// contention without starvation. Exhausted calls return
// (false, err) — safe-by-default since a CAS limiter over-throttles
// under contention but never under-throttles.
const maxCASRetries = 24

// Options configure the memcached-backed rate limit store.
type Options struct {
	// KeyPrefix is prepended to every rate limit key. Default: "rl:".
	KeyPrefix string

	// RPS is the refill rate in tokens per second. Required (> 0).
	RPS float64

	// Burst is the bucket capacity. Required (> 0).
	Burst int

	// TTL is the memcached expiry applied to each bucket key.
	// Default: 2 * (Burst / RPS), minimum 1 minute.
	TTL time.Duration
}

// Store implements [ratelimit.Store] and [ratelimit.StoreUndo].
type Store struct {
	client  *celmc.Client
	prefix  string
	rps     float64
	burst   int
	ttl     time.Duration
	retries atomic.Uint64 // total CAS retries across all calls
}

var (
	_ ratelimit.Store     = (*Store)(nil)
	_ ratelimit.StoreUndo = (*Store)(nil)
)

// New constructs a Store. Returns an error on invalid options.
func New(client *celmc.Client, opts Options) (*Store, error) {
	if opts.RPS <= 0 {
		return nil, errors.New("ratelimit/memcachedstore: Options.RPS must be > 0")
	}
	if opts.Burst <= 0 {
		return nil, errors.New("ratelimit/memcachedstore: Options.Burst must be > 0")
	}
	if opts.KeyPrefix == "" {
		opts.KeyPrefix = "rl:"
	}
	if opts.TTL <= 0 {
		opts.TTL = time.Duration(float64(opts.Burst)/opts.RPS*2) * time.Second
		if opts.TTL < time.Minute {
			opts.TTL = time.Minute
		}
	}
	return &Store{
		client: client,
		prefix: opts.KeyPrefix,
		rps:    opts.RPS,
		burst:  opts.Burst,
		ttl:    opts.TTL,
	}, nil
}

// bucketState is encoded to memcached as a fixed 16-byte blob:
// [8 bytes: last (unix nanos, int64 big-endian)][8 bytes: tokens×1e6
// (int64 big-endian)]. Using micro-tokens lets us keep a float-like
// resolution without decoding ASCII on every Allow.
type bucketState struct {
	last   int64
	tokens float64
}

func encodeBucket(s bucketState) []byte {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[0:8], uint64(s.last))
	binary.BigEndian.PutUint64(buf[8:16], uint64(int64(s.tokens*1e6)))
	return buf[:]
}

func decodeBucket(raw []byte) (bucketState, error) {
	if len(raw) != 16 {
		return bucketState{}, fmt.Errorf("ratelimit/memcachedstore: expected 16-byte bucket blob, got %d", len(raw))
	}
	return bucketState{
		last:   int64(binary.BigEndian.Uint64(raw[0:8])),
		tokens: float64(int64(binary.BigEndian.Uint64(raw[8:16]))) / 1e6,
	}, nil
}

// Allow implements [ratelimit.Store].
func (s *Store) Allow(key string) (bool, int, time.Time, error) {
	ctx := context.Background()
	full := s.prefix + key
	now := time.Now().UnixNano()

	for attempt := 0; attempt <= maxCASRetries; attempt++ {
		item, err := s.client.Gets(ctx, full)
		cas := uint64(0)
		state := bucketState{last: now, tokens: float64(s.burst)}
		if err == nil {
			cas = item.CAS
			if d, derr := decodeBucket(item.Value); derr == nil {
				state = d
			}
		} else if !errors.Is(err, celmc.ErrCacheMiss) {
			return false, 0, time.Time{}, err
		}

		// Refill.
		if elapsed := float64(now-state.last) / 1e9; elapsed > 0 {
			state.tokens += elapsed * s.rps
			if state.tokens > float64(s.burst) {
				state.tokens = float64(s.burst)
			}
		}
		allowed := false
		if state.tokens >= 1 {
			state.tokens--
			allowed = true
		}
		state.last = now

		blob := encodeBucket(state)
		if cas == 0 {
			// No existing entry — use Add so another concurrent
			// initializer doesn't overwrite us.
			if addErr := s.client.Add(ctx, full, blob, s.ttl); addErr != nil {
				if errors.Is(addErr, celmc.ErrNotStored) {
					// Someone else won the init; retry as an update.
					s.retries.Add(1)
					continue
				}
				return false, 0, time.Time{}, addErr
			}
		} else {
			ok, casErr := s.client.CAS(ctx, full, blob, cas, s.ttl)
			if casErr != nil {
				return false, 0, time.Time{}, casErr
			}
			if !ok {
				s.retries.Add(1)
				continue
			}
		}

		missing := 1 - state.tokens
		if missing < 0 {
			missing = 0
		}
		resetAt := time.Unix(0, now+int64(missing*1e9/s.rps))
		return allowed, int(state.tokens), resetAt, nil
	}

	// Fell through retry cap. Deny the request — safer under
	// contention than allowing a potentially-overdrawn bucket.
	return false, 0, time.Time{}, fmt.Errorf("ratelimit/memcachedstore: CAS loop exhausted after %d attempts on key %q", maxCASRetries+1, key)
}

// Undo implements [ratelimit.StoreUndo]. Returns one token to the
// bucket, capped at Burst. Uses the same CAS loop as Allow.
func (s *Store) Undo(key string) error {
	ctx := context.Background()
	full := s.prefix + key
	for attempt := 0; attempt <= maxCASRetries; attempt++ {
		item, err := s.client.Gets(ctx, full)
		if err != nil {
			if errors.Is(err, celmc.ErrCacheMiss) {
				return nil // nothing to undo
			}
			return err
		}
		state, derr := decodeBucket(item.Value)
		if derr != nil {
			return derr
		}
		state.tokens++
		if state.tokens > float64(s.burst) {
			state.tokens = float64(s.burst)
		}
		blob := encodeBucket(state)
		ok, casErr := s.client.CAS(ctx, full, blob, item.CAS, s.ttl)
		if casErr != nil {
			return casErr
		}
		if ok {
			return nil
		}
		s.retries.Add(1)
	}
	return fmt.Errorf("ratelimit/memcachedstore: Undo CAS loop exhausted for key %q", key)
}

// RetriesTotal returns the number of CAS retries observed since
// construction. Useful for monitoring contention.
func (s *Store) RetriesTotal() uint64 { return s.retries.Load() }
