package jwtparse

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// helpers

func mustSignToken(t *testing.T, method SigningMethod, claims Claims, key any) string {
	t.Helper()
	tok, err := SignToken(method, claims, key)
	if err != nil {
		t.Fatalf("SignToken: %v", err)
	}
	return tok
}

func hmacKey() []byte {
	return []byte("test-secret-key-32-bytes-long!!!")
}

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return k
}

func ecKey(t *testing.T, m *signingMethodECDSA) *ecdsa.PrivateKey {
	t.Helper()
	k, err := GenerateECDSAKey(m)
	if err != nil {
		t.Fatalf("GenerateECDSAKey: %v", err)
	}
	return k
}

func edKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}

// HS256/384/512 round-trip

func TestHS256RoundTrip(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	p := NewParser(WithValidMethods([]string{"HS256"}))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
	if parsed.Header.Alg != "HS256" {
		t.Fatalf("alg = %q, want HS256", parsed.Header.Alg)
	}
}

func TestHS384RoundTrip(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodHS384, claims, key)

	p := NewParser(WithValidMethods([]string{"HS384"}))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

func TestHS512RoundTrip(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodHS512, claims, key)

	p := NewParser(WithValidMethods([]string{"HS512"}))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

// RSA round-trip

func TestRS256RoundTrip(t *testing.T) {
	key := rsaKey(t)
	claims := MapClaims{"sub": "rsa-user", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodRS256, claims, key)

	p := NewParser(WithValidMethods([]string{"RS256"}))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return &key.PublicKey, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

func TestRS384RoundTrip(t *testing.T) {
	key := rsaKey(t)
	claims := MapClaims{"sub": "rsa-user", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodRS384, claims, key)

	p := NewParser(WithValidMethods([]string{"RS384"}))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return &key.PublicKey, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

func TestRS512RoundTrip(t *testing.T) {
	key := rsaKey(t)
	claims := MapClaims{"sub": "rsa-user", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodRS512, claims, key)

	p := NewParser(WithValidMethods([]string{"RS512"}))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return &key.PublicKey, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

// ECDSA round-trip

func TestES256RoundTrip(t *testing.T) {
	key := ecKey(t, SigningMethodES256)
	claims := MapClaims{"sub": "ec-user", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodES256, claims, key)

	p := NewParser(WithValidMethods([]string{"ES256"}))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return &key.PublicKey, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

func TestES384RoundTrip(t *testing.T) {
	key := ecKey(t, SigningMethodES384)
	claims := MapClaims{"sub": "ec-user", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodES384, claims, key)

	p := NewParser(WithValidMethods([]string{"ES384"}))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return &key.PublicKey, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

func TestES512RoundTrip(t *testing.T) {
	key := ecKey(t, SigningMethodES512)
	claims := MapClaims{"sub": "ec-user", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodES512, claims, key)

	p := NewParser(WithValidMethods([]string{"ES512"}))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return &key.PublicKey, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

// EdDSA round-trip

func TestEdDSARoundTrip(t *testing.T) {
	pub, priv := edKey(t)
	claims := MapClaims{"sub": "ed-user", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodEdDSA, claims, priv)

	p := NewParser(WithValidMethods([]string{"EdDSA"}))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return pub, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

// alg:none rejection

func TestAlgNoneRejected(t *testing.T) {
	// Manually construct a token with alg:none.
	tok := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJzdWIiOiIxMjM0In0."
	p := NewParser()
	_, err := p.Parse(tok, func(_ *Token) (any, error) { return nil, nil })
	if !errors.Is(err, ErrAlgNone) {
		t.Fatalf("expected ErrAlgNone, got %v", err)
	}
}

func TestAlgNoneCaseInsensitive(t *testing.T) {
	// alg:"None" (mixed case) should also be rejected.
	tok := "eyJhbGciOiJOb25lIiwidHlwIjoiSldUIn0.eyJzdWIiOiIxMjM0In0."
	p := NewParser()
	_, err := p.Parse(tok, func(_ *Token) (any, error) { return nil, nil })
	if !errors.Is(err, ErrAlgNone) {
		t.Fatalf("expected ErrAlgNone, got %v", err)
	}
}

// Expired token

func TestExpiredToken(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{
		"sub": "1234",
		"exp": float64(time.Now().Add(-time.Hour).Unix()),
	}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	p := NewParser()
	_, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
}

// Not-yet-valid token

func TestNotValidYet(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{
		"sub": "1234",
		"nbf": float64(time.Now().Add(time.Hour).Unix()),
		"exp": float64(time.Now().Add(2 * time.Hour).Unix()),
	}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	p := NewParser()
	_, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if !errors.Is(err, ErrTokenNotValidYet) {
		t.Fatalf("expected ErrTokenNotValidYet, got %v", err)
	}
}

// Malformed tokens

func TestMalformedTokens(t *testing.T) {
	p := NewParser()
	kf := func(_ *Token) (any, error) { return hmacKey(), nil }

	cases := []string{
		"",
		"abc",
		"abc.def",
		"abc.def.ghi.jkl",
		"abc.def.ghi.jkl.mno",
	}
	for _, tc := range cases {
		_, err := p.Parse(tc, kf)
		if !errors.Is(err, ErrTokenMalformed) {
			t.Errorf("token %q: expected ErrTokenMalformed, got %v", tc, err)
		}
	}
}

// MapClaims validation

func TestMapClaimsIssuedInFuture(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{
		"sub": "1234",
		"iat": float64(time.Now().Add(time.Hour).Unix()),
		"exp": float64(time.Now().Add(2 * time.Hour).Unix()),
	}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	p := NewParser()
	_, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if !errors.Is(err, ErrTokenUsedBeforeIssued) {
		t.Fatalf("expected ErrTokenUsedBeforeIssued, got %v", err)
	}
}

// RegisteredClaims validation

func TestRegisteredClaimsValid(t *testing.T) {
	key := hmacKey()
	claims := &RegisteredClaims{
		Subject:   "1234",
		ExpiresAt: NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt:  NewNumericDate(time.Now().Add(-time.Minute)),
	}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	p := NewParser()
	rc := &RegisteredClaims{}
	parsed, err := p.ParseWithClaims(tok, rc, func(_ *Token) (any, error) { return key, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
	if rc.Subject != "1234" {
		t.Fatalf("subject = %q, want 1234", rc.Subject)
	}
}

func TestRegisteredClaimsExpired(t *testing.T) {
	key := hmacKey()
	claims := &RegisteredClaims{
		Subject:   "1234",
		ExpiresAt: NewNumericDate(time.Now().Add(-time.Hour)),
	}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	p := NewParser()
	_, err := p.ParseWithClaims(tok, &RegisteredClaims{}, func(_ *Token) (any, error) { return key, nil })
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
}

func TestRegisteredClaimsNotValidYet(t *testing.T) {
	key := hmacKey()
	claims := &RegisteredClaims{
		Subject:   "1234",
		NotBefore: NewNumericDate(time.Now().Add(time.Hour)),
		ExpiresAt: NewNumericDate(time.Now().Add(2 * time.Hour)),
	}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	p := NewParser()
	_, err := p.ParseWithClaims(tok, &RegisteredClaims{}, func(_ *Token) (any, error) { return key, nil })
	if !errors.Is(err, ErrTokenNotValidYet) {
		t.Fatalf("expected ErrTokenNotValidYet, got %v", err)
	}
}

// Audience: string vs []string

func TestAudienceString(t *testing.T) {
	b := []byte(`"myapp"`)
	var a Audience
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(a) != 1 || a[0] != "myapp" {
		t.Fatalf("audience = %v, want [myapp]", a)
	}
}

func TestAudienceArray(t *testing.T) {
	b := []byte(`["app1","app2"]`)
	var a Audience
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(a) != 2 || a[0] != "app1" || a[1] != "app2" {
		t.Fatalf("audience = %v, want [app1 app2]", a)
	}
}

// NumericDate JSON

func TestNumericDateUnmarshal(t *testing.T) {
	b := []byte(`1609459200`)
	var nd NumericDate
	if err := json.Unmarshal(b, &nd); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	expected := time.Unix(1609459200, 0)
	if nd.Unix() != expected.Unix() {
		t.Fatalf("time = %v, want %v", nd.Time, expected)
	}
}

func TestNumericDateMarshal(t *testing.T) {
	nd := NewNumericDate(time.Unix(1609459200, 0))
	b, err := json.Marshal(nd)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != "1609459200" {
		t.Fatalf("json = %q, want 1609459200", string(b))
	}
}

// WithValidMethods restriction

func TestWithValidMethodsRejects(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	p := NewParser(WithValidMethods([]string{"RS256"}))
	_, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if !errors.Is(err, ErrTokenUnverifiable) {
		t.Fatalf("expected ErrTokenUnverifiable, got %v", err)
	}
}

// Header kid extraction

func TestHeaderKid(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	// Our SignToken doesn't set kid, so parse and check it's empty.
	p := NewParser()
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Header.Kid != "" {
		t.Fatalf("kid = %q, want empty", parsed.Header.Kid)
	}
}

// Wrong signing key

func TestWrongSigningKey(t *testing.T) {
	key1 := []byte("key-one-32-bytes-long!!!!!!!!!!!")
	key2 := []byte("key-two-32-bytes-long!!!!!!!!!!!")
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodHS256, claims, key1)

	p := NewParser()
	_, err := p.Parse(tok, func(_ *Token) (any, error) { return key2, nil })
	if !errors.Is(err, ErrTokenSignatureInvalid) {
		t.Fatalf("expected ErrTokenSignatureInvalid, got %v", err)
	}
}

// Issuer validation

func TestWithIssuer(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{
		"sub": "1234",
		"iss": "myissuer",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	p := NewParser(WithIssuer("myissuer"))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

func TestWithIssuerRejects(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{
		"sub": "1234",
		"iss": "wrong",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	p := NewParser(WithIssuer("myissuer"))
	_, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if !errors.Is(err, ErrInvalidIssuer) {
		t.Fatalf("expected ErrInvalidIssuer, got %v", err)
	}
}

// Audience validation

func TestWithAudience(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{
		"sub": "1234",
		"aud": "myapp",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	p := NewParser(WithAudience("myapp"))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

func TestWithAudienceRejects(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{
		"sub": "1234",
		"aud": "otherapp",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	p := NewParser(WithAudience("myapp"))
	_, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if !errors.Is(err, ErrInvalidAudience) {
		t.Fatalf("expected ErrInvalidAudience, got %v", err)
	}
}

// Keyfunc error

func TestKeyfuncError(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	p := NewParser()
	_, err := p.Parse(tok, func(_ *Token) (any, error) { return nil, errors.New("no key") })
	if !errors.Is(err, ErrTokenUnverifiable) {
		t.Fatalf("expected ErrTokenUnverifiable, got %v", err)
	}
}

// No exp claim still validates (exp is optional)

func TestNoExpClaim(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{"sub": "1234"}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	p := NewParser()
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

// Signing method registry

func TestGetSigningMethodRegistered(t *testing.T) {
	for _, alg := range []string{"HS256", "HS384", "HS512", "RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "EdDSA", "PS256", "PS384", "PS512"} {
		m, ok := GetSigningMethod(alg)
		if !ok {
			t.Fatalf("signing method %s not registered", alg)
		}
		if m.Alg() != alg {
			t.Fatalf("Alg() = %q, want %q", m.Alg(), alg)
		}
	}
}

func TestGetSigningMethodUnknown(t *testing.T) {
	_, ok := GetSigningMethod("UNKNOWN")
	if ok {
		t.Fatal("expected unknown method to not be found")
	}
}

// RSA-PSS round-trip tests

func TestPS256RoundTrip(t *testing.T) {
	key := rsaKey(t)
	claims := MapClaims{"sub": "pss-user", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodPS256, claims, key)

	p := NewParser(WithValidMethods([]string{"PS256"}))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return &key.PublicKey, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
	if parsed.Header.Alg != "PS256" {
		t.Fatalf("alg = %q, want PS256", parsed.Header.Alg)
	}
}

func TestPS384RoundTrip(t *testing.T) {
	key := rsaKey(t)
	claims := MapClaims{"sub": "pss-user", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodPS384, claims, key)

	p := NewParser(WithValidMethods([]string{"PS384"}))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return &key.PublicKey, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

func TestPS512RoundTrip(t *testing.T) {
	key := rsaKey(t)
	claims := MapClaims{"sub": "pss-user", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodPS512, claims, key)

	p := NewParser(WithValidMethods([]string{"PS512"}))
	parsed, err := p.Parse(tok, func(_ *Token) (any, error) { return &key.PublicKey, nil })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

// PSS wrong key type

func TestPSSWrongKeyType(t *testing.T) {
	key := rsaKey(t)
	claims := MapClaims{"sub": "pss-user", "exp": float64(time.Now().Add(time.Hour).Unix())}
	tok := mustSignToken(t, SigningMethodPS256, claims, key)

	p := NewParser(WithValidMethods([]string{"PS256"}))
	_, err := p.Parse(tok, func(_ *Token) (any, error) { return []byte("wrong-key-type"), nil })
	if !errors.Is(err, ErrTokenUnverifiable) {
		t.Fatalf("expected ErrTokenUnverifiable, got %v", err)
	}
}

// WithLeeway tests

func TestWithLeewayAllowsRecentlyExpired(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{
		"sub": "1234",
		"exp": float64(time.Now().Add(-5 * time.Second).Unix()),
	}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	// Without leeway: should fail.
	p := NewParser()
	_, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("without leeway: expected ErrTokenExpired, got %v", err)
	}

	// With 1 minute leeway: should succeed.
	p2 := NewParser(WithLeeway(time.Minute))
	parsed, err := p2.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if err != nil {
		t.Fatalf("with leeway: unexpected error: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("with leeway: token should be valid")
	}
}

func TestWithLeewayAllowsNotYetValidWithinLeeway(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{
		"sub": "1234",
		"nbf": float64(time.Now().Add(5 * time.Second).Unix()),
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	// Without leeway: should fail.
	p := NewParser()
	_, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if !errors.Is(err, ErrTokenNotValidYet) {
		t.Fatalf("without leeway: expected ErrTokenNotValidYet, got %v", err)
	}

	// With 1 minute leeway: should succeed.
	p2 := NewParser(WithLeeway(time.Minute))
	parsed, err := p2.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if err != nil {
		t.Fatalf("with leeway: unexpected error: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("with leeway: token should be valid")
	}
}

func TestWithLeewayRegisteredClaims(t *testing.T) {
	key := hmacKey()
	claims := &RegisteredClaims{
		Subject:   "1234",
		ExpiresAt: NewNumericDate(time.Now().Add(-5 * time.Second)),
	}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	// Without leeway: should fail.
	p := NewParser()
	_, err := p.ParseWithClaims(tok, &RegisteredClaims{}, func(_ *Token) (any, error) { return key, nil })
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("without leeway: expected ErrTokenExpired, got %v", err)
	}

	// With 1 minute leeway: should succeed.
	p2 := NewParser(WithLeeway(time.Minute))
	parsed, err := p2.ParseWithClaims(tok, &RegisteredClaims{}, func(_ *Token) (any, error) { return key, nil })
	if err != nil {
		t.Fatalf("with leeway: unexpected error: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("with leeway: token should be valid")
	}
}

func TestWithLeewayStillRejectsFarExpired(t *testing.T) {
	key := hmacKey()
	claims := MapClaims{
		"sub": "1234",
		"exp": float64(time.Now().Add(-time.Hour).Unix()),
	}
	tok := mustSignToken(t, SigningMethodHS256, claims, key)

	// 1 minute leeway should NOT save a 1-hour-expired token.
	p := NewParser(WithLeeway(time.Minute))
	_, err := p.Parse(tok, func(_ *Token) (any, error) { return key, nil })
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired for far-expired token, got %v", err)
	}
}

// Max token size test

func TestMaxTokenSizeRejected(t *testing.T) {
	// Construct a token string exceeding 16KB.
	oversized := make([]byte, 17*1024)
	for i := range oversized {
		oversized[i] = 'a'
	}
	// Place dots so it looks like 3 segments.
	oversized[100] = '.'
	oversized[200] = '.'

	p := NewParser()
	_, err := p.Parse(string(oversized), func(_ *Token) (any, error) { return hmacKey(), nil })
	if !errors.Is(err, ErrTokenMalformed) {
		t.Fatalf("expected ErrTokenMalformed for oversized token, got %v", err)
	}
}

// JSON recursion depth limit test

func TestJSONRecursionDepthLimit(t *testing.T) {
	// Build deeply nested JSON: {"a":{"a":{"a":...}}}
	depth := 40 // exceeds the 32-level limit
	data := make([]byte, 0, depth*6+2)
	for i := 0; i < depth; i++ {
		data = append(data, `{"a":`...)
	}
	data = append(data, "1"...)
	for i := 0; i < depth; i++ {
		data = append(data, '}')
	}

	m := MapClaims{}
	err := parseMapClaims(data, m)
	if err == nil {
		t.Fatal("expected error for deeply nested JSON, got nil")
	}
}
