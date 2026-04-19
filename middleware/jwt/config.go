package jwt

import (
	"fmt"
	"log"
	"net/url"
	"time"

	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/internal/extract"
	"github.com/goceleris/celeris/middleware/jwt/internal/jwtparse"
	"github.com/goceleris/celeris/middleware/store"
)

// TokenKey is the context store key for the parsed *jwt.Token.
const TokenKey = "jwt_token"

// ClaimsKey is the context store key for the token's claims.
const ClaimsKey = "jwt_claims"

// Config defines the JWT middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// SigningKey is the key used to verify tokens. For HMAC this is a
	// []byte secret; for RSA/ECDSA this is the public key.
	SigningKey any

	// SigningKeys maps kid (Key ID) to verification key for key rotation.
	// When set, the token's "kid" header selects the key.
	SigningKeys map[string]any

	// SigningMethod is the expected signing algorithm.
	// Default: jwt.SigningMethodHS256.
	SigningMethod jwtparse.SigningMethod

	// ValidMethods restricts the allowed signing algorithms.
	// Default: []string{SigningMethod.Alg()}.
	ValidMethods []string

	// TokenLookup defines where to extract the token. Comma-separated
	// sources are tried in order. Format: "source:name[:prefix]".
	// Supported sources: header, query, cookie, form, param.
	// Default: "header:Authorization:Bearer ".
	TokenLookup string

	// Claims is the template for the claims type. The middleware clones
	// this for each request via [cloneClaims]. For built-in types
	// ([MapClaims], *[RegisteredClaims]) this is zero-cost. For custom
	// struct types, reflect.New is used to create a fresh zero-value
	// instance -- the template's field values are NOT copied. For
	// maximum control and to avoid reflect overhead, use ClaimsFactory
	// instead. Default: jwt.MapClaims{}.
	Claims jwtparse.Claims

	// ClaimsFactory creates a fresh Claims instance per request. When set,
	// this takes precedence over the Claims template field. Use this for
	// custom struct claims types to avoid data races.
	ClaimsFactory func() jwtparse.Claims

	// KeyFunc is a custom key function that overrides SigningKey/SigningKeys.
	KeyFunc jwtparse.Keyfunc

	// JWKSURL is a URL for JWKS auto-discovery (e.g. Auth0, Keycloak).
	// When JWKSURLs is also set, this URL is prepended to that list.
	JWKSURL string

	// JWKSURLs is a list of JWKS URLs for multi-provider federation.
	// When a token's kid is not found in one provider's keyset, the
	// next URL's keys are tried in order. If JWKSURL is also set, it
	// is treated as the first entry.
	JWKSURLs []string

	// JWKSRefresh is the interval for re-fetching the JWKS keyset.
	// Default: 1h.
	JWKSRefresh time.Duration

	// JWKSPreload controls whether JWKS keys are fetched eagerly at
	// middleware creation time. When nil (default) or pointing to true,
	// Preload is enabled and the first request avoids a blocking network
	// call. Set to a pointer to false to disable preloading (lazy fetch
	// on first request). If preload fails, a warning is logged and the
	// middleware falls back to lazy fetching.
	JWKSPreload *bool

	// JWKSCache, when set, caches the raw JWKS JSON response keyed by
	// the JWKS URL. On refresh, the middleware checks the cache before
	// issuing an HTTP GET and writes back on successful fetch, reducing
	// JWKS endpoint load in multi-instance deployments. Cache TTL equals
	// JWKSRefresh. All cache failures are non-fatal.
	//
	// Typical use: pass [middleware/jwt/jwtcache.New](redisClient).
	JWKSCache store.KV

	// TokenProcessorFunc is called on the raw token string after extraction
	// but before parsing. Use it for decryption, decompression, or other
	// transformations. If it returns an error, the middleware returns
	// ErrTokenInvalid wrapping that error.
	//
	// The returned string must be a valid compact JWS (3-part
	// dot-separated). Returning an empty string or a non-JWS value will
	// cause a parse error. The original raw token is NOT preserved;
	// Token.Raw will contain the transformed value.
	TokenProcessorFunc func(token string) (string, error)

	// TokenContextKey overrides the context store key for the parsed token.
	// Default: "jwt_token" (TokenKey).
	TokenContextKey string

	// ClaimsContextKey overrides the context store key for the claims.
	// Default: "jwt_claims" (ClaimsKey).
	ClaimsContextKey string

	// ParseOptions provides additional parser options (e.g., WithLeeway,
	// WithIssuer, WithAudience) appended after the default ValidMethods
	// option. This allows fine-grained control over the JWT parser without
	// needing a custom KeyFunc.
	ParseOptions []jwtparse.ParserOption

	// ErrorHandler handles authentication failures. Receives the error
	// and returns the error to propagate. Default: returns the error as-is.
	ErrorHandler func(c *celeris.Context, err error) error

	// SuccessHandler is called after successful token validation, before
	// c.Next(). Use for logging, metrics, or enriching the context.
	SuccessHandler func(c *celeris.Context)

	// ContinueOnIgnoredError, when true, causes the middleware to call
	// c.Next() when ErrorHandler returns nil. By default (false), when
	// ErrorHandler returns nil the middleware returns nil immediately
	// without calling c.Next(), short-circuiting the handler chain.
	// Note: when false and ErrorHandler returns nil, the middleware
	// itself returns nil (not the original error). The framework's
	// outer handler loop then runs downstream handlers independently;
	// errors from those handlers are NOT propagated through this
	// middleware's return value. Enable ContinueOnIgnoredError to
	// explicitly call c.Next() and propagate downstream errors.
	ContinueOnIgnoredError bool
}

