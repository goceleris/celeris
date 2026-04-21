package jwtparse

import "errors"

// Sentinel errors for JWT validation failures.
var (
	ErrTokenMalformed        = errors.New("token is malformed")
	ErrTokenUnverifiable     = errors.New("token is unverifiable")
	ErrTokenSignatureInvalid = errors.New("token signature is invalid")
	ErrTokenExpired          = errors.New("token has expired")
	ErrTokenNotValidYet      = errors.New("token is not valid yet")
	ErrTokenUsedBeforeIssued = errors.New("token used before issued")
	ErrAlgNone               = errors.New("\"none\" algorithm is not allowed")
	ErrInvalidIssuer         = errors.New("token has invalid issuer")
	ErrInvalidAudience       = errors.New("token has invalid audience")
	ErrInvalidSubject        = errors.New("token has invalid subject")
)

// wrapErr pairs a sentinel (outer) with an inner cause. Replaces
// fmt.Errorf("%w: %w", sentinel, err) on the parse hot paths
// (keyFunc failure, signature verify failure). fmt.Errorf with
// double-%w costs ~4 allocs (printer state, msg string, wrapErrors
// struct, []error unwrap slice); wrapErr is one struct alloc and
// materializes the concat lazily when Error() is called.
//
// Matches the classifiedError pattern used by middleware/jwt.
type wrapErr struct {
	outer error
	inner error
}

func (w *wrapErr) Error() string {
	return w.outer.Error() + ": " + w.inner.Error()
}

// Is returns true when target matches either side. Avoids Unwrap()
// []error which would cost a slice alloc per errors.Is call.
func (w *wrapErr) Is(target error) bool {
	if errors.Is(w.outer, target) {
		return true
	}
	return errors.Is(w.inner, target)
}

// Unwrap returns the outer sentinel so errors.As can find it.
func (w *wrapErr) Unwrap() error { return w.outer }
