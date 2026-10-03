package basicauth

import (
	"crypto/sha256"
	"encoding/hex"
)

// sha256Hex returns the bare hex SHA-256 digest of s, the stored-hash shape
// the removed HashPassword produced (celeris#826). Test-only: a fast,
// unsalted digest is not a credential hash.
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
