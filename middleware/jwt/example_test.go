package jwt_test

import (
	"time"

	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/jwt"
)

func ExampleNew() {
	// Simple HMAC authentication with a shared secret.
	_ = jwt.New(jwt.Config{
		SigningKey: []byte("my-secret-key"),
	})
}

func ExampleNew_signingKeys() {
	// Key rotation with kid-based key selection.
	_ = jwt.New(jwt.Config{
		SigningKeys: map[string]any{
			"key-1": []byte("secret-1"),
			"key-2": []byte("secret-2"),
		},
	})
}

func ExampleNew_jwks() {
	// JWKS auto-discovery (e.g. Auth0, Keycloak).
	_ = jwt.New(jwt.Config{
		JWKSURL:     "https://example.com/.well-known/jwks.json",
		JWKSRefresh: 30 * time.Minute,
	})
}

func ExampleNew_queryParam() {
	// Extract token from query parameter instead of header.
	_ = jwt.New(jwt.Config{
		SigningKey:  []byte("secret"),
		TokenLookup: "query:token",
	})
}

func ExampleNew_cookie() {
	// Extract token from a cookie.
	_ = jwt.New(jwt.Config{
		SigningKey:  []byte("secret"),
		TokenLookup: "cookie:jwt",
	})
}

func ExampleNew_multipleSources() {
	// Try header first, then query, then cookie.
	_ = jwt.New(jwt.Config{
		SigningKey:  []byte("secret"),
		TokenLookup: "header:Authorization:Bearer ,query:token,cookie:jwt",
	})
}

func ExampleNew_errorHandler() {
	// Custom error handler for JSON responses.
	_ = jwt.New(jwt.Config{
		SigningKey: []byte("secret"),
		ErrorHandler: func(c *celeris.Context, err error) error {
			return c.JSON(401, map[string]string{"error": err.Error()})
		},
	})
}

func ExampleNew_customKeyFunc() {
	// Full control over key resolution.
	_ = jwt.New(jwt.Config{
		SigningKey: []byte("unused"), // satisfies validation
		KeyFunc: func(t *jwt.Token) (any, error) {
			// Custom key lookup logic.
			return []byte("dynamic-secret"), nil
		},
	})
}

type MyClaims struct {
	jwt.RegisteredClaims
	Role string `json:"role"`
}

func ExampleNew_claimsFactory() {
	// Use ClaimsFactory for custom struct claims to avoid data races.
	_ = jwt.New(jwt.Config{
		SigningKey: []byte("secret"),
		ClaimsFactory: func() jwt.Claims {
			return &MyClaims{}
		},
	})
}

func ExampleNew_customContextKeys() {
	// Store token and claims under custom context keys.
	_ = jwt.New(jwt.Config{
		SigningKey:       []byte("secret"),
		TokenContextKey:  "my_token",
		ClaimsContextKey: "my_claims",
	})
}

func ExampleClaimsFromContext() {
	// Retrieve typed claims in a downstream handler.
	_ = func(c *celeris.Context) error {
		claims, ok := jwt.ClaimsFromContext[jwt.MapClaims](c)
		if !ok {
			return c.String(401, "no claims")
		}
		sub, _ := claims["sub"].(string)
		return c.String(200, "Hello %s", sub)
	}
}

func ExampleTokenFromContext() {
	// Retrieve the raw parsed token in a downstream handler.
	_ = func(c *celeris.Context) error {
		token := jwt.TokenFromContext(c)
		if token == nil {
			return c.String(401, "no token")
		}
		return c.String(200, "Algorithm: %s", token.Method.Alg())
	}
}

func ExampleNew_tokenProcessorFunc() {
	// JWE decryption: TokenProcessorFunc decrypts the outer JWE envelope
	// before the inner JWS is parsed and validated by the middleware.
	_ = jwt.New(jwt.Config{
		SigningKey: []byte("signing-secret"),
		TokenProcessorFunc: func(token string) (string, error) {
			// In a real application, this would decrypt a JWE compact
			// serialization (5-part dot-separated) using a private key
			// or shared symmetric key, returning the inner JWS token.
			//
			// Example with a JWE library (pseudocode):
			//   privKey := loadDecryptionKey()
			//   jws, err := jwe.Decrypt([]byte(token), privKey)
			//   return string(jws), err
			//
			// For this example, we simulate by stripping a "jwe:" prefix.
			if len(token) > 4 && token[:4] == "jwe:" {
				return token[4:], nil
			}
			return token, nil
		},
	})
}
