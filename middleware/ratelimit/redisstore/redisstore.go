// Package redisstore provides a Redis-backed [ratelimit.Store]
// (token-bucket) built on the native celeris [driver/redis] client.
//
// The token-bucket state is stored as a Redis hash {tokens, last} per
// key; atomic updates are performed by an embedded Lua script loaded
// via SCRIPT LOAD at [New] time. EVALSHA is used on the hot path; on
// NOSCRIPT (script flushed from server cache) the script is reloaded
// once and the call is retried.
//
// Atomicity is per-key: a single key's bucket is updated atomically.
// Cross-key operations are independent.
package redisstore

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/driver/redis/protocol"
	"github.com/goceleris/celeris/middleware/ratelimit"
)

//go:embed redisstore.lua
var luaTokenBucket string

const luaUndo = `-- celeris-ratelimit: undo (give one token back, capped at burst)
local key   = KEYS[1]
local burst = tonumber(ARGV[1])
local ttl   = tonumber(ARGV[2])
local tokens = tonumber(redis.call('HGET', key, 'tokens'))
if tokens == nil then
    return 0
end
tokens = math.min(burst, tokens + 1)
redis.call('HSET', key, 'tokens', tokens)
redis.call('EXPIRE', key, ttl)
return 1
`

// Options configure the Redis-backed rate limit store.
type Options struct {
	// KeyPrefix is prepended to every rate limit key. Default: "rl:".
	KeyPrefix string

	// RPS is the refill rate in tokens/second. Required (> 0).
	RPS float64

	// Burst is the bucket capacity (maximum tokens). Required (> 0).
	Burst int

	// TTL is the Redis expiry applied to each bucket key. Default:
	// 2 * (Burst / RPS), minimum 1 minute.
	TTL time.Duration
}

// Store implements [ratelimit.Store] and [ratelimit.StoreUndo].
type Store struct {
	client *redis.Client
	prefix string
	rps    float64
	burst  int
	ttlSec int64

	// Pre-boxed static EVALSHA args — RPS/Burst/TTL are immutable after
	// construction, so their string + interface boxing happens once here
	// rather than on every Allow/Undo call.
	argRPS    any
	argBurst  any
	argTTLSec any

	allowSHA atomic.Value // string
	undoSHA  atomic.Value // string
}

var (
	_ ratelimit.Store     = (*Store)(nil)
	_ ratelimit.StoreUndo = (*Store)(nil)
)

