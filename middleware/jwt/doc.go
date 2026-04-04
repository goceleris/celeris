// Package jwt provides JSON Web Token authentication middleware for celeris.
//
// The middleware extracts a JWT from a configurable source (header, query
// parameter, cookie, form field, or URL parameter), validates it using HMAC,
// RSA, ECDSA, EdDSA, or RSA-PSS keys, and stores the parsed token and claims
// in the context store under [TokenKey] and [ClaimsKey]. Failed
// authentication returns 401.
//
// All public types ([Token], [Claims], [MapClaims], [RegisteredClaims],
// [SigningMethod], etc.) are re-exported from the internal parser.
//
// Simple usage with a symmetric HMAC secret:
//
//	server.Use(jwt.New(jwt.Config{
//	    SigningKey: []byte("my-secret-key"),
//	}))
//
// JWKS auto-discovery (e.g. Auth0, Keycloak):
//
//	server.Use(jwt.New(jwt.Config{
//	    JWKSURL:     "https://example.com/.well-known/jwks.json",
//	    JWKSRefresh: 30 * time.Minute,
//	}))
//
// # Retrieving Token and Claims
//
//	token := jwt.TokenFromContext(c)
//	claims, ok := jwt.ClaimsFromContext[jwt.MapClaims](c)
//
// # Token Lookup
//
// [Config].TokenLookup supports comma-separated sources tried in order.
// Format: "source:name[:prefix]". Sources: header, query, cookie, form, param.
//
// # Custom Claims
//
// Use [Config].ClaimsFactory for custom struct claims types:
//
//	jwt.New(jwt.Config{
//	    SigningKey: secret,
//	    ClaimsFactory: func() jwt.Claims { return &MyClaims{} },
//	})
//
// # Creating Tokens
//
//	token, err := jwt.SignToken(jwt.SigningMethodHS256, jwt.MapClaims{
//	    "sub": "user-123",
//	    "exp": float64(time.Now().Add(time.Hour).Unix()),
//	}, []byte("secret"))
//
// # Security Best Practices
//
// Always set "exp" on issued tokens. Use "aud" with [WithAudience] to
// prevent token confusion. Prefer asymmetric algorithms (RS256, ES256) in
// multi-service deployments. Store keys in environment variables or a
// secrets manager. Serve JWKS endpoints over HTTPS.
//
// # Algorithm-Key-Type Binding
//
//   - HS256/HS384/HS512: []byte
//   - RS256/RS384/RS512/PS256/PS384/PS512: *rsa.PublicKey / *rsa.PrivateKey
//   - ES256/ES384/ES512: *ecdsa.PublicKey / *ecdsa.PrivateKey
//   - EdDSA: ed25519.PublicKey / ed25519.PrivateKey
//
// # Skipping
//
// Set [Config].Skip to bypass dynamically, or [Config].SkipPaths for
// exact-match path exclusions.
package jwt
