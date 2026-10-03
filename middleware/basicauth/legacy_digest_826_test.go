package basicauth

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// sha256OfSecret is SHA-256("secret") in lower-case hex, written out so the
// fixture is pinned independently of sha256Hex (sha256hex_test.go).
const sha256OfSecret = "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b"

// TestVerifyPasswordRejectsBareSHA256Digest826 pins celeris#826: with
// HashPassword gone, nothing in celeris produces a bare SHA-256 digest, and
// VerifyPassword accepts only the pbkdf2-sha256 format of HashPasswordPBKDF2.
// The digest tried is the real SHA-256 of the password offered, so it is
// rejected for its format and not for a mismatch; the pbkdf2 rows show the
// verifier still accepts a good hash, so a VerifyPassword that rejected
// everything could not pass.
func TestVerifyPasswordRejectsBareSHA256Digest826(t *testing.T) {
	t.Parallel()
	if got := sha256Hex("secret"); got != sha256OfSecret {
		t.Fatalf("fixture: sha256Hex(\"secret\") = %q, want %q", got, sha256OfSecret)
	}
	for _, tc := range []struct {
		name, hash, pass string
		want             bool
	}{
		{"pbkdf2-sha256, right password", secretHash(), "secret", true},
		{"pbkdf2-sha256, wrong password", secretHash(), "wrong", false},
		{"bare SHA-256 digest of the password", sha256OfSecret, "secret", false},
		{"bare SHA-256 digest, upper-case hex", strings.ToUpper(sha256OfSecret), "secret", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel() // every verification costs a default-cost derivation
			if got := VerifyPassword(tc.hash, tc.pass); got != tc.want {
				t.Fatalf("VerifyPassword(%q, %q) = %v, want %v", tc.hash, tc.pass, got, tc.want)
			}
		})
	}
}

// TestHashedUsersBareSHA256EntryRejected826 is the same through the
// middleware: a store wired to VerifyPassword answers 401 for an entry holding
// a bare SHA-256 digest, even with the right password, and keeps serving its
// pbkdf2-sha256 entries.
func TestHashedUsersBareSHA256EntryRejected826(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		HashedUsers: map[string]string{
			"new": secretHash(),
			"old": sha256Hex("legacy"),
		},
		HashedUsersFunc: VerifyPassword,
	})
	for _, tt := range []struct {
		name, user, pass string
		wantCode         int
	}{
		{"pbkdf2 entry", "new", "secret", 200},
		{"pbkdf2 entry, wrong password", "new", "legacy", 401},
		{"bare SHA-256 entry, right password", "old", "legacy", 401},
		{"bare SHA-256 entry, wrong password", "old", "secret", 401},
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
