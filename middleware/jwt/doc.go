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
// Retrieve the validated token and claims in a handler:
//
//	token := jwt.TokenFromContext(c)
//	claims, ok := jwt.ClaimsFromContext[jwt.MapClaims](c)
//
// Key configuration options: [Config].TokenLookup controls where the token is
// extracted (comma-separated "source:name[:prefix]" pairs tried in order;
// default "header:Authorization:Bearer "). [Config].ClaimsFactory creates a
// fresh [Claims] instance per request for custom struct types. [Config].Skip
// and [Config].SkipPaths bypass the middleware dynamically or by exact path.
// Use [SignToken] to create signed tokens for testing or token issuance.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/middleware-auth
package jwt
