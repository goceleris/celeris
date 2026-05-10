//go:build !validation

package jwt

import (
	"time"

	"github.com/goceleris/celeris/middleware/jwt/internal/jwtparse"
)

// validateAdmission is the production no-op stub. Inlines to nothing
// — the JWT admit hot path pays zero overhead in production builds.
// The assertion-bearing implementation lives in validation_check.go
// and is compiled under -tags=validation.
func validateAdmission(_ jwtparse.Claims, _ time.Duration) {}
