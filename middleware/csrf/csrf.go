package csrf

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"

	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/internal/extract"
	"github.com/goceleris/celeris/middleware/internal/randutil"
	"github.com/goceleris/celeris/middleware/store"
)

// ContextKey is the default context store key for the CSRF token.
const ContextKey = "csrf_token"

// handlerContextKey is the context store key for the Handler instance.
const handlerContextKey = "csrf_handler"

// Sentinel errors returned by the CSRF middleware. These are package-level
// variables for use with errors.Is. Do not reassign them.
var (
	// ErrForbidden is returned when the submitted CSRF token does not match
	// the cookie token.
	ErrForbidden = celeris.NewHTTPError(403, "Forbidden")

	// ErrMissingToken is returned when the CSRF token is absent from the
	// cookie or the request source.
	ErrMissingToken = &celeris.HTTPError{Code: 403, Message: "Forbidden", Err: errors.New("csrf: missing token")}

	// ErrTokenNotFound is returned by DeleteToken when no token exists.
	ErrTokenNotFound = &celeris.HTTPError{Code: 404, Message: "Not Found", Err: errors.New("csrf: token not found")}

	// ErrOriginMismatch is returned when the Origin header does not match
	// the request host or any trusted origin.
	ErrOriginMismatch = &celeris.HTTPError{Code: 403, Message: "Forbidden", Err: errors.New("csrf: origin does not match")}

	// ErrRefererMissing is returned when the Referer header is absent on an
	// HTTPS request with no Origin header.
	ErrRefererMissing = &celeris.HTTPError{Code: 403, Message: "Forbidden", Err: errors.New("csrf: referer header missing")}

	// ErrRefererMismatch is returned when the Referer header does not match
	// the request host or any trusted origin.
	ErrRefererMismatch = &celeris.HTTPError{Code: 403, Message: "Forbidden", Err: errors.New("csrf: referer does not match")}

	// ErrSecFetchSite is returned when the Sec-Fetch-Site header value is
	// "cross-site", indicating a cross-site request.
	ErrSecFetchSite = &celeris.HTTPError{Code: 403, Message: "Forbidden", Err: errors.New("csrf: sec-fetch-site is cross-site")}
)

// tokenExtractor extracts a token string from the request.
// CSRF only uses header, form, and query sources, but the shared extract
// package supports all sources (cookie, param) for generality.
type tokenExtractor = extract.Func

// Handler provides methods for managing CSRF tokens for a specific
// middleware configuration. Retrieved via HandlerFromContext.
//
// The fields are copied from Config at construction time rather than
// holding a Config reference. This is intentional for immutability:
// the caller cannot mutate Handler state after New() returns, and the
// middleware closure and Handler always see consistent values without
// synchronization.
type Handler struct {
	storage        store.KV
	tokenLen       int
	cookieName     string
	cookiePath     string
	cookieDomain   string
	cookieSecure   bool
	cookieHTTPOnly bool
	cookieSameSite celeris.SameSite
	contextKey     string
}

// DeleteToken removes the CSRF token from server-side storage and
// expires the cookie. Useful for logout flows. Returns ErrTokenNotFound
// if no token exists in the cookie.
func (h *Handler) DeleteToken(c *celeris.Context) error {
	cookieToken, err := c.Cookie(h.cookieName)
	if err != nil || cookieToken == "" {
		return ErrTokenNotFound
	}
	if h.storage != nil {
		_ = h.storage.Delete(c.Context(), storageKey(cookieToken))
	}
	c.SetCookie(&celeris.Cookie{
		Name:     h.cookieName,
		Value:    "",
		Path:     h.cookiePath,
		Domain:   h.cookieDomain,
		MaxAge:   -1,
		Secure:   h.cookieSecure,
		HTTPOnly: h.cookieHTTPOnly,
		SameSite: h.cookieSameSite,
	})
	return nil
}

