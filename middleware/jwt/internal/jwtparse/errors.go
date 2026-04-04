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
