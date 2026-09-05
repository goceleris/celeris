package basicauth

import (
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// --- HashPasswordPBKDF2 / VerifyPassword (celeris#503) ---
//
// Every derivation below costs 600k HMAC-SHA256 iterations (~0.2s native,
// seconds under -race), so the tests share one pre-computed hash where the
// salt does not matter, keep their case tables tight, and run in parallel.

// secretHash returns a single pbkdf2-sha256 hash of "secret", computed once
// per test binary.
var secretHash = sync.OnceValue(func() string { return HashPasswordPBKDF2("secret") })

func TestHashPasswordPBKDF2RoundTrip(t *testing.T) {
	t.Parallel()
	h := secretHash()
	if !VerifyPassword(h, "secret") {
		t.Fatalf("VerifyPassword rejected the password it was derived from: %q", h)
	}
}

func TestHashPasswordPBKDF2WrongPasswordRejected(t *testing.T) {
	t.Parallel()
	h := secretHash()
	// Note: "secret\x00" is deliberately absent. PBKDF2 feeds the password
	// in as the HMAC key, and HMAC zero-pads keys shorter than the block
	// size, so a trailing NUL is a documented PBKDF2 equivalence rather
	// than a verifier bug.
	for _, pw := range []string{"wrong", "", "Secret"} {
		if VerifyPassword(h, pw) {
			t.Fatalf("VerifyPassword accepted wrong password %q for %q", pw, h)
		}
	}
}

func TestHashPasswordPBKDF2OutputFormat(t *testing.T) {
	t.Parallel()
	h := secretHash()
	parts := strings.Split(h, "$")
	if len(parts) != 4 {
		t.Fatalf("want 4 $-separated fields, got %d in %q", len(parts), h)
	}
	if parts[0] != "pbkdf2-sha256" {
		t.Fatalf("algorithm tag: got %q, want %q", parts[0], "pbkdf2-sha256")
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("iterations field %q not an integer: %v", parts[1], err)
	}
	if iter != PBKDF2Iterations || PBKDF2Iterations != 600000 {
		t.Fatalf("iterations: got %d (const %d), want 600000", iter, PBKDF2Iterations)
	}
	salt, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("salt field %q not base64: %v", parts[2], err)
	}
	if len(salt) != 16 {
		t.Fatalf("salt length: got %d bytes, want 16", len(salt))
	}
	key, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		t.Fatalf("hash field %q not base64: %v", parts[3], err)
	}
	if len(key) != 32 {
		t.Fatalf("derived key length: got %d bytes, want 32", len(key))
	}

	// A salted hash must not be deterministic: two hashes of the same
	// password share nothing but the tag and iteration count.
	h2 := HashPasswordPBKDF2("secret")
	if h2 == h {
		t.Fatalf("two HashPasswordPBKDF2 calls produced identical output (unsalted?): %q", h)
	}
	if strings.Split(h2, "$")[2] == parts[2] {
		t.Fatalf("salt reused across calls: %q", parts[2])
	}
}

func TestVerifyPasswordTamperedRejected(t *testing.T) {
	t.Parallel()
	h := secretHash()
	parts := strings.Split(h, "$")
	if len(parts) != 4 {
		t.Fatalf("want 4 fields, got %d in %q", len(parts), h)
	}
	join := func(tag, iter, salt, key string) string {
		return tag + "$" + iter + "$" + salt + "$" + key
	}
	flipFirst := func(s string) string {
		// Swap the first character for a different valid base64 char so
		// the field still decodes but to different bytes.
		if s[0] == 'A' {
			return "B" + s[1:]
		}
		return "A" + s[1:]
	}
	saltRaw, _ := base64.StdEncoding.DecodeString(parts[2])
	shortSalt := base64.StdEncoding.EncodeToString(saltRaw[:8])
	keyRaw, _ := base64.StdEncoding.DecodeString(parts[3])
	shortKey := base64.StdEncoding.EncodeToString(keyRaw[:16])

	cases := map[string]string{
		"fewer iterations":       join(parts[0], "1000", parts[2], parts[3]),
		"more iterations":        join(parts[0], "600001", parts[2], parts[3]),
		"zero iterations":        join(parts[0], "0", parts[2], parts[3]),
		"negative iterations":    join(parts[0], "-600000", parts[2], parts[3]),
		"non-numeric iterations": join(parts[0], "abc", parts[2], parts[3]),
		"flipped salt":           join(parts[0], parts[1], flipFirst(parts[2]), parts[3]),
		"truncated salt":         join(parts[0], parts[1], shortSalt, parts[3]),
		"invalid base64 salt":    join(parts[0], parts[1], "!!!!", parts[3]),
		"flipped hash":           join(parts[0], parts[1], parts[2], flipFirst(parts[3])),
		"truncated hash":         join(parts[0], parts[1], parts[2], shortKey),
		"missing field":          parts[0] + "$" + parts[1] + "$" + parts[2],
		"extra field":            h + "$extra",
		"wrong algorithm tag":    join("pbkdf2-sha512", parts[1], parts[2], parts[3]),
		"tag only":               "pbkdf2-sha256$",
		"empty":                  "",
	}
	for name, tampered := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel() // most cases cost a full 600k-iteration derivation
			if VerifyPassword(tampered, "secret") {
				t.Fatalf("VerifyPassword accepted tampered hash %q", tampered)
			}
		})
	}
}

