package jwt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"testing"

	"github.com/goceleris/celeris/middleware/jwt/internal/jwtparse"
)

// Golden test vectors for algorithm verification.
// The HS256 case uses the well-known jwt.io example token. RS256 and ES512
// use generated keys for round-trip verification of non-deterministic algorithms.

func TestGoldenVectors(t *testing.T) {
	// HS256 golden vector: the canonical jwt.io example.
	// Header:  {"alg":"HS256","typ":"JWT"}
	// Payload: {"sub":"1234567890","name":"John Doe","iat":1516239022}
	// Key:     "your-256-bit-secret"
	hs256Token := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
		"eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ." +
		"SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	hs256Key := []byte("your-256-bit-secret")

	// RS256 round-trip: generate a key pair and sign.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rs256Claims := jwtparse.MapClaims{
		"sub":   "1234567890",
		"name":  "John Doe",
		"admin": true,
		"iat":   float64(1516239022),
	}
	rs256Token := signTokenWithMethod(jwtparse.SigningMethodRS256, rs256Claims, rsaKey)

	// ES512 round-trip: generate a key pair and sign.
	ecKey, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	es512Claims := jwtparse.MapClaims{
		"sub":  "es512-user",
		"name": "ES512 Test",
		"iat":  float64(1516239022),
	}
	es512Token := signTokenWithMethod(jwtparse.SigningMethodES512, es512Claims, ecKey)

	tests := []struct {
		name       string
		token      string
		method     jwtparse.SigningMethod
		key        any
		wantSub    string
		wantName   string
		wantClaims map[string]any
	}{
		{
			name:     "HS256/jwt.io-example",
			token:    hs256Token,
			method:   jwtparse.SigningMethodHS256,
			key:      hs256Key,
			wantSub:  "1234567890",
			wantName: "John Doe",
		},
		{
			name:     "RS256/generated-key",
			token:    rs256Token,
			method:   jwtparse.SigningMethodRS256,
			key:      &rsaKey.PublicKey,
			wantSub:  "1234567890",
			wantName: "John Doe",
			wantClaims: map[string]any{
				"admin": true,
			},
		},
		{
			name:     "ES512/generated-key",
			token:    es512Token,
			method:   jwtparse.SigningMethodES512,
			key:      &ecKey.PublicKey,
			wantSub:  "es512-user",
			wantName: "ES512 Test",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parser := jwtparse.NewParser(jwtparse.WithValidMethods([]string{tc.method.Alg()}))
			parsed, err := parser.ParseWithClaims(tc.token, jwtparse.MapClaims{}, func(_ *jwtparse.Token) (any, error) {
				return tc.key, nil
			})
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			if !parsed.Valid {
				t.Fatal("token not valid")
			}

			mc, ok := parsed.Claims.(jwtparse.MapClaims)
			if !ok {
				t.Fatalf("claims type: got %T, want MapClaims", parsed.Claims)
			}

			if sub, _ := mc["sub"].(string); sub != tc.wantSub {
				t.Fatalf("sub: got %q, want %q", sub, tc.wantSub)
			}
			if name, _ := mc["name"].(string); name != tc.wantName {
				t.Fatalf("name: got %q, want %q", name, tc.wantName)
			}
			if parsed.Method.Alg() != tc.method.Alg() {
				t.Fatalf("alg: got %q, want %q", parsed.Method.Alg(), tc.method.Alg())
			}
			for k, want := range tc.wantClaims {
				got := mc[k]
				if got != want {
					t.Fatalf("claim %q: got %v, want %v", k, got, want)
				}
			}
		})
	}
}
