package basicauth

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// --- HashPasswordPBKDF2 / VerifyPassword (celeris#503) ---
//
// Every VerifyPassword call costs one 600k-iteration derivation (~0.2s
// native, seconds under -race) — legacy digests, "" and malformed input
// included, by design — so the tests share one pre-computed hash where the
// salt does not matter, keep their case tables tight, probe the parser
// window directly where a derivation would prove nothing extra, and run in
// parallel. TestVerifyPasswordCostUniform is the deliberate exception: it
// measures, so it runs alone.

// secretHash returns a single pbkdf2-sha256 hash of "secret", computed once
// per test binary.
var secretHash = sync.OnceValue(func() string { return HashPasswordPBKDF2("secret") })

// mkPBKDF2 builds a pbkdf2-sha256 string for password with explicit
// parameters, bypassing HashPasswordPBKDF2's fixed defaults, so tests can
// probe the accepted window with hashes whose derived key is correct.
func mkPBKDF2(t *testing.T, password string, iter int, salt []byte) string {
	t.Helper()
	key, err := pbkdf2.Key(sha256.New, password, salt, iter, pbkdf2KeyLen)
	if err != nil {
		t.Fatalf("pbkdf2.Key(iter=%d, salt=%d bytes): %v", iter, len(salt), err)
	}
	return pbkdf2Tag + "$" + strconv.Itoa(iter) + "$" +
		base64.StdEncoding.EncodeToString(salt) + "$" +
		base64.StdEncoding.EncodeToString(key)
}

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
	tinySalt := base64.StdEncoding.EncodeToString(saltRaw[:1])
	keyRaw, _ := base64.StdEncoding.DecodeString(parts[3])
	shortKey := base64.StdEncoding.EncodeToString(keyRaw[:16])

	cases := map[string]string{
		// An in-window parameter change parses fine and fails on the
		// derived key. The window's edges are pinned by TestParsePBKDF2Window.
		"more iterations (key mismatch)": join(parts[0], "600001", parts[2], parts[3]),
		// Out-of-window parameters are refused before any derivation
		// against them and take the burn-then-false path.
		"below-floor iterations": join(parts[0], "599999", parts[2], parts[3]),
		"downgraded iterations":  join(parts[0], "1000", parts[2], parts[3]),
		"31-bit max iterations":  join(parts[0], "2147483647", parts[2], parts[3]),
		"zero iterations":        join(parts[0], "0", parts[2], parts[3]),
		"negative iterations":    join(parts[0], "-600000", parts[2], parts[3]),
		"non-numeric iterations": join(parts[0], "abc", parts[2], parts[3]),
		"8-byte salt":            join(parts[0], parts[1], shortSalt, parts[3]),
		"1-byte salt":            join(parts[0], parts[1], tinySalt, parts[3]),
		"flipped salt":           join(parts[0], parts[1], flipFirst(parts[2]), parts[3]),
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
			t.Parallel() // every case costs a full 600k-iteration derivation
			if VerifyPassword(tampered, "secret") {
				t.Fatalf("VerifyPassword accepted tampered hash %q", tampered)
			}
		})
	}
}