// defaultConfig is the default JWT configuration.
var defaultConfig = Config{
	SigningMethod: jwtparse.SigningMethodHS256,
	TokenLookup:   "header:Authorization:Bearer ",
	JWKSRefresh:   time.Hour,
}

func applyDefaults(cfg Config) Config {
	if cfg.SigningMethod == nil {
		cfg.SigningMethod = defaultConfig.SigningMethod
	}
	if cfg.TokenLookup == "" {
		cfg.TokenLookup = defaultConfig.TokenLookup
	}
	if cfg.Claims == nil && cfg.ClaimsFactory == nil {
		cfg.Claims = jwtparse.MapClaims{}
	}
	if cfg.JWKSRefresh == 0 {
		cfg.JWKSRefresh = defaultConfig.JWKSRefresh
	}
	if cfg.TokenContextKey == "" {
		cfg.TokenContextKey = TokenKey
	}
	if cfg.ClaimsContextKey == "" {
		cfg.ClaimsContextKey = ClaimsKey
	}
	return cfg
}

func (cfg Config) validate() {
	if cfg.SigningKey == nil && len(cfg.SigningKeys) == 0 && cfg.KeyFunc == nil && cfg.JWKSURL == "" && len(cfg.JWKSURLs) == 0 {
		panic("jwt: one of SigningKey, SigningKeys, KeyFunc, JWKSURL, or JWKSURLs is required")
	}
	if cfg.JWKSURL != "" {
		validateJWKSURL(cfg.JWKSURL)
	}
	for _, u := range cfg.JWKSURLs {
		validateJWKSURL(u)
	}
	validateHMACKeyLength(cfg)
}

// hmacMinKeyLengths maps HMAC algorithm names to their recommended minimum
// key lengths in bytes (matching the hash output size per RFC 2104).
var hmacMinKeyLengths = map[string]int{
	"HS256": 32,
	"HS384": 48,
	"HS512": 64,
}

