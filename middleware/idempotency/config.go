package idempotency

import (
	"context"
	"errors"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/store"
)

// KVStore combines [store.KV] with [store.SetNXer] — idempotency
// requires both. The in-memory default satisfies this out of the box.
// Backends without SetNX cannot be used as an idempotency store.
type KVStore interface {
	store.KV
	store.SetNXer
}

// Config defines the idempotency middleware configuration.
type Config struct {
	// Store persists completed responses and lock entries. Default:
	// [NewMemoryStore]. Must implement [store.KV] + [store.SetNXer].
	Store KVStore

	// KeyHeader is the request header carrying the client-supplied
	// idempotency key. Default: "Idempotency-Key".
	KeyHeader string

	// TTL is the lifetime of a completed response entry. Default: 24h.
	TTL time.Duration

	// LockTimeout is the maximum duration a lock is held before it
	// expires (recovers after a crashed handler). Default: 30s.
	LockTimeout time.Duration

	// Methods lists HTTP methods the middleware applies to. Default:
	// POST, PUT, PATCH, DELETE. Methods outside this list pass through.
	Methods []string

	// OnConflict runs when a duplicate request arrives while the
	// original is still in-flight (lock held, no completed entry yet).
	// Default: respond with 409 Conflict.
	OnConflict func(*celeris.Context) error

	// MaxKeyLength is the upper bound for the key header value.
	// Default: 255.
	MaxKeyLength int

	// BodyHash, when true, hashes the request body (up to MaxBodyBytes)
	// and compares it to the stored hash on replay. Mismatches return
	// 422 Unprocessable Entity. Default: false (hash checking disabled).
	BodyHash bool

	// MaxBodyBytes caps the bytes hashed for BodyHash and the bytes
	// stored in a completed response. Default: 1 MiB.
	MaxBodyBytes int

	// Skip defines a function to skip this middleware for certain
	// requests.
	Skip func(*celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// CleanupContext stops the MemoryStore cleanup goroutine (no effect
	// on Redis/Postgres stores).
	CleanupContext context.Context
}

// ErrStoreMissingSetNX is returned by [New] when Config.Store is set
// but does not satisfy [KVStore].
var ErrStoreMissingSetNX = errors.New("idempotency: store must implement store.SetNXer")

var defaultConfig = Config{
	KeyHeader:    "Idempotency-Key",
	TTL:          24 * time.Hour,
	LockTimeout:  30 * time.Second,
	Methods:      []string{"POST", "PUT", "PATCH", "DELETE"},
	MaxKeyLength: 255,
	MaxBodyBytes: 1 << 20,
}

func applyDefaults(cfg Config) Config {
	if cfg.KeyHeader == "" {
		cfg.KeyHeader = defaultConfig.KeyHeader
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultConfig.TTL
	}
	if cfg.LockTimeout <= 0 {
		cfg.LockTimeout = defaultConfig.LockTimeout
	}
	if len(cfg.Methods) == 0 {
		cfg.Methods = defaultConfig.Methods
	}
	if cfg.MaxKeyLength <= 0 {
		cfg.MaxKeyLength = defaultConfig.MaxKeyLength
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultConfig.MaxBodyBytes
	}
	return cfg
}
