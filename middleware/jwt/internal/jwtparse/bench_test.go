package jwtparse

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"
)

func BenchmarkParseHS256(b *testing.B) {
	secret := []byte("test-secret-key-32-bytes-long!!!")
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}
	token, err := SignToken(SigningMethodHS256, claims, secret)
	if err != nil {
		b.Fatal(err)
	}

	p := NewParser(WithValidMethods([]string{"HS256"}))
	keyFunc := func(_ *Token) (any, error) { return secret, nil }

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.ParseWithClaims(token, MapClaims{}, keyFunc)
	}
}

func BenchmarkParseHS384(b *testing.B) {
	secret := []byte("test-secret-key-32-bytes-long!!!")
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}
	token, err := SignToken(SigningMethodHS384, claims, secret)
	if err != nil {
		b.Fatal(err)
	}

	p := NewParser(WithValidMethods([]string{"HS384"}))
	keyFunc := func(_ *Token) (any, error) { return secret, nil }

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.ParseWithClaims(token, MapClaims{}, keyFunc)
	}
}

func BenchmarkParseHS512(b *testing.B) {
	secret := []byte("test-secret-key-32-bytes-long!!!")
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}
	token, err := SignToken(SigningMethodHS512, claims, secret)
	if err != nil {
		b.Fatal(err)
	}

	p := NewParser(WithValidMethods([]string{"HS512"}))
	keyFunc := func(_ *Token) (any, error) { return secret, nil }

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.ParseWithClaims(token, MapClaims{}, keyFunc)
	}
}

func BenchmarkParseRS256(b *testing.B) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		b.Fatal(err)
	}
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}
	token, err := SignToken(SigningMethodRS256, claims, key)
	if err != nil {
		b.Fatal(err)
	}

	pub := &key.PublicKey
	p := NewParser(WithValidMethods([]string{"RS256"}))
	keyFunc := func(_ *Token) (any, error) { return pub, nil }

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.ParseWithClaims(token, MapClaims{}, keyFunc)
	}
}

func BenchmarkParseES256(b *testing.B) {
	key, err := GenerateECDSAKey(SigningMethodES256)
	if err != nil {
		b.Fatal(err)
	}
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}
	token, err := SignToken(SigningMethodES256, claims, key)
	if err != nil {
		b.Fatal(err)
	}

	pub := &key.PublicKey
	p := NewParser(WithValidMethods([]string{"ES256"}))
	keyFunc := func(_ *Token) (any, error) { return pub, nil }

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.ParseWithClaims(token, MapClaims{}, keyFunc)
	}
}

func BenchmarkParseEdDSA(b *testing.B) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}
	token, err := SignToken(SigningMethodEdDSA, claims, priv)
	if err != nil {
		b.Fatal(err)
	}

	p := NewParser(WithValidMethods([]string{"EdDSA"}))
	keyFunc := func(_ *Token) (any, error) { return pub, nil }

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.ParseWithClaims(token, MapClaims{}, keyFunc)
	}
}

func BenchmarkParseRegisteredClaims(b *testing.B) {
	secret := []byte("test-secret-key-32-bytes-long!!!")
	claims := &RegisteredClaims{
		Subject:   "1234",
		Issuer:    "bench",
		ExpiresAt: NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt:  NewNumericDate(time.Now()),
	}
	token, err := SignToken(SigningMethodHS256, claims, secret)
	if err != nil {
		b.Fatal(err)
	}

	p := NewParser(WithValidMethods([]string{"HS256"}))
	keyFunc := func(_ *Token) (any, error) { return secret, nil }

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.ParseWithClaims(token, &RegisteredClaims{}, keyFunc)
	}
}

// BenchmarkSignHS256 measures signing cost.
func BenchmarkSignHS256(b *testing.B) {
	secret := []byte("test-secret-key-32-bytes-long!!!")
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = SignToken(SigningMethodHS256, claims, secret)
	}
}

// Ensure parsed tokens are actually valid (guards against benchmarks silently measuring error paths).
func BenchmarkParseHS256_Validate(b *testing.B) {
	secret := []byte("test-secret-key-32-bytes-long!!!")
	claims := MapClaims{"sub": "1234", "exp": float64(time.Now().Add(time.Hour).Unix())}
	token, _ := SignToken(SigningMethodHS256, claims, secret)

	p := NewParser(WithValidMethods([]string{"HS256"}))
	keyFunc := func(_ *Token) (any, error) { return secret, nil }

	parsed, err := p.ParseWithClaims(token, MapClaims{}, keyFunc)
	if err != nil {
		b.Fatalf("token should parse: %v", err)
	}
	if !parsed.Valid {
		b.Fatal("token should be valid")
	}
}
