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

	// HashedUsers maps usernames to hex-encoded SHA-256 password hashes.
	// When set and Validator, ValidatorWithContext, and Users are all nil,
	// a constant-time validator is auto-generated that SHA-256 hashes the
	// incoming password and compares it against the stored hash.
	// Use [HashPassword] to produce the hex-encoded hash values.
	HashedUsers map[string]string

	// HashedUsersFunc, when non-nil, replaces the built-in SHA-256
	// comparison used by HashedUsers. It receives the stored hex-encoded
	// hash and the plaintext password and returns true if they match.
	// This lets callers plug in bcrypt, argon2, or any other password
	// hashing algorithm without this package importing those libraries.
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
		if cfg.HashedUsersFunc != nil {
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
		} else {
			type hashedEntry struct{ hashBytes [sha256.Size]byte }
			entries := make(map[string]hashedEntry, len(cfg.HashedUsers))
			for u, h := range cfg.HashedUsers {
				b, err := hex.DecodeString(h)
				if err != nil || len(b) != sha256.Size {
					panic("basicauth: HashedUsers[" + u + "]: invalid hex-encoded SHA-256 hash")
				}
				var entry hashedEntry
				copy(entry.hashBytes[:], b)
				entries[u] = entry
			}
			var dummyHash [sha256.Size]byte
			if _, err := rand.Read(dummyHash[:]); err != nil {
				panic("basicauth: crypto/rand failed: " + err.Error())
			}
			cfg.Validator = func(user, pass string) bool {
				e, ok := entries[user]
				passHash := sha256.Sum256([]byte(pass))
				if !ok {
					_ = subtle.ConstantTimeCompare(passHash[:], dummyHash[:])
					return false
				}
				return subtle.ConstantTimeCompare(passHash[:], e.hashBytes[:]) == 1
			}
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
// Use this to produce values for [Config].HashedUsers.
func HashPassword(password string) string {
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
}

func (cfg Config) validate() {
	if cfg.Validator == nil && cfg.ValidatorWithContext == nil {
		panic("basicauth: Validator, ValidatorWithContext, Users, or HashedUsers is required")
	}
}
