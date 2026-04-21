package jwt

import (
	"errors"
	"fmt"
	"log"
	"reflect"

	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/jwt/internal/jwtparse"
)

// ErrUnauthorized is returned when authentication fails.
// ErrUnauthorized aliases [celeris.ErrUnauthorized] so cross-package
// errors.Is checks work (e.g. mixed jwt+keyauth stacks).
var ErrUnauthorized = celeris.ErrUnauthorized

// ErrTokenMissing is returned when no token is found in the request.
var ErrTokenMissing = &celeris.HTTPError{Code: 401, Message: "Unauthorized", Err: errors.New("jwt: missing or malformed token")}

// ErrTokenInvalid is returned when the token fails validation for reasons
// other than expiration or malformation (e.g., bad signature, unknown kid).
var ErrTokenInvalid = &celeris.HTTPError{Code: 401, Message: "Unauthorized", Err: errors.New("jwt: invalid or expired token")}

// ErrJWTExpired is returned when the token's exp claim is in the past.
var ErrJWTExpired = &celeris.HTTPError{Code: 401, Message: "Unauthorized", Err: errors.New("jwt: token has expired")}

// ErrJWTMalformed is returned when the token cannot be parsed (bad
// encoding, wrong number of segments, etc.).
var ErrJWTMalformed = &celeris.HTTPError{Code: 401, Message: "Unauthorized", Err: errors.New("jwt: token is malformed")}

// New creates a JWT authentication middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	skipMap := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skipMap[p] = struct{}{}
	}

	extractor := parseTokenLookup(cfg.TokenLookup)
	claimsTemplate := cfg.Claims
	claimsFactory := cfg.ClaimsFactory

	customValidMethods := len(cfg.ValidMethods) > 0
	validMethods := cfg.ValidMethods
	if !customValidMethods {
		validMethods = []string{cfg.SigningMethod.Alg()}
	}

	keyFunc, jwksFetchers := resolveKeyFunc(cfg, customValidMethods)

	if len(jwksFetchers) > 0 && (cfg.JWKSPreload == nil || *cfg.JWKSPreload) {
		preloadJWKS(jwksFetchers)
	}

	tokenCtxKey := cfg.TokenContextKey
	claimsCtxKey := cfg.ClaimsContextKey

	parserOpts := make([]jwtparse.ParserOption, 0, 1+len(cfg.ParseOptions))
	parserOpts = append(parserOpts, jwtparse.WithValidMethods(validMethods))
	parserOpts = append(parserOpts, cfg.ParseOptions...)
	parser := jwtparse.NewParser(parserOpts...)

	errorHandler := cfg.ErrorHandler
	if errorHandler == nil {
		errorHandler = func(c *celeris.Context, err error) error {
			c.SetHeader("www-authenticate", "Bearer")
			c.SetHeader("cache-control", "no-store")
			return err
		}
	}
	successHandler := cfg.SuccessHandler
	tokenProcessor := cfg.TokenProcessorFunc
	continueOnIgnored := cfg.ContinueOnIgnoredError

	handleError := func(c *celeris.Context, err error) error {
		result := errorHandler(c, err)
		if result == nil && continueOnIgnored {
			return c.Next()
		}
		return result
	}

	return func(c *celeris.Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}

		if _, ok := skipMap[c.Path()]; ok {
			return c.Next()
		}

		// Defensive OPTIONS skip: CORS preflight must not be auth-blocked
		// regardless of middleware-installation order. If cors runs before
		// jwt (the recommended order), preflights short-circuit before
		// reaching here. If a misconfiguration puts auth ahead of cors,
		// this lets the preflight through to cors.
		if c.Method() == "OPTIONS" {
			return c.Next()
		}

		tokenStr := extractor(c)
		if tokenStr == "" {
			return handleError(c, ErrTokenMissing)
		}

		if tokenProcessor != nil {
			processed, err := tokenProcessor(tokenStr)
			if err != nil {
				return handleError(c, &classifiedError{outer: ErrTokenInvalid, inner: err})
			}
			tokenStr = processed
		}

		claims := newClaims(claimsFactory, claimsTemplate)
		token, err := parser.ParseWithClaims(tokenStr, claims, keyFunc)
		if err != nil || !token.Valid {
			// Release pooled token and claims on the error path so they
			// return to their pools. token.Claims aliases claims when
			// token != nil, so the MapClaims release happens exactly
			// once either way (claims is always the live reference).
			if m, ok := claims.(jwtparse.MapClaims); ok {
				jwtparse.ReleaseMapClaims(m)
			}
			if token != nil {
				jwtparse.ReleaseToken(token)
			}
			wrappedErr := classifyTokenError(err)
			return handleError(c, wrappedErr)
		}

		c.Set(tokenCtxKey, token)
		c.Set(claimsCtxKey, token.Claims)

		// Using the pre-bound method value instead of an anonymous
		// closure saves 1 alloc per request (full closure captures c and
		// escapes via cross-package OnRelease).
		c.OnRelease(token.OnReleaseFn())

		if successHandler != nil {
			successHandler(c)
		}

		return c.Next()
	}
}

// TokenFromContext returns the parsed JWT token from the context store
// using the default TokenKey. Returns nil if no token was stored.
// If the middleware was configured with a custom TokenContextKey,
// use TokenFromContextWithKey instead.
func TokenFromContext(c *celeris.Context) *jwtparse.Token {
	return TokenFromContextWithKey(c, TokenKey)
}