// New constructs a Store and preloads the Lua scripts. Returns an error
// if script loading fails.
func New(ctx context.Context, client *redis.Client, opts Options) (*Store, error) {
	if opts.RPS <= 0 {
		return nil, errors.New("ratelimit/redisstore: Options.RPS must be > 0")
	}
	if opts.Burst <= 0 {
		return nil, errors.New("ratelimit/redisstore: Options.Burst must be > 0")
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
	ttlSec := int64(opts.TTL.Seconds())
	s := &Store{
		client:    client,
		prefix:    opts.KeyPrefix,
		rps:       opts.RPS,
		burst:     opts.Burst,
		ttlSec:    ttlSec,
		argRPS:    strconv.FormatFloat(opts.RPS, 'g', -1, 64),
		argBurst:  strconv.Itoa(opts.Burst),
		argTTLSec: strconv.FormatInt(ttlSec, 10),
	}
	if err := s.loadScripts(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) loadScripts(ctx context.Context) error {
	sha, err := s.client.ScriptLoad(ctx, luaTokenBucket)
	if err != nil {
		return fmt.Errorf("ratelimit/redisstore: SCRIPT LOAD tokenBucket: %w", err)
	}
	s.allowSHA.Store(sha)
	uSha, err := s.client.ScriptLoad(ctx, luaUndo)
	if err != nil {
		return fmt.Errorf("ratelimit/redisstore: SCRIPT LOAD undo: %w", err)
	}
	s.undoSHA.Store(uSha)
	return nil
}

// Allow implements [ratelimit.Store]. A passed key is prefixed with
// the configured KeyPrefix before hitting Redis.
func (s *Store) Allow(key string) (bool, int, time.Time, error) {
	ctx := context.Background()
	return s.allow(ctx, key)
}

func (s *Store) allow(ctx context.Context, key string) (bool, int, time.Time, error) {
	fullKey := s.prefix + key
	now := time.Now().UnixNano()
	// Static RPS/Burst/TTL args are pre-boxed in Store fields, so only
	// the per-call `now` string is allocated + boxed here.
	args := []any{
		strconv.FormatInt(now, 10),
		s.argRPS,
		s.argBurst,
		s.argTTLSec,
	}

	sha, _ := s.allowSHA.Load().(string)
	val, err := s.client.EvalSHA(ctx, sha, []string{fullKey}, args...)
	if err != nil && isNoScript(err) {
		if rerr := s.loadScripts(ctx); rerr != nil {
			return false, 0, time.Time{}, rerr
		}
		sha, _ = s.allowSHA.Load().(string)
		val, err = s.client.EvalSHA(ctx, sha, []string{fullKey}, args...)
	}
	if err != nil {
		return false, 0, time.Time{}, err
	}
	allowed, remaining, resetNs, derr := decodeAllowResult(val)
	if derr != nil {
		return false, 0, time.Time{}, derr
	}
	return allowed, remaining, time.Unix(0, resetNs), nil
}

// Undo implements [ratelimit.StoreUndo] — returns one token to the
// bucket, capped at Burst. Called when a request was permitted but
// ultimately should not count (e.g. SkipFailedRequests semantics).
func (s *Store) Undo(key string) error {
	ctx := context.Background()
	fullKey := s.prefix + key
	args := []any{s.argBurst, s.argTTLSec}
	sha, _ := s.undoSHA.Load().(string)
	_, err := s.client.EvalSHA(ctx, sha, []string{fullKey}, args...)
	if err != nil && isNoScript(err) {
		if rerr := s.loadScripts(ctx); rerr != nil {
			return rerr
		}
		sha, _ = s.undoSHA.Load().(string)
		_, err = s.client.EvalSHA(ctx, sha, []string{fullKey}, args...)
	}
	return err
}

func isNoScript(err error) bool {
	var re *redis.RedisError
	if errors.As(err, &re) {
		return re.Prefix == "NOSCRIPT"
	}
	// Some RESP parsers return a plain error string — match by substring.
	return err != nil && containsNoScript(err.Error())
}

func containsNoScript(s string) bool {
	for i := 0; i+8 <= len(s); i++ {
		if s[i:i+8] == "NOSCRIPT" {
			return true
		}
	}
	return false
}

// decodeAllowResult expects a 3-element array: {allowed, remaining, reset_ns}.
// Redis Lua integer returns arrive as protocol.TyInt; Lua numbers arrive
// as TyBulk (string) in some RESP2 contexts. Handle both.
func decodeAllowResult(v *protocol.Value) (bool, int, int64, error) {
	if v == nil || v.Type != protocol.TyArray || len(v.Array) != 3 {
		return false, 0, 0, fmt.Errorf("ratelimit/redisstore: unexpected reply shape: %+v", v)
	}
	allowed, err := toInt(v.Array[0])
	if err != nil {
		return false, 0, 0, err
	}
	remaining, err := toInt(v.Array[1])
	if err != nil {
		return false, 0, 0, err
	}
	resetNs, err := toInt(v.Array[2])
	if err != nil {
		return false, 0, 0, err
	}
	return allowed == 1, int(remaining), resetNs, nil
}

func toInt(v protocol.Value) (int64, error) {
	switch v.Type {
	case protocol.TyInt:
		return v.Int, nil
	case protocol.TyBulk, protocol.TySimple, protocol.TyVerbatim:
		n, err := parseInt(string(v.Str))
		if err != nil {
			return 0, fmt.Errorf("ratelimit/redisstore: cannot parse integer %q: %w", v.Str, err)
		}
		return n, nil
	}
	return 0, fmt.Errorf("ratelimit/redisstore: unexpected value type %v", v.Type)
}

func parseInt(s string) (int64, error) {
	var n int64
	var neg bool
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	if i == len(s) {
		return 0, errors.New("empty")
	}
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, errors.New("non-digit")
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}