// New creates a CSRF middleware with the given config.
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

	safeMap := make(map[string]struct{}, len(cfg.SafeMethods))
	for _, m := range cfg.SafeMethods {
		safeMap[m] = struct{}{}
	}

	// Parse trusted origins: separate exact matches from wildcard patterns.
	trustedOrigins := make(map[string]struct{})
	var wildcardOrigins []wildcardOrigin
	for _, o := range cfg.TrustedOrigins {
		if strings.Contains(o, "*") {
			idx := strings.Index(o, "*")
			wildcardOrigins = append(wildcardOrigins, wildcardOrigin{
				prefix:            o[:idx],
				suffix:            o[idx+1:],
				maxSubdomainDepth: 1,
			})
		} else {
			trustedOrigins[o] = struct{}{}
		}
	}

	extractor := buildExtractor(cfg.TokenLookup)
	tokenLen := cfg.TokenLength
	cookieName := cfg.CookieName
	cookiePath := cfg.CookiePath
	cookieDomain := cfg.CookieDomain
	cookieMaxAge := cfg.CookieMaxAge
	cookieSecure := cfg.CookieSecure
	cookieHTTPOnly := cfg.CookieHTTPOnly
	cookieSameSite := cfg.CookieSameSite
	contextKey := cfg.ContextKey
	storage := cfg.Storage
	expiration := cfg.Expiration
	singleUse := cfg.SingleUseToken
	keyGen := cfg.KeyGenerator

	errorHandler := cfg.ErrorHandler
	if errorHandler == nil {
		errorHandler = func(_ *celeris.Context, err error) error { return err }
	}

	genToken := func() string {
		if keyGen != nil {
			return keyGen()
		}
		return randutil.HexToken(tokenLen)
	}

	handler := &Handler{
		storage:        storage,
		tokenLen:       tokenLen,
		cookieName:     cookieName,
		cookiePath:     cookiePath,
		cookieDomain:   cookieDomain,
		cookieSecure:   cookieSecure,
		cookieHTTPOnly: cookieHTTPOnly,
		cookieSameSite: cookieSameSite,
		contextKey:     contextKey,
	}

	setCookie := func(c *celeris.Context, token string) {
		cookie := &celeris.Cookie{
			Name:     cookieName,
			Value:    token,
			Path:     cookiePath,
			Domain:   cookieDomain,
			MaxAge:   cookieMaxAge,
			Secure:   cookieSecure,
			HTTPOnly: cookieHTTPOnly,
			SameSite: cookieSameSite,
		}
		if c.IsTLS() || c.Scheme() == "https" {
			cookie.Secure = true
		}
		c.SetCookie(cookie)
	}

	return func(c *celeris.Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}

		if _, ok := skipMap[c.Path()]; ok {
			return c.Next()
		}

		c.Set(handlerContextKey, handler)

		_, safe := safeMap[c.Method()]

		if safe {
			token, err := c.Cookie(cookieName)
			newToken := false
			if err != nil || token == "" {
				token = genToken()
				newToken = true
			} else if storage != nil {
				if _, gerr := storage.Get(c.Context(), storageKey(token)); errors.Is(gerr, store.ErrNotFound) {
					token = genToken()
					newToken = true
				}
			}
			if storage != nil && newToken {
				_ = storage.Set(c.Context(), storageKey(token), []byte(token), expiration)
			}
			setCookie(c, token)
			c.AddHeader("vary", "Cookie")
			c.Set(contextKey, token)
			return c.Next()
		}

		// Defense-in-depth: reject cross-site requests via Sec-Fetch-Site.
		if sfs := c.Header("sec-fetch-site"); sfs == "cross-site" {
			return errorHandler(c, ErrSecFetchSite)
		}

		// Defense-in-depth: validate Origin header against request host.
		origin := c.Header("origin")
		if origin != "" {
			if !isOriginAllowed(origin, c.Host(), trustedOrigins, wildcardOrigins) {
				return errorHandler(c, ErrOriginMismatch)
			}
		}

		// Referer fallback when Origin is absent on HTTPS.
		if origin == "" && c.Scheme() == "https" {
			referer := c.Header("referer")
			if referer == "" {
				return errorHandler(c, ErrRefererMissing)
			}
			refOrigin := extractOrigin(referer)
			if refOrigin == "" {
				return errorHandler(c, ErrRefererMismatch)
			}
			if !isOriginAllowed(refOrigin, c.Host(), trustedOrigins, wildcardOrigins) {
				return errorHandler(c, ErrRefererMismatch)
			}
		}

		cookieToken, err := c.Cookie(cookieName)
		if err != nil || cookieToken == "" {
			return errorHandler(c, ErrMissingToken)
		}

		// Cookie-injection defense: validate the cookie value is a
		// well-formed hex token. An attacker with subdomain cookie-
		// setting ability could inject a malformed value; rejecting
		// non-hex or wrong-length values limits the attack surface.
		if !isValidTokenHex(cookieToken, tokenLen) {
			return errorHandler(c, ErrForbidden)
		}

		requestToken := extractor(c)
		if requestToken == "" {
			return errorHandler(c, ErrMissingToken)
		}

		if storage != nil {
			key := storageKey(cookieToken)
			var storedToken []byte
			var gerr error
			if singleUse {
				if gd, ok := storage.(store.GetAndDeleter); ok {
					storedToken, gerr = gd.GetAndDelete(c.Context(), key)
				} else {
					// Non-atomic fallback: TOCTOU-susceptible when multiple
					// requests arrive with the same token concurrently.
					storedToken, gerr = storage.Get(c.Context(), key)
					if gerr == nil {
						_ = storage.Delete(c.Context(), key)
					}
				}
			} else {
				storedToken, gerr = storage.Get(c.Context(), key)
			}
			if errors.Is(gerr, store.ErrNotFound) {
				return errorHandler(c, ErrForbidden)
			}
			if gerr != nil {
				return errorHandler(c, ErrForbidden)
			}
			if subtle.ConstantTimeCompare(storedToken, []byte(requestToken)) != 1 {
				return errorHandler(c, ErrForbidden)
			}
		} else {
			if subtle.ConstantTimeCompare([]byte(cookieToken), []byte(requestToken)) != 1 {
				return errorHandler(c, ErrForbidden)
			}
		}

		c.AddHeader("vary", "Cookie")
		c.Set(contextKey, cookieToken)
		return c.Next()
	}
}

