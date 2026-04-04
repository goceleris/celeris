package jwtparse

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// maxTokenSize is the maximum allowed length of a raw JWT token string (16KB).
const maxTokenSize = 16 * 1024

// Token represents a parsed JWT.
type Token struct {
	Raw       string
	Header    Header
	Claims    Claims
	Method    SigningMethod
	Signature []byte
	Valid     bool
}

// Header is the JOSE header of a JWT.
type Header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid,omitempty"`
	Typ string `json:"typ,omitempty"`
}

// Keyfunc is called during parsing to supply the key for signature verification.
type Keyfunc func(token *Token) (any, error)

// ParserOption configures a [Parser].
type ParserOption func(*Parser)

// WithValidMethods restricts which signing algorithms are accepted.
func WithValidMethods(methods []string) ParserOption {
	return func(p *Parser) {
		p.validMethods = make(map[string]struct{}, len(methods))
		for _, m := range methods {
			p.validMethods[m] = struct{}{}
		}
	}
}

// WithLeeway adds a time leeway for exp/nbf/iat validation.
func WithLeeway(d time.Duration) ParserOption {
	return func(p *Parser) {
		p.leeway = d
	}
}

// WithIssuer requires the token to have the specified issuer.
func WithIssuer(iss string) ParserOption {
	return func(p *Parser) {
		p.issuer = iss
	}
}

// WithAudience requires the token to contain the specified audience.
func WithAudience(aud string) ParserOption {
	return func(p *Parser) {
		p.audience = aud
	}
}

// WithSubject requires the token to have the specified subject.
func WithSubject(sub string) ParserOption {
	return func(p *Parser) {
		p.subject = sub
	}
}

// Parser is a JWT token parser.
type Parser struct {
	validMethods map[string]struct{}
	leeway       time.Duration
	issuer       string
	audience     string
	subject      string
}