// TokenFromContextWithKey returns the parsed JWT token from the context
// store using the specified key. Returns nil if no token was stored.
func TokenFromContextWithKey(c *celeris.Context, key string) *jwtparse.Token {
	v, ok := c.Get(key)
	if !ok {
		return nil
	}
	t, _ := v.(*jwtparse.Token)
	return t
}

// ClaimsFromContext returns the token claims from the context store, typed
// to the requested claims type T, using the default ClaimsKey.
// Returns the zero value and false if no claims were stored or the type
// assertion fails. If the middleware was configured with a custom
// ClaimsContextKey, use ClaimsFromContextWithKey instead.
func ClaimsFromContext[T jwtparse.Claims](c *celeris.Context) (T, bool) {
	return ClaimsFromContextWithKey[T](c, ClaimsKey)
}

// ClaimsFromContextWithKey returns the token claims from the context store,
// typed to the requested claims type T, using the specified key.
// Returns the zero value and false if no claims were stored or the type
// assertion fails.
func ClaimsFromContextWithKey[T jwtparse.Claims](c *celeris.Context, key string) (T, bool) {
	v, ok := c.Get(key)
	if !ok {
		var zero T
		return zero, false
	}
	claims, ok := v.(T)
	return claims, ok
}

// newClaims creates a fresh claims instance for each request.
// If a factory is provided, it is called directly.
// Otherwise, known types get a zero-value clone; unknown struct types
// use reflect.New to avoid returning the shared template pointer.
func newClaims(factory func() jwtparse.Claims, template jwtparse.Claims) jwtparse.Claims {
	if factory != nil {
		return factory()
	}
	return cloneClaims(template)
}

// cloneClaims creates a fresh claims instance matching the template type.
// MapClaims gets a new empty map; RegisteredClaims gets a new zero struct.
// For other struct types, reflect.New creates a fresh zero-value struct.
// Non-struct, non-map types that do not implement Claims via a pointer
// receiver will panic -- use ClaimsFactory for such types.
func cloneClaims(template jwtparse.Claims) jwtparse.Claims {
	switch template.(type) {
	case jwtparse.MapClaims:
		return jwtparse.AcquireMapClaims()
	case *jwtparse.RegisteredClaims:
		return &jwtparse.RegisteredClaims{}
	default:
		t := reflect.TypeOf(template)
		if t.Kind() == reflect.Ptr {
			if t.Elem().Kind() != reflect.Struct {
				panic(fmt.Sprintf("jwt: unsupported custom claims type %T; use ClaimsFactory for non-struct pointer types", template))
			}
			return reflect.New(t.Elem()).Interface().(jwtparse.Claims)
		}
		if t.Kind() != reflect.Struct && t.Kind() != reflect.Map {
			panic(fmt.Sprintf("jwt: unsupported custom claims type %T; use ClaimsFactory for non-struct types", template))
		}
		return reflect.New(t).Interface().(jwtparse.Claims)
	}
}

// classifiedError pairs an HTTPError sentinel (outer) with the underlying
// parser error (inner). Replaces fmt.Errorf("%w: %w", ...) — fmt.Errorf
// allocates the formatter state, the message string, the wrapErrors
// struct, and a []error unwrap slice per call; classifiedError is one
// allocation. Error() materializes the concat lazily, so callers who only
// errors.Is-check the result never pay for the string.
type classifiedError struct {
	outer error
	inner error // nil when classifyTokenError receives a nil err
}

func (c *classifiedError) Error() string {
	if c.inner == nil {
		return c.outer.Error()
	}
	return c.outer.Error() + ": " + c.inner.Error()
}

// Is reports whether target matches outer or inner. Obviates Unwrap()
// []error (which would require a per-call slice alloc) for errors.Is.
func (c *classifiedError) Is(target error) bool {
	if errors.Is(c.outer, target) {
		return true
	}
	return c.inner != nil && errors.Is(c.inner, target)
}

// Unwrap returns outer so errors.As can reach the HTTPError sentinel.
func (c *classifiedError) Unwrap() error { return c.outer }

// staticInner is the fixed "token not valid" message used by the
// err == nil branch of classifyTokenError. Pre-built once so the branch
// matches the other three (one struct alloc, no string alloc).
var staticInner = errors.New("token not valid")

// classifyTokenError inspects the parser error and wraps it with the
// appropriate HTTPError sentinel so callers can distinguish expired
// tokens from malformed ones without inspecting internal parse errors.
func classifyTokenError(err error) error {
	if err == nil {
		return &classifiedError{outer: ErrTokenInvalid, inner: staticInner}
	}
	if errors.Is(err, jwtparse.ErrTokenExpired) || errors.Is(err, jwtparse.ErrTokenNotValidYet) || errors.Is(err, jwtparse.ErrTokenUsedBeforeIssued) {
		return &classifiedError{outer: ErrJWTExpired, inner: err}
	}
	if errors.Is(err, jwtparse.ErrTokenMalformed) || errors.Is(err, jwtparse.ErrTokenUnverifiable) {
		return &classifiedError{outer: ErrJWTMalformed, inner: err}
	}
	return &classifiedError{outer: ErrTokenInvalid, inner: err}
}

// preloadJWKS eagerly fetches JWKS keys at startup when enabled.
func preloadJWKS(fetchers []*jwksFetcher) {
	for _, f := range fetchers {
		if err := f.Preload(); err != nil {
			log.Printf("jwt: JWKS preload from %s failed (will lazy-fetch on first request): %v", f.url, err)
		}
	}
}