// isValidTokenHex checks that a cookie token value is a well-formed hex
// string of the expected length (2*tokenByteLen chars). This rejects
// injected or corrupted cookie values early, before any cryptographic
// comparison.
func isValidTokenHex(token string, tokenByteLen int) bool {
	n := len(token)
	if n != tokenByteLen*2 {
		return false
	}
	for i := 0; i < n; i++ {
		c := token[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// isOriginAllowed checks if the request origin matches the host, an exact
// trusted origin, or a wildcard trusted origin pattern.
func isOriginAllowed(origin, host string, trusted map[string]struct{}, wildcards []wildcardOrigin) bool {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	originHost := parsed.Host
	if originHost == "" {
		return false
	}
	if strings.EqualFold(originHost, host) {
		return true
	}
	if _, ok := trusted[origin]; ok {
		return true
	}
	for i := range wildcards {
		if wildcards[i].match(origin) {
			return true
		}
	}
	return false
}

// extractOrigin parses a URL and returns scheme + "://" + host.
// Returns an empty string if parsing fails or the host is absent.
func extractOrigin(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// storageKey returns a SHA-256 hex digest of the token for use as a storage key.
// This prevents leaking raw tokens through the storage backend.
func storageKey(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// TokenFromContext returns the CSRF token from the context store.
// Returns an empty string if no token was stored (e.g., skipped request).
// When the middleware is configured with a custom ContextKey, the handler's
// key is tried first, falling back to the default ContextKey constant.
func TokenFromContext(c *celeris.Context) string {
	if h := handlerFromContext(c); h != nil {
		v, ok := c.Get(h.contextKey)
		if ok {
			s, _ := v.(string)
			return s
		}
	}
	v, ok := c.Get(ContextKey)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// HandlerFromContext returns the Handler associated with the CSRF middleware
// from the request context. Returns nil if the middleware has not run.
func HandlerFromContext(c *celeris.Context) *Handler {
	return handlerFromContext(c)
}

func handlerFromContext(c *celeris.Context) *Handler {
	v, ok := c.Get(handlerContextKey)
	if !ok {
		return nil
	}
	h, _ := v.(*Handler)
	return h
}

// DeleteToken removes the CSRF token from server-side storage and expires
// the cookie. This is a convenience wrapper around HandlerFromContext.
// Returns ErrTokenNotFound if no token or handler exists.
func DeleteToken(c *celeris.Context) error {
	h := handlerFromContext(c)
	if h == nil {
		return ErrTokenNotFound
	}
	return h.DeleteToken(c)
}

// buildExtractor parses the TokenLookup config string into an extractor.
func buildExtractor(lookup string) tokenExtractor {
	return extract.Parse(lookup)
}