// NewParser creates a new JWT parser with the given options.
func NewParser(opts ...ParserOption) *Parser {
	p := &Parser{}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Parse parses a token string into MapClaims.
func (p *Parser) Parse(tokenString string, keyFunc Keyfunc) (*Token, error) {
	return p.ParseWithClaims(tokenString, MapClaims{}, keyFunc)
}

// ParseWithClaims parses a token string and unmarshals claims into the provided Claims value.
func (p *Parser) ParseWithClaims(tokenString string, claims Claims, keyFunc Keyfunc) (*Token, error) {
	// 0. Reject oversized tokens.
	if len(tokenString) > maxTokenSize {
		return nil, ErrTokenMalformed
	}

	// 1. Find dots (zero-alloc splitting).
	dot1 := strings.IndexByte(tokenString, '.')
	if dot1 < 0 {
		return nil, ErrTokenMalformed
	}
	rest := tokenString[dot1+1:]
	dot2 := strings.IndexByte(rest, '.')
	if dot2 < 0 {
		return nil, ErrTokenMalformed
	}
	dot2 += dot1 + 1

	// Reject tokens with more than 3 segments.
	if strings.IndexByte(tokenString[dot2+1:], '.') >= 0 {
		return nil, ErrTokenMalformed
	}

	// 2. Decode header into stack buffer; fall back to heap for large headers.
	headerSeg := tokenString[:dot1]
	var headerBytes []byte
	var headerBuf [256]byte
	hn, err := base64Decode(headerBuf[:], headerSeg)
	if err != nil {
		if base64.RawURLEncoding.DecodedLen(len(headerSeg)) > len(headerBuf) {
			headerBytes, err = base64DecodeAlloc(headerSeg)
			if err != nil {
				return nil, ErrTokenMalformed
			}
		} else {
			return nil, ErrTokenMalformed
		}
	}
	if headerBytes == nil {
		headerBytes = headerBuf[:hn]
	}

	var header Header
	if err := parseHeader(headerBytes, &header); err != nil {
		return nil, ErrTokenMalformed
	}

	// 3. Reject alg:none.
	if header.Alg == "" || strings.EqualFold(header.Alg, "none") {
		return nil, ErrAlgNone
	}

	// 4. Look up signing method.
	method, ok := GetSigningMethod(header.Alg)
	if !ok {
		return nil, fmt.Errorf("%w: signing method %s is not registered", ErrTokenUnverifiable, header.Alg)
	}
	if p.validMethods != nil {
		if _, ok := p.validMethods[header.Alg]; !ok {
			return nil, fmt.Errorf("%w: signing method %s is not allowed", ErrTokenUnverifiable, header.Alg)
		}
	}

	// 5. Decode claims. Use stack buffer for MapClaims (typical JWT claims < 1KB);
	// fall back to heap allocation when the decoded payload exceeds the buffer.
	claimsSeg := tokenString[dot1+1 : dot2]
	if _, ok := claims.(MapClaims); ok {
		var claimsData []byte
		var claimsBuf [1024]byte
		cn, cerr := base64Decode(claimsBuf[:], claimsSeg)
		if cerr != nil {
			if base64.RawURLEncoding.DecodedLen(len(claimsSeg)) > len(claimsBuf) {
				claimsData, cerr = base64DecodeAlloc(claimsSeg)
				if cerr != nil {
					return nil, ErrTokenMalformed
				}
			} else {
				return nil, ErrTokenMalformed
			}
		}
		if claimsData == nil {
			claimsData = claimsBuf[:cn]
		}
		if err := unmarshalClaims(claimsData, claims); err != nil {
			return nil, ErrTokenMalformed
		}
	} else {
		claimsBytes, err := base64DecodeAlloc(claimsSeg)
		if err != nil {
			return nil, ErrTokenMalformed
		}
		if err := unmarshalClaims(claimsBytes, claims); err != nil {
			return nil, ErrTokenMalformed
		}
	}

	// 6. Decode signature into stack buffer (1024B covers RSA-4096 and future key sizes);
	// fall back to heap for unusually large signatures.
	sigSeg := tokenString[dot2+1:]
	var sigBytes []byte
	var sigBuf [1024]byte
	sn, err := base64Decode(sigBuf[:], sigSeg)
	if err != nil {
		if base64.RawURLEncoding.DecodedLen(len(sigSeg)) > len(sigBuf) {
			sigBytes, err = base64DecodeAlloc(sigSeg)
			if err != nil {
				return nil, ErrTokenMalformed
			}
		} else {
			return nil, ErrTokenMalformed
		}
	}
	if sigBytes == nil {
		sigBytes = sigBuf[:sn]
	}
	sig := sigBytes

	// 7. Build token, call keyfunc.
	token := &Token{
		Raw:    tokenString,
		Header: header,
		Claims: claims,
		Method: method,
	}

	key, err := keyFunc(token)
	if err != nil {
		return token, fmt.Errorf("%w: %w", ErrTokenUnverifiable, err)
	}

	// 8. Verify signature. signingInput is a substring of tokenString (zero alloc).
	signingInput := tokenString[:dot2]
	if err := method.Verify(signingInput, sig, key); err != nil {
		return token, fmt.Errorf("%w: %w", ErrTokenSignatureInvalid, err)
	}
	// 9. Validate claims with leeway support.
	if err := claims.Valid(); err != nil {
		if p.leeway > 0 && isTimeError(err) {
			if rerr := validateClaimsWithLeeway(claims, p.leeway); rerr != nil {
				return token, rerr
			}
		} else {
			return token, err
		}
	}

	// 10. Parser-level issuer/audience checks.
	if p.issuer != "" {
		if !p.checkIssuer(claims) {
			return token, ErrInvalidIssuer
		}
	}
	if p.audience != "" {
		if !p.checkAudience(claims) {
			return token, ErrInvalidAudience
		}
	}
	if p.subject != "" {
		if !p.checkSubject(claims) {
			return token, ErrInvalidSubject
		}
	}

	token.Valid = true
	return token, nil
}

func (p *Parser) checkIssuer(claims Claims) bool {
	switch c := claims.(type) {
	case *RegisteredClaims:
		return c.Issuer == p.issuer
	case RegisteredClaims:
		return c.Issuer == p.issuer
	case MapClaims:
		iss, _ := c["iss"].(string)
		return iss == p.issuer
	}
	return false
}

func (p *Parser) checkAudience(claims Claims) bool {
	switch c := claims.(type) {
	case *RegisteredClaims:
		for _, a := range c.Audience {
			if a == p.audience {
				return true
			}
		}
	case RegisteredClaims:
		for _, a := range c.Audience {
			if a == p.audience {
				return true
			}
		}
	case MapClaims:
		switch aud := c["aud"].(type) {
		case string:
			return aud == p.audience
		case []any:
			for _, a := range aud {
				if s, ok := a.(string); ok && s == p.audience {
					return true
				}
			}
		}
	}
	return false
}

func (p *Parser) checkSubject(claims Claims) bool {
	switch c := claims.(type) {
	case *RegisteredClaims:
		return c.Subject == p.subject
	case RegisteredClaims:
		return c.Subject == p.subject
	case MapClaims:
		sub, _ := c["sub"].(string)
		return sub == p.subject
	}
	return false
}

// SignToken creates a signed JWT string from a method, claims, and key.
func SignToken(method SigningMethod, claims Claims, key any) (string, error) {
	headerJSON, err := json.Marshal(Header{Alg: method.Alg(), Typ: "JWT"})
	if err != nil {
		return "", err
	}

	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	headerEnc := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsEnc := base64.RawURLEncoding.EncodeToString(claimsJSON)

	var b strings.Builder
	b.Grow(len(headerEnc) + 1 + len(claimsEnc) + 1 + base64.RawURLEncoding.EncodedLen(512))
	b.WriteString(headerEnc)
	b.WriteByte('.')
	b.WriteString(claimsEnc)
	signingInput := b.String()

	sig, err := method.Sign(signingInput, key)
	if err != nil {
		return "", err
	}

	b.WriteByte('.')
	b.WriteString(base64.RawURLEncoding.EncodeToString(sig))
	return b.String(), nil
}

// isTimeError returns true if the error is a time-related claims error.
func isTimeError(err error) bool {
	return errors.Is(err, ErrTokenExpired) ||
		errors.Is(err, ErrTokenNotValidYet) ||
		errors.Is(err, ErrTokenUsedBeforeIssued)
}

// validateClaimsWithLeeway re-validates time-based claims with a leeway applied.
func validateClaimsWithLeeway(claims Claims, leeway time.Duration) error {
	now := time.Now()
	switch c := claims.(type) {
	case *RegisteredClaims:
		if c.ExpiresAt != nil && now.After(c.ExpiresAt.Add(leeway)) {
			return ErrTokenExpired
		}
		if c.NotBefore != nil && now.Add(leeway).Before(c.NotBefore.Time) {
			return ErrTokenNotValidYet
		}
		if c.IssuedAt != nil && now.Add(leeway).Before(c.IssuedAt.Time) {
			return ErrTokenUsedBeforeIssued
		}
	case RegisteredClaims:
		if c.ExpiresAt != nil && now.After(c.ExpiresAt.Add(leeway)) {
			return ErrTokenExpired
		}
		if c.NotBefore != nil && now.Add(leeway).Before(c.NotBefore.Time) {
			return ErrTokenNotValidYet
		}
		if c.IssuedAt != nil && now.Add(leeway).Before(c.IssuedAt.Time) {
			return ErrTokenUsedBeforeIssued
		}
	case MapClaims:
		nowUnix := now.Unix()
		leewaySeconds := int64(leeway.Seconds())
		if exp, ok := c.numericClaim("exp"); ok {
			if nowUnix > exp+leewaySeconds {
				return ErrTokenExpired
			}
		}
		if nbf, ok := c.numericClaim("nbf"); ok {
			if nowUnix+leewaySeconds < nbf {
				return ErrTokenNotValidYet
			}
		}
		if iat, ok := c.numericClaim("iat"); ok {
			if nowUnix+leewaySeconds < iat {
				return ErrTokenUsedBeforeIssued
			}
		}
	default:
		// Unknown claims type; re-run Valid() which already failed.
		return claims.Valid()
	}
	return nil
}

// unmarshalClaims decodes JSON claims. For MapClaims it uses the minimal
// parseMapClaims (low alloc); for structured types it falls back to encoding/json.
func unmarshalClaims(data []byte, c Claims) error {
	if m, ok := c.(MapClaims); ok {
		return parseMapClaims(data, m)
	}
	return json.Unmarshal(data, c)
}

// base64Decode decodes base64url (no padding) into dst. Returns bytes written.
func base64Decode(dst []byte, src string) (int, error) {
	return base64.RawURLEncoding.Decode(dst, stringToBytes(src))
}

// base64DecodeAlloc decodes base64url (no padding), allocating the result.
func base64DecodeAlloc(src string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(src)
}
