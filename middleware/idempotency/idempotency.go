// Package idempotency implements the HTTP Idempotency-Key pattern.
//
// When a request carries an Idempotency-Key header, the middleware
// attempts to acquire an atomic lock on the key via [store.SetNXer].
// The first request to acquire the lock runs the handler, persists the
// response under the same key, and releases the lock. Subsequent
// requests with the same key replay the stored response.
//
// Concurrent duplicates that arrive while the original is still
// in-flight return 409 Conflict (configurable via [Config.OnConflict]).
// Crashed handlers leak a lock entry; the lock expires after
// [Config.LockTimeout] so the next request can retry.
//
// When [Config.BodyHash] is enabled, the request body hash is stored
// alongside the response; mismatches on replay return 422
// Unprocessable Entity to catch clients that reuse a key with
// different payloads.
package idempotency

import (
	"crypto/sha256"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/store"
)

// wire format versions
const (
	entryLocked    byte = 1
	entryCompleted byte = 2
)

// lockNonceCounter ensures distinct lock nonces per process.
var lockNonceCounter atomic.Uint64

// New returns an idempotency middleware.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	if cfg.Store == nil {
		cfg.Store = NewMemoryStore()
	}

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	methods := make(map[string]struct{}, len(cfg.Methods))
	for _, m := range cfg.Methods {
		methods[strings.ToUpper(m)] = struct{}{}
	}

	onConflict := cfg.OnConflict
	if onConflict == nil {
		onConflict = func(c *celeris.Context) error {
			return c.AbortWithStatus(409)
		}
	}

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}
		if _, ok := methods[strings.ToUpper(c.Method())]; !ok {
			return c.Next()
		}
		key := c.Header(cfg.KeyHeader)
		if key == "" {
			return c.Next()
		}
		if !validKey(key, cfg.MaxKeyLength) {
			return c.AbortWithStatus(400)
		}

		ctx := c.Context()

		// Compute body hash if enabled — needed for replay mismatch check.
		var bodyHash [32]byte
		if cfg.BodyHash {
			body := c.Body()
			if len(body) > cfg.MaxBodyBytes {
				return c.AbortWithStatus(413)
			}
			bodyHash = sha256.Sum256(body)
		}

		// Check for completed entry first.
		if raw, err := cfg.Store.Get(ctx, key); err == nil && len(raw) > 0 {
			kind := raw[0]
			payload := raw[1:]
			switch kind {
			case entryCompleted:
				// Verify body hash when present.
				if cfg.BodyHash && len(payload) >= 32 {
					storedHash := payload[:32]
					if string(storedHash) != string(bodyHash[:]) {
						return c.AbortWithStatus(422)
					}
					payload = payload[32:]
				}
				rep, derr := store.DecodeResponse(payload)
				if derr == nil {
					c.Abort()
					return replay(c, rep)
				}
				// Corrupt entry — best-effort cleanup and fall through
				// so the handler runs.
				_ = cfg.Store.Delete(ctx, key)
			case entryLocked:
				return onConflict(c)
			}
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			// Store transport error — let the request through to avoid
			// 5xx avalanche; the client can retry.
			return c.Next()
		}

		// Attempt to acquire the lock. Build the 17-byte payload in a
		// single allocation (1 status byte + 16-byte nonce) instead of
		// making a 16-byte nonce and then append-copying it — saves one
		// alloc per attempt.
		lockPayload := makeLockPayload()
		acquired, err := cfg.Store.SetNX(ctx, key, lockPayload, cfg.LockTimeout)
		if err != nil {
			return c.Next()
		}
		if !acquired {
			// Another request got here first. Check once if it has
			// already completed; otherwise conflict.
			if raw, gerr := cfg.Store.Get(ctx, key); gerr == nil && len(raw) > 0 && raw[0] == entryCompleted {
				payload := raw[1:]
				if cfg.BodyHash && len(payload) >= 32 {
					if string(payload[:32]) != string(bodyHash[:]) {
						return c.AbortWithStatus(422)
					}
					payload = payload[32:]
				}
				rep, derr := store.DecodeResponse(payload)
				if derr == nil {
					c.Abort()
					return replay(c, rep)
				}
			}
			return onConflict(c)
		}

		// Leader path — run the handler and capture its response.
		c.BufferResponse()
		chainErr := c.Next()
		status := c.ResponseStatus()
		body := c.ResponseBody()
		if len(body) > cfg.MaxBodyBytes {
			// Body too large to replay — release lock and pass through
			// without caching the response.
			_ = cfg.Store.Delete(ctx, key)
			if ferr := c.FlushResponse(); ferr != nil && chainErr == nil {
				return ferr
			}
			return chainErr
		}

		headers := c.ResponseHeaders()
		enc := store.EncodedResponse{Status: status, Headers: headers, Body: body}
		encoded := enc.Encode()

		var payload []byte
		if cfg.BodyHash {
			payload = make([]byte, 1+32+len(encoded))
			payload[0] = entryCompleted
			copy(payload[1:], bodyHash[:])
			copy(payload[1+32:], encoded)
		} else {
			payload = make([]byte, 1+len(encoded))
			payload[0] = entryCompleted
			copy(payload[1:], encoded)
		}
		// Persist completed entry before flushing the wire so a concurrent
		// follower that wakes mid-flight sees the completed state.
		if chainErr == nil {
			_ = cfg.Store.Set(ctx, key, payload, cfg.TTL)
		} else {
			// Handler errored — release the lock so the client can retry.
			_ = cfg.Store.Delete(ctx, key)
		}

		if ferr := c.FlushResponse(); ferr != nil && chainErr == nil {
			return ferr
		}
		return chainErr
	}
}

// makeLockPayload returns [entryLocked | 16-byte nonce] in one 17-byte
// allocation. The 16-byte nonce combines the process counter with the
// current nanosecond — not cryptographically secure; exists only to
// make lock entries human-distinguishable in debug logs.
func makeLockPayload() []byte {
	n := lockNonceCounter.Add(1)
	out := make([]byte, 17)
	out[0] = entryLocked
	nano := uint64(time.Now().UnixNano())
	for i := 0; i < 8; i++ {
		out[1+i] = byte(n >> (i * 8))
		out[9+i] = byte(nano >> (i * 8))
	}
	return out
}

// validKey returns true if key is 1..maxLen printable ASCII.
func validKey(key string, maxLen int) bool {
	if key == "" || len(key) > maxLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

func replay(c *celeris.Context, rep store.EncodedResponse) error {
	ct := "application/octet-stream"
	for _, h := range rep.Headers {
		if strings.EqualFold(h[0], "content-type") {
			ct = h[1]
			continue
		}
		c.SetHeader(h[0], h[1])
	}
	return c.Blob(rep.Status, ct, rep.Body)
}
