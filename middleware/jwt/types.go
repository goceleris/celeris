package jwt

import (
	"time"

	"github.com/goceleris/celeris/middleware/jwt/internal/jwtparse"
)

// Token and related type aliases re-exported from the internal parser.
type Token = jwtparse.Token

// Header is the parsed JWT JOSE header (alg, typ, kid, etc.).
type Header = jwtparse.Header

// Claims is the interface satisfied by all JWT claims types.
type Claims = jwtparse.Claims

// MapClaims is an unstructured claims map.
type MapClaims = jwtparse.MapClaims

// RegisteredClaims contains the registered (standard) JWT claim fields.
type RegisteredClaims = jwtparse.RegisteredClaims

// NumericDate represents a JSON numeric date value (Unix timestamp).
type NumericDate = jwtparse.NumericDate

// Audience is a list of recipients the JWT is intended for.
type Audience = jwtparse.Audience

// SigningMethod is the interface for JWT signing algorithms.
type SigningMethod = jwtparse.SigningMethod

// Keyfunc is the callback used to supply the verification key.
type Keyfunc = jwtparse.Keyfunc

// ParserOption configures the JWT parser.
type ParserOption = jwtparse.ParserOption

// Signing method singletons.
var (
	SigningMethodHS256 = jwtparse.SigningMethodHS256
	SigningMethodHS384 = jwtparse.SigningMethodHS384
	SigningMethodHS512 = jwtparse.SigningMethodHS512
	SigningMethodRS256 = jwtparse.SigningMethodRS256
	SigningMethodRS384 = jwtparse.SigningMethodRS384
	SigningMethodRS512 = jwtparse.SigningMethodRS512
	SigningMethodES256 = jwtparse.SigningMethodES256
	SigningMethodES384 = jwtparse.SigningMethodES384
	SigningMethodES512 = jwtparse.SigningMethodES512
	SigningMethodEdDSA = jwtparse.SigningMethodEdDSA
	SigningMethodPS256 = jwtparse.SigningMethodPS256
	SigningMethodPS384 = jwtparse.SigningMethodPS384
	SigningMethodPS512 = jwtparse.SigningMethodPS512
)

// Sentinel errors from the parser.
var (
	ErrTokenMalformed        = jwtparse.ErrTokenMalformed
	ErrTokenExpired          = jwtparse.ErrTokenExpired
	ErrTokenNotValidYet      = jwtparse.ErrTokenNotValidYet
	ErrTokenSignatureInvalid = jwtparse.ErrTokenSignatureInvalid
	ErrAlgNone               = jwtparse.ErrAlgNone
	ErrTokenUnverifiable     = jwtparse.ErrTokenUnverifiable
	ErrTokenUsedBeforeIssued = jwtparse.ErrTokenUsedBeforeIssued
	ErrInvalidIssuer         = jwtparse.ErrInvalidIssuer
	ErrInvalidAudience       = jwtparse.ErrInvalidAudience
	ErrInvalidSubject        = jwtparse.ErrInvalidSubject
)

// Parser option constructors.
var (
	WithValidMethods = jwtparse.WithValidMethods
	WithLeeway       = jwtparse.WithLeeway
	WithIssuer       = jwtparse.WithIssuer
	WithAudience     = jwtparse.WithAudience
	WithSubject      = jwtparse.WithSubject
)

// SignToken creates and signs a new JWT.
func SignToken(method SigningMethod, claims Claims, key any) (string, error) {
	return jwtparse.SignToken(method, claims, key)
}

// NewNumericDate creates a NumericDate from a time.Time.
func NewNumericDate(t time.Time) *NumericDate {
	return jwtparse.NewNumericDate(t)
}
