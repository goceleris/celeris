package basicauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"

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
	// non-empty, with one exception: when every value carries the
	// "pbkdf2-sha256$" tag of [HashPasswordPBKDF2] and parses within the
	// window [VerifyPassword] accepts, VerifyPassword is wired in
	// automatically. basicauth.New() panics otherwise — naming the entry
	// when a tagged value is malformed or out of window. There is no
	// fast-hash default because all general-purpose hashes (SHA-2, SHA-3,
	// BLAKE2) are too fast to safely store credentials with.
	HashedUsers map[string]string

	// HashedUsersFunc receives the stored hash string and the plaintext
	// candidate; returns true on match. Required when HashedUsers is set
	// (unless all hashes are pbkdf2-sha256, see HashedUsers). Callers
	// typically pass [VerifyPassword] or wrap bcrypt.CompareHashAndPassword
	// or argon2.IDKey + subtle.ConstantTimeCompare.
	//
	// IMPORTANT: The function MUST take constant time for any input,
	// including empty or invalid hash strings. For bcrypt, this means
	// pre-computing a dummy hash (via bcrypt.GenerateFromPassword) and
	// comparing against it for unknown users, rather than letting
	// bcrypt.CompareHashAndPassword fail instantly on an empty hash.
	// [VerifyPassword] meets this: it performs one PBKDF2 derivation for
	// every input, whatever format the stored hash is in.
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
			if !allPBKDF2(cfg.HashedUsers) {
				// SHA-256 is fast — adversaries can crack it on commodity
				// GPUs at billions of guesses per second. There is no
				// fast-hash default: callers must wire VerifyPassword,
				// bcrypt / scrypt / argon2 (or equivalent) explicitly.
				// See package docs for the migration path.
				panic("basicauth: HashedUsers requires HashedUsersFunc unless every hash is pbkdf2-sha256 " +
					"(use HashPasswordPBKDF2 + VerifyPassword, bcrypt, or argon2; plain SHA-256 is not credential-grade)")
			}
			if u, bad := malformedPBKDF2Entry(cfg.HashedUsers); bad {
				// A tagged value VerifyPassword cannot honour would 401
				// that user on every request; fail at startup instead and
				// say which entry.
				panic("basicauth: HashedUsers entry for " + strconv.Quote(u) + " is not a valid pbkdf2-sha256 hash " +
					"(want pbkdf2-sha256$<iter>$<salt-b64>$<hash-b64> with " +
					strconv.Itoa(minPBKDF2Iterations) + " <= iter <= " + strconv.Itoa(maxPBKDF2Iterations) +
					", salt of at least " + strconv.Itoa(minPBKDF2SaltLen) + " bytes, hash of exactly " +
					strconv.Itoa(pbkdf2KeyLen) + " bytes)")
			}
			// Every hash is a slow, salted KDF inside the window we
			// enforce, so a built-in verifier is safe here.
			cfg.HashedUsersFunc = VerifyPassword
		}
		hashCopy := make(map[string]string, len(cfg.HashedUsers))
		for u, h := range cfg.HashedUsers {
			hashCopy[u] = h
		}
		dummyHash := pickDummyHash(hashCopy)
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

// pickDummyHash chooses the stored hash the auto-generated Validator
// verifies unknown usernames against. It is a real stored value so a
// caller-supplied verifier (bcrypt, argon2) pays its genuine cost on a
// miss. A pbkdf2-sha256 entry is preferred when the store is mixed —
// VerifyPassword costs the same for every format, but a custom verifier
// that dispatches on the tag may not — and ties break on username so the
// choice does not depend on map-iteration order. Returns "" for an empty
// map.
func pickDummyHash(hashes map[string]string) string {
	var best, bestUser string
	bestRank := -1
	for u, h := range hashes {
		rank := 0
		if isPBKDF2Hash(h) {
			rank = 1
		}
		if rank > bestRank || (rank == bestRank && u < bestUser) {
			best, bestUser, bestRank = h, u, rank
		}
	}
	return best
}

// hmacSHA256 computes HMAC-SHA256(key, data) and returns the 32-byte tag.
func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// HashPassword returns the hex-encoded SHA-256 hash of password.
//
// Deprecated: an unsalted, fast SHA-256 digest is not a credential-storage
// hash — identical passwords share a digest and it is brute-forceable at
// GPU speed (CodeQL go/weak-sensitive-data-hashing, celeris#503). Use
// [HashPasswordPBKDF2] to produce new hashes; [VerifyPassword] accepts both
// formats so existing stores can migrate one entry at a time. This helper's
// behaviour is frozen for backwards-compatibility and it may be removed in
// a future major release.
func HashPassword(password string) string {
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
}

func (cfg Config) validate() {
	if cfg.Validator == nil && cfg.ValidatorWithContext == nil {
		panic("basicauth: Validator, ValidatorWithContext, Users, or HashedUsers is required")
	}
}
