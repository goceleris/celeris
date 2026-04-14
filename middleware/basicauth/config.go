package basicauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"

	"github.com/goceleris/celeris"
)

// UsernameKey is the context store key for the authenticated username.
const UsernameKey = "basicauth_username"

// Config defines the basic auth middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// Validator checks credentials. Required if Users is nil -- panics if both are nil.
	Validator func(user, pass string) bool

	// ValidatorWithContext checks credentials with access to the request context.
	// Takes precedence over Validator when set.
	ValidatorWithContext func(c *celeris.Context, user, pass string) bool

	// Users maps usernames to passwords. When set and Validator is nil,
	// a constant-time validator is auto-generated from this map.
	Users map[string]string

	// HashedUsers maps usernames to opaque hash strings. The format is
	// determined by HashedUsersFunc — bcrypt's $2y$..., argon2id's $argon2..,
	// scrypt, etc. HashedUsersFunc is REQUIRED whenever HashedUsers is
	// non-empty; basicauth.New() panics otherwise. There is no built-in
	// password-hash default because all general-purpose hashes (SHA-2,
	// SHA-3, BLAKE2) are too fast to safely store credentials with.
	HashedUsers map[string]string

	// HashedUsersFunc receives the stored hash string and the plaintext
	// candidate; returns true on match. Required when HashedUsers is set.
	// Callers typically wrap bcrypt.CompareHashAndPassword or
	// argon2.IDKey + subtle.ConstantTimeCompare.
	//
	// IMPORTANT: The function MUST take constant time for any input,
	// including empty or invalid hash strings. For bcrypt, this means
	// pre-computing a dummy hash (via bcrypt.GenerateFromPassword) and
	// comparing against it for unknown users, rather than letting
	// bcrypt.CompareHashAndPassword fail instantly on an empty hash.
	HashedUsersFunc func(hash, password string) bool

	// Realm is the authentication realm. Default: "Restricted".
	Realm string

	// ErrorHandler handles authentication failures. The err parameter is
	// [ErrUnauthorized] for all auth failures.
	// Default: 401 with WWW-Authenticate + Cache-Control + Vary headers.
	ErrorHandler func(c *celeris.Context, err error) error

	// SuccessHandler is called after successful credential validation,
	// before c.Next(). Use for logging, metrics, or enriching the context.
	// Matches the hook exposed by middleware/jwt and middleware/keyauth so
	// mixed auth stacks can enrich uniformly.
	SuccessHandler func(c *celeris.Context)
}

// defaultConfig is the default basic auth configuration.
var defaultConfig = Config{
	Realm: "Restricted",
}

// hmacKey generates a 32-byte cryptographically random key.
func hmacKey() [32]byte {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		panic("basicauth: crypto/rand failed: " + err.Error())
	}
	return key
}

func applyDefaults(cfg Config) Config {
	if cfg.Realm == "" {
		cfg.Realm = defaultConfig.Realm
	}
	if cfg.Validator == nil && cfg.ValidatorWithContext == nil && len(cfg.Users) > 0 {
		type userEntry struct{ mac []byte }
		key := hmacKey()
		entries := make(map[string]userEntry, len(cfg.Users))
		for u, p := range cfg.Users {
			entries[u] = userEntry{mac: hmacSHA256(key[:], []byte(p))}
		}
		dummyMAC := hmacSHA256(key[:], []byte("__celeris_dummy_pw__"))
		cfg.Validator = func(user, pass string) bool {
			e, ok := entries[user]
			inputMAC := hmacSHA256(key[:], []byte(pass))
			if !ok {
				_ = subtle.ConstantTimeCompare(inputMAC, dummyMAC)
				return false
			}
			return subtle.ConstantTimeCompare(inputMAC, e.mac) == 1
		}
	}
	if cfg.Validator == nil && cfg.ValidatorWithContext == nil && len(cfg.HashedUsers) > 0 {
		if cfg.HashedUsersFunc == nil {
			// SHA-256 is fast — adversaries can crack it on commodity GPUs
			// at billions of guesses per second. The default has been
			// removed so callers must wire bcrypt / scrypt / argon2 (or
			// equivalent) explicitly. See package docs for an example.
			panic("basicauth: HashedUsers requires HashedUsersFunc (use bcrypt or argon2; SHA-256 is not credential-grade)")
		}
		hashCopy := make(map[string]string, len(cfg.HashedUsers))
		for u, h := range cfg.HashedUsers {
			hashCopy[u] = h
		}
		var dummyHash string
		for _, h := range hashCopy {
			dummyHash = h
			break
		}
		verifyFn := cfg.HashedUsersFunc
		cfg.Validator = func(user, pass string) bool {
			h, ok := hashCopy[user]
			if !ok {
				verifyFn(dummyHash, pass)
				return false
			}
			return verifyFn(h, pass)
		}
	}
	return cfg
}

// hmacSHA256 computes HMAC-SHA256(key, data) and returns the 32-byte tag.
func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// HashPassword returns the hex-encoded SHA-256 hash of password.
//
// DEPRECATED: SHA-256 is not credential-grade — adversaries can crack it
// at billions of guesses per second on commodity GPUs. Use bcrypt or
// argon2 with [Config.HashedUsersFunc] instead. This helper is retained
// for backwards-compatibility but may be removed in a future major
// release.
func HashPassword(password string) string {
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
}

func (cfg Config) validate() {
	if cfg.Validator == nil && cfg.ValidatorWithContext == nil {
		panic("basicauth: Validator, ValidatorWithContext, Users, or HashedUsers is required")
	}
}
