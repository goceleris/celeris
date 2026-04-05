package jwt

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/jwt/internal/jwtparse"
)

// ErrUnauthorized is returned when authentication fails.
var ErrUnauthorized = celeris.NewHTTPError(401, "Unauthorized")

// ErrTokenMissing is returned when no token is found in the request.
var ErrTokenMissing = &celeris.HTTPError{Code: 401, Message: "Unauthorized", Err: errors.New("jwt: missing or malformed token")}

// ErrTokenInvalid is returned when the token is invalid or expired.
var ErrTokenInvalid = &celeris.HTTPError{Code: 401, Message: "Unauthorized", Err: errors.New("jwt: invalid or expired token")}

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

	keyFunc := resolveKeyFunc(cfg, customValidMethods)

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

		tokenStr := extractor(c)
		if tokenStr == "" {
			return handleError(c, ErrTokenMissing)
		}

		if tokenProcessor != nil {
			processed, err := tokenProcessor(tokenStr)
			if err != nil {
				return handleError(c, fmt.Errorf("%w: %w", ErrTokenInvalid, err))
			}
			tokenStr = processed
		}

		claims := newClaims(claimsFactory, claimsTemplate)
		token, err := parser.ParseWithClaims(tokenStr, claims, keyFunc)
		if err != nil || !token.Valid {
			wrappedErr := fmt.Errorf("%w: %w", ErrTokenInvalid, err)
			return handleError(c, wrappedErr)
		}

		c.Set(tokenCtxKey, token)
		c.Set(claimsCtxKey, token.Claims)

		c.OnRelease(func() {
			if v, ok := c.Get(tokenCtxKey); ok {
				if t, ok := v.(*jwtparse.Token); ok {
					if m, ok := t.Claims.(jwtparse.MapClaims); ok {
						jwtparse.ReleaseMapClaims(m)
					}
					jwtparse.ReleaseToken(t)
				}
			}
		})

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
