//go:build validation

package jwt

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/goceleris/celeris/middleware/jwt/internal/jwtparse"
	"github.com/goceleris/celeris/validation"
)

// TestValidateAdmissionExpiredBumpsCounter exercises the late-admit
// detection: a token whose exp is in the past beyond the leeway must
// bump validation.JWTLateAdmits.
func TestValidateAdmissionExpiredBumpsCounter(t *testing.T) {
	before := validation.JWTLateAdmits.Load()

	exp := time.Now().Add(-2 * time.Minute)
	claims := &jwtparse.RegisteredClaims{
		ExpiresAt: jwtparse.NewNumericDate(exp),
	}
	validateAdmission(claims, time.Minute)

	after := validation.JWTLateAdmits.Load()
	if after < before+1 {
		t.Fatalf("JWTLateAdmits: got %d, want >=%d (before=%d)", after, before+1, before)
	}
}

// TestValidateAdmissionWithinLeewayQuiet confirms the leeway window
// is honoured: a token whose exp is in the past but within the leeway
// is admitted without bumping the counter.
func TestValidateAdmissionWithinLeewayQuiet(t *testing.T) {
	before := validation.JWTLateAdmits.Load()

	exp := time.Now().Add(-30 * time.Second)
	claims := &jwtparse.RegisteredClaims{
		ExpiresAt: jwtparse.NewNumericDate(exp),
	}
	validateAdmission(claims, time.Minute) // 30s past < 60s leeway

	after := validation.JWTLateAdmits.Load()
	if after != before {
		t.Fatalf("JWTLateAdmits within leeway: got %d, want %d (no bump)", after, before)
	}
}

// TestValidateAdmissionFutureClaim confirms a healthy token (exp in
// the future) does not bump the counter regardless of leeway.
func TestValidateAdmissionFutureClaim(t *testing.T) {
	before := validation.JWTLateAdmits.Load()

	exp := time.Now().Add(1 * time.Hour)
	claims := &jwtparse.RegisteredClaims{
		ExpiresAt: jwtparse.NewNumericDate(exp),
	}
	validateAdmission(claims, 0)
	validateAdmission(claims, time.Minute)

	after := validation.JWTLateAdmits.Load()
	if after != before {
		t.Fatalf("JWTLateAdmits healthy: got %d, want %d (no bump)", after, before)
	}
}

// TestValidateAdmissionMapClaimsExpired covers the MapClaims path
// (JSON-decoded numerics arrive as float64).
func TestValidateAdmissionMapClaimsExpired(t *testing.T) {
	before := validation.JWTLateAdmits.Load()

	claims := jwtparse.MapClaims{
		"exp": float64(time.Now().Add(-2 * time.Minute).Unix()),
	}
	validateAdmission(claims, time.Minute)

	after := validation.JWTLateAdmits.Load()
	if after < before+1 {
		t.Fatalf("JWTLateAdmits MapClaims: got %d, want >=%d (before=%d)", after, before+1, before)
	}
}

// TestMapClaimsExpJSONNumber confirms json.Number-typed exp claims
// resolve correctly. Decoders configured with UseNumber yield
// json.Number rather than float64; the late-admit check must still
// observe the value.
func TestMapClaimsExpJSONNumber(t *testing.T) {
	before := validation.JWTLateAdmits.Load()

	expSec := time.Now().Add(-2 * time.Minute).Unix()
	claims := jwtparse.MapClaims{
		"exp": json.Number(strconv.FormatInt(expSec, 10)),
	}
	validateAdmission(claims, time.Minute)

	after := validation.JWTLateAdmits.Load()
	if after < before+1 {
		t.Fatalf("JWTLateAdmits json.Number: got %d, want >=%d (before=%d)", after, before+1, before)
	}
}
