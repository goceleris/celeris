//go:build validation

package jwt

import (
	"encoding/json"
	"time"

	"github.com/goceleris/celeris/middleware/jwt/internal/jwtparse"
	"github.com/goceleris/celeris/validation"
)

// validateAdmission asserts the invariant exp + leeway >= now() at
// the moment the JWT middleware admits the request. The library's
// parser already rejects expired tokens before they reach the admit
// site, applying any configured WithLeeway to the comparison; this
// check exists to catch the race window between the parser's
// time.Now() read and the c.Next() dispatch, plus the catastrophic
// case of an exp claim that the parser somehow ignored (e.g. a
// custom claims type that returns nil from Valid).
//
// Late admits are bumped on validation.JWTLateAdmits so the
// jwt-late-admit predicate observes the discrepancy.
func validateAdmission(c jwtparse.Claims, leeway time.Duration) {
	if c == nil {
		return
	}
	now := time.Now()
	switch v := c.(type) {
	case *jwtparse.RegisteredClaims:
		if v.ExpiresAt != nil && now.After(v.ExpiresAt.Time.Add(leeway)) {
			validation.JWTLateAdmits.Add(1)
		}
	case jwtparse.MapClaims:
		exp, ok := mapClaimsExp(v)
		if !ok {
			return
		}
		if now.Unix() > exp+int64(leeway.Seconds()) {
			validation.JWTLateAdmits.Add(1)
		}
	}
}

// mapClaimsExp pulls the exp claim from a MapClaims as int64 seconds
// since epoch. JSON-decoded numerics arrive as float64; encoding/json
// with a UseNumber decoder yields json.Number; explicit constructions
// accept int / int64 too. Returns ok=false when the claim is absent
// or carries an unexpected type — in either case the assertion is a
// no-op (the harness's "no late admit" predicate degrades to a
// vacuous truth when exp is unset).
func mapClaimsExp(m jwtparse.MapClaims) (int64, bool) {
	v, ok := m["exp"]
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, true
		}
		if f, err := t.Float64(); err == nil {
			return int64(f), true
		}
		return 0, false
	default:
		return 0, false
	}
}