func TestVerifyPasswordLegacySHA256(t *testing.T) {
	t.Parallel()
	legacy := HashPassword("secret")
	if !VerifyPassword(legacy, "secret") {
		t.Fatalf("legacy sha256 hash %q no longer verifies", legacy)
	}
	if VerifyPassword(legacy, "wrong") {
		t.Fatal("legacy sha256 hash accepted wrong password")
	}
	// hex is case-insensitive; upper-case hex is the same digest.
	if !VerifyPassword(strings.ToUpper(legacy), "secret") {
		t.Fatal("upper-case legacy hex hash should verify")
	}
	for _, bad := range []string{
		legacy[:63],       // truncated
		legacy + "00",     // too long
		"zz" + legacy[2:], // not hex
	} {
		if VerifyPassword(bad, "secret") {
			t.Fatalf("malformed legacy hash %q accepted", bad)
		}
	}
	// HashPassword's behaviour is unchanged: still the plain hex digest.
	if _, err := hex.DecodeString(legacy); err != nil || len(legacy) != 64 {
		t.Fatalf("HashPassword output changed: %q", legacy)
	}
}

// TestHashedUsersPBKDF2Default: a HashedUsers map containing only
// pbkdf2-sha256 hashes no longer needs an explicit HashedUsersFunc —
// VerifyPassword is wired in by default.
func TestHashedUsersPBKDF2Default(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		HashedUsers: map[string]string{"admin": secretHash()},
	})
	tests := []struct {
		name     string
		user     string
		pass     string
		wantCode int
	}{
		{"valid", "admin", "secret", 200},
		{"wrong password", "admin", "wrong", 401},
		{"unknown user", "nobody", "secret", 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var storedUser string
			handler := func(c *celeris.Context) error {
				storedUser = UsernameFromContext(c)
				return c.String(200, "ok")
			}
			rec, err := testutil.RunChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/",
				celeristest.WithBasicAuth(tt.user, tt.pass))
			if tt.wantCode == 200 {
				testutil.AssertNoError(t, err)
				testutil.AssertStatus(t, rec, 200)
				if storedUser != tt.user {
					t.Fatalf("stored user: got %q, want %q", storedUser, tt.user)
				}
			} else {
				testutil.AssertHTTPError(t, err, tt.wantCode)
			}
		})
	}
}

// Legacy sha256 hashes (or anything else) without a HashedUsersFunc must
// still panic — the default is only safe when every hash is a slow KDF.
func TestHashedUsersLegacyWithoutFuncStillPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic: mixed legacy sha256 + pbkdf2 store without HashedUsersFunc")
		}
	}()
	New(Config{HashedUsers: map[string]string{
		"admin": secretHash(),
		"old":   HashPassword("legacy"),
	}})
}

// Mixed stores migrate incrementally: VerifyPassword accepts both formats.
func TestHashedUsersVerifyPasswordMixedStore(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		HashedUsers: map[string]string{
			"new": secretHash(),
			"old": HashPassword("legacy"),
		},
		HashedUsersFunc: VerifyPassword,
	})
	for _, tt := range []struct {
		name, user, pass string
		wantCode         int
	}{
		{"pbkdf2 entry", "new", "secret", 200},
		{"legacy entry", "old", "legacy", 200},
		{"pbkdf2 entry wrong password", "new", "legacy", 401},
		{"legacy entry wrong password", "old", "secret", 401},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			handler := func(c *celeris.Context) error { return c.String(200, "ok") }
			rec, err := testutil.RunChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/",
				celeristest.WithBasicAuth(tt.user, tt.pass))
			if tt.wantCode == 200 {
				testutil.AssertNoError(t, err)
				testutil.AssertStatus(t, rec, 200)
			} else {
				testutil.AssertHTTPError(t, err, tt.wantCode)
			}
		})
	}
}