func validateHMACKeyLength(cfg Config) {
	keyBytes, ok := cfg.SigningKey.([]byte)
	if !ok || cfg.SigningKey == nil {
		return
	}
	alg := cfg.SigningMethod.Alg()
	recommended, isHMAC := hmacMinKeyLengths[alg]
	if !isHMAC {
		return
	}
	if len(keyBytes) < recommended {
		log.Printf("jwt: WARNING: %s signing key is %d bytes, recommended minimum is %d bytes", alg, len(keyBytes), recommended)
	}
}

// validateJWKSURL enforces HTTPS for JWKS URLs, allowing HTTP only for
// localhost addresses (127.0.0.1, ::1, localhost) for development.
func validateJWKSURL(rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(fmt.Sprintf("jwt: invalid JWKS URL: %v", err))
	}
	if u.Scheme == "https" {
		return
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return
		}
	}
	panic("jwt: JWKS URL must use HTTPS (HTTP allowed only for localhost)")
}

// parseTokenLookup parses the TokenLookup config string into an extractor.
func parseTokenLookup(lookup string) extract.Func {
	return extract.Parse(lookup)
}

// resolveKeyFunc builds the key function and returns any JWKS fetchers
// created so the caller can preload them. When customValidMethods is true,
// the caller has set ValidMethods explicitly, so the parser already enforces
// algorithm restrictions and the key function should not duplicate that check.
func resolveKeyFunc(cfg Config, customValidMethods bool) (jwtparse.Keyfunc, []*jwksFetcher) {
	if cfg.KeyFunc != nil {
		return cfg.KeyFunc, nil
	}
	jwksURLs := buildJWKSURLs(cfg)
	if len(jwksURLs) > 0 {
		fetchers := make([]*jwksFetcher, len(jwksURLs))
		for i, u := range jwksURLs {
			fetchers[i] = newJWKSFetcher(u, cfg.JWKSRefresh)
			if cfg.JWKSCache != nil {
				fetchers[i].cache = cfg.JWKSCache
			}
		}
		if len(fetchers) == 1 {
			return fetchers[0].keyFunc, fetchers
		}
		return multiJWKSKeyFunc(fetchers), fetchers
	}
	if len(cfg.SigningKeys) > 0 {
		return func(t *jwtparse.Token) (any, error) {
			kid := t.Header.Kid
			key, ok := cfg.SigningKeys[kid]
			if !ok {
				return nil, ErrTokenInvalid
			}
			return key, nil
		}, nil
	}
	if customValidMethods {
		return func(_ *jwtparse.Token) (any, error) {
			return cfg.SigningKey, nil
		}, nil
	}
	return func(t *jwtparse.Token) (any, error) {
		if t.Method.Alg() != cfg.SigningMethod.Alg() {
			return nil, fmt.Errorf("jwt: unexpected signing method: %v", t.Header.Alg)
		}
		return cfg.SigningKey, nil
	}, nil
}

// buildJWKSURLs merges JWKSURL and JWKSURLs into a single deduplicated list.
func buildJWKSURLs(cfg Config) []string {
	if cfg.JWKSURL == "" && len(cfg.JWKSURLs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, 1+len(cfg.JWKSURLs))
	urls := make([]string, 0, 1+len(cfg.JWKSURLs))
	if cfg.JWKSURL != "" {
		seen[cfg.JWKSURL] = struct{}{}
		urls = append(urls, cfg.JWKSURL)
	}
	for _, u := range cfg.JWKSURLs {
		if _, dup := seen[u]; !dup {
			seen[u] = struct{}{}
			urls = append(urls, u)
		}
	}
	return urls
}

// multiJWKSKeyFunc creates a key function that tries multiple JWKS fetchers
// in order. The first fetcher that returns a key for the token's kid wins.
func multiJWKSKeyFunc(fetchers []*jwksFetcher) jwtparse.Keyfunc {
	return func(t *jwtparse.Token) (any, error) {
		var lastErr error
		for _, f := range fetchers {
			key, err := f.keyFunc(t)
			if err == nil {
				return key, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}