// TestParsePBKDF2Window pins the edges of the parameter window without
// paying for a derivation per case: iterations in [600,000, 10,000,000], a
// salt of at least 16 bytes, a key of exactly 32 bytes, strict decimal and
// strict (padded) base64. The probes that used to parse — 2147483647
// iterations, 1 iteration, a 1-byte salt — are all here.
func TestParsePBKDF2Window(t *testing.T) {
	t.Parallel()
	b64 := func(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }
	salt, key := b64(pbkdf2SaltLen), b64(pbkdf2KeyLen)
	mk := func(iter, salt, key string) string { return pbkdf2Tag + "$" + iter + "$" + salt + "$" + key }

	cases := []struct {
		name string
		hash string
		ok   bool
		iter int
	}{
		{"default", mk("600000", salt, key), true, 600000},
		{"floor", mk(strconv.Itoa(minPBKDF2Iterations), salt, key), true, minPBKDF2Iterations},
		{"cap", mk("10000000", salt, key), true, 10000000},
		{"one above default", mk("600001", salt, key), true, 600001},
		{"below floor", mk(strconv.Itoa(minPBKDF2Iterations-1), salt, key), false, 0},
		{"RFC 8018 minimum", mk("1000", salt, key), false, 0},
		{"one", mk("1", salt, key), false, 0},
		{"zero", mk("0", salt, key), false, 0},
		{"above cap", mk("10000001", salt, key), false, 0},
		{"31-bit max", mk("2147483647", salt, key), false, 0},
		{"32-bit max", mk("4294967295", salt, key), false, 0},
		{"64-bit max", mk("18446744073709551615", salt, key), false, 0},
		{"plus sign", mk("+600000", salt, key), false, 0},
		{"minus sign", mk("-600000", salt, key), false, 0},
		{"leading space", mk(" 600000", salt, key), false, 0},
		{"underscore", mk("600_000", salt, key), false, 0},
		{"hex", mk("0x927C0", salt, key), false, 0},
		{"exponent", mk("6e5", salt, key), false, 0},
		{"empty iterations", mk("", salt, key), false, 0},
		{"17-byte salt", mk("600000", b64(17), key), true, 600000},
		{"15-byte salt", mk("600000", b64(15), key), false, 0},
		{"8-byte salt", mk("600000", b64(8), key), false, 0},
		{"1-byte salt", mk("600000", b64(1), key), false, 0},
		{"empty salt", mk("600000", "", key), false, 0},
		{"unpadded base64 salt", mk("600000", strings.TrimRight(salt, "="), key), false, 0},
		{"31-byte key", mk("600000", salt, b64(31)), false, 0},
		{"33-byte key", mk("600000", salt, b64(33)), false, 0},
		{"empty key", mk("600000", salt, ""), false, 0},
	}
	for _, tc := range cases {
		iter, s, k, ok := parsePBKDF2(tc.hash)
		if ok != tc.ok {
			t.Errorf("%s: parsePBKDF2(%q) ok=%v, want %v", tc.name, tc.hash, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if iter != tc.iter {
			t.Errorf("%s: iterations = %d, want %d", tc.name, iter, tc.iter)
		}
		if len(s) < minPBKDF2SaltLen || len(k) != pbkdf2KeyLen {
			t.Errorf("%s: accepted salt of %d bytes / key of %d bytes", tc.name, len(s), len(k))
		}
	}

	// HashPasswordPBKDF2's own parameters must sit inside the window it
	// is verified against, or the default wiring would reject its output.
	// (Also enforced at compile time in pbkdf2.go; this is the readable
	// failure.)
	if PBKDF2Iterations < minPBKDF2Iterations || PBKDF2Iterations > maxPBKDF2Iterations ||
		pbkdf2SaltLen < minPBKDF2SaltLen {
		t.Fatalf("HashPasswordPBKDF2 defaults (iter=%d, salt=%d) fall outside the accepted window",
			PBKDF2Iterations, pbkdf2SaltLen)
	}
}

// The window is a real bound on the derivation, not just on the parser:
// a hash with the correct key at in-window non-default parameters
// verifies, and the same key one step outside the window does not.
func TestVerifyPasswordWindowEdges(t *testing.T) {
	t.Parallel()
	salt := make([]byte, pbkdf2SaltLen)
	for i := range salt {
		salt[i] = byte(i + 1)
	}
	t.Run("in-window count above the default verifies", func(t *testing.T) {
		t.Parallel()
		h := mkPBKDF2(t, "secret", PBKDF2Iterations+1, salt)
		if !VerifyPassword(h, "secret") {
			t.Fatalf("in-window hash rejected: %q", h)
		}
	})
	t.Run("correct key one iteration below the floor is refused", func(t *testing.T) {
		t.Parallel()
		h := mkPBKDF2(t, "secret", minPBKDF2Iterations-1, salt)
		if VerifyPassword(h, "secret") {
			t.Fatalf("downgraded iteration count accepted with a correct key: %q", h)
		}
	})
	t.Run("correct key with one salt byte too few is refused", func(t *testing.T) {
		t.Parallel()
		h := mkPBKDF2(t, "secret", PBKDF2Iterations, salt[:minPBKDF2SaltLen-1])
		if VerifyPassword(h, "secret") {
			t.Fatalf("%d-byte salt accepted with a correct key: %q", minPBKDF2SaltLen-1, h)
		}
	})
}

func TestVerifyPasswordLegacySHA256(t *testing.T) {
	t.Parallel()
	legacy := HashPassword("secret")
	// HashPassword's behaviour is unchanged: still the plain hex digest.
	if _, err := hex.DecodeString(legacy); err != nil || len(legacy) != 64 {
		t.Fatalf("HashPassword output changed: %q", legacy)
	}
	cases := []struct {
		name, hash, pass string
		want             bool
	}{
		{"valid", legacy, "secret", true},
		{"wrong password", legacy, "wrong", false},
		{"upper-case hex", strings.ToUpper(legacy), "secret", true}, // hex is case-insensitive
		{"truncated", legacy[:63], "secret", false},
		{"too long", legacy + "00", "secret", false},
		{"not hex", "zz" + legacy[2:], "secret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel() // every legacy verification burns a default-cost derivation
			if got := VerifyPassword(tc.hash, tc.pass); got != tc.want {
				t.Fatalf("VerifyPassword(%q, %q) = %v, want %v", tc.hash, tc.pass, got, tc.want)
			}
		})
	}
}

// TestVerifyPasswordCostUniform pins the HashedUsersFunc contract for the
// built-in verifier: a legacy digest, an empty hash (what callers pass for
// unknown users) and a hostile out-of-window pbkdf2 string must all cost
// about one default derivation, the same as a genuine pbkdf2-sha256 entry.
// Before the legacy path burned a derivation, "" and hex digests returned
// in microseconds against ~170 ms for a pbkdf2 hash — a 10^5x gap that
// sorted usernames into {legacy, pbkdf2, unknown} by response time. The
// bound is deliberately loose (10x) so scheduler noise cannot trip it
// while any regression of that kind still does; a lost iteration cap
// would show up here as a multi-minute stall instead. Runs serially
// because it measures.
func TestVerifyPasswordCostUniform(t *testing.T) {
	if testing.Short() {
		t.Skip("four full derivations; skipped under -short")
	}
	h := secretHash()
	parts := strings.Split(h, "$")
	hostile := parts[0] + "$2147483647$" + parts[2] + "$" + parts[3]
	legacy := HashPassword("secret")

	measure := func(hash string, want bool) time.Duration {
		start := time.Now()
		got := VerifyPassword(hash, "secret")
		d := time.Since(start)
		if got != want {
			t.Fatalf("VerifyPassword(%q, \"secret\") = %v, want %v", hash, got, want)
		}
		return d
	}
	samples := map[string]time.Duration{
		"pbkdf2":        measure(h, true),
		"legacy":        measure(legacy, true),
		"empty":         measure("", false),
		"hostile count": measure(hostile, false),
	}
	fastest, slowest := time.Duration(1<<62), time.Duration(0)
	for _, d := range samples {
		fastest, slowest = min(fastest, d), max(slowest, d)
	}
	t.Logf("VerifyPassword cost by stored-hash shape: %v", samples)
	if slowest > 10*fastest {
		t.Fatalf("VerifyPassword cost is not uniform across hash formats (fastest %v, slowest %v): %v",
			fastest, slowest, samples)
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

// An auto-wired store (no HashedUsersFunc) holding a pbkdf2-sha256 entry
// outside the accepted window fails at New, naming the entry, rather than
// silently answering 401 to that user on every request.
func TestHashedUsersOutOfWindowPBKDF2Panics(t *testing.T) {
	t.Parallel()
	parts := strings.Split(secretHash(), "$")
	rewrite := func(iter string) string { return parts[0] + "$" + iter + "$" + parts[2] + "$" + parts[3] }
	for name, typo := range map[string]string{
		"above cap":   rewrite("60000000"), // one zero too many
		"below floor": rewrite("60000"),    // one zero too few
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var msg string
			func() {
				defer func() {
					if r := recover(); r != nil {
						msg, _ = r.(string)
					}
				}()
				New(Config{HashedUsers: map[string]string{
					"admin": secretHash(),
					"typo":  typo,
				}})
			}()
			if msg == "" {
				t.Fatalf("expected panic: auto-wired store with out-of-window hash %q", typo)
			}
			if !strings.Contains(msg, `"typo"`) || !strings.Contains(msg, "pbkdf2-sha256") {
				t.Fatalf("panic should name the entry and the format, got: %q", msg)
			}
		})
	}
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

// pickDummyHash prefers a pbkdf2-sha256 entry and otherwise breaks ties on
// username, so the unknown-user path never depends on map-iteration order.
// Repeated because a single map walk could hit the right order by luck.
func TestPickDummyHash(t *testing.T) {
	t.Parallel()
	pb := secretHash()
	if got := pickDummyHash(nil); got != "" {
		t.Fatalf("pickDummyHash(nil) = %q, want \"\"", got)
	}
	for range 16 {
		mixed := map[string]string{"a": HashPassword("a"), "m": pb, "z": HashPassword("z")}
		if got := pickDummyHash(mixed); got != pb {
			t.Fatalf("mixed store: pickDummyHash = %q, want the pbkdf2-sha256 entry", got)
		}
		legacyOnly := map[string]string{"zed": HashPassword("z"), "amy": HashPassword("a"), "bob": HashPassword("b")}
		if got := pickDummyHash(legacyOnly); got != HashPassword("a") {
			t.Fatalf("legacy-only store: pickDummyHash = %q, want the entry of the smallest username", got)
		}
	}
}

// In a mixed store the unknown-user path must hand the verifier a
// pbkdf2-sha256 entry, not whichever value map iteration yields first, so
// a caller-supplied verifier that dispatches on the tag pays a
// deterministic cost on a miss. Repeated so a lucky iteration order
// cannot pass it (three legacy entries to one pbkdf2: 4^-32 by chance).
func TestHashedUsersUnknownUserDummyPrefersPBKDF2(t *testing.T) {
	t.Parallel()
	pb := secretHash()
	handler := func(c *celeris.Context) error { return c.String(200, "ok") }
	for i := range 32 {
		var seen string
		mw := New(Config{
			HashedUsers: map[string]string{
				"old1": HashPassword("a"),
				"old2": HashPassword("b"),
				"old3": HashPassword("c"),
				"new":  pb,
			},
			HashedUsersFunc: func(hash, _ string) bool { seen = hash; return false },
		})
		_, err := testutil.RunChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/",
			celeristest.WithBasicAuth("nobody", "x"))
		testutil.AssertHTTPError(t, err, 401)
		if seen != pb {
			t.Fatalf("iteration %d: unknown-user dummy hash = %q, want the pbkdf2-sha256 entry", i, seen)
		}
	}
}
