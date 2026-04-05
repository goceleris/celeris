package redirect

import (
	"strings"

	"github.com/goceleris/celeris"
)

// initRedirect parses config, applies defaults, validates, and returns
// the redirect code and initialized SkipHelper.
func initRedirect(config []Config) (int, celeris.SkipHelper) {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()
	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)
	return cfg.Code, skip
}

// initSkip parses config, applies defaults, validates, and returns an
// initialized SkipHelper. Used by rewrite functions that ignore Code.
func initSkip(config []Config) celeris.SkipHelper {
	_, skip := initRedirect(config)
	return skip
}

// buildRedirectURL constructs a redirect URL preserving the query string.
func buildRedirectURL(scheme, host, path, rawQuery string) string {
	if rawQuery != "" {
		return scheme + "://" + host + path + "?" + rawQuery
	}
	return scheme + "://" + host + path
}

// HTTPSRedirect redirects HTTP requests to HTTPS. HTTPS requests pass
// through to the next handler. The redirect URL preserves the original
// host, path, and query string.
//
// The default status code is 301 (Moved Permanently). Note that 301
// allows browsers to change the request method (e.g., POST becomes GET).
// Use Config{Code: 308} to preserve the original method.
func HTTPSRedirect(config ...Config) celeris.HandlerFunc {
	code, skip := initRedirect(config)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		if c.Scheme() == "https" {
			return c.Next()
		}

		host := c.Host()
		if host == "" {
			return c.Next()
		}

		return c.Redirect(code, buildRedirectURL("https", host, c.Path(), c.RawQuery()))
	}
}

// WWWRedirect redirects non-www requests to the www subdomain. Requests
// already on the www subdomain pass through to the next handler. The
// redirect URL preserves the original scheme, path, and query string.
//
// The default status code is 301 (Moved Permanently). Note that 301
// allows browsers to change the request method (e.g., POST becomes GET).
// Use Config{Code: 308} to preserve the original method.
func WWWRedirect(config ...Config) celeris.HandlerFunc {
	code, skip := initRedirect(config)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		host := c.Host()
		if host == "" {
			return c.Next()
		}

		if len(host) > 4 && strings.EqualFold(host[:4], "www.") {
			return c.Next()
		}

		return c.Redirect(code, buildRedirectURL(c.Scheme(), "www."+host, c.Path(), c.RawQuery()))
	}
}

// NonWWWRedirect redirects www requests to the non-www host. Requests
// without the www prefix pass through to the next handler. The redirect
// URL preserves the original scheme, path, and query string.
//
// The default status code is 301 (Moved Permanently). Note that 301
// allows browsers to change the request method (e.g., POST becomes GET).
// Use Config{Code: 308} to preserve the original method.
func NonWWWRedirect(config ...Config) celeris.HandlerFunc {
	code, skip := initRedirect(config)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		host := c.Host()
		if host == "" {
			return c.Next()
		}

		if len(host) <= 4 || !strings.EqualFold(host[:4], "www.") {
			return c.Next()
		}

		return c.Redirect(code, buildRedirectURL(c.Scheme(), host[4:], c.Path(), c.RawQuery()))
	}
}

// TrailingSlashRedirect adds a trailing slash to the request path when
// missing. The root path "/" and paths already ending with "/" pass
// through to the next handler.
//
// The default status code is 301 (Moved Permanently). Note that 301
// allows browsers to change the request method (e.g., POST becomes GET).
// Use Config{Code: 308} to preserve the original method.
func TrailingSlashRedirect(config ...Config) celeris.HandlerFunc {
	code, skip := initRedirect(config)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		path := c.Path()
		if path == "/" || (len(path) > 0 && path[len(path)-1] == '/') {
			return c.Next()
		}

		host := c.Host()
		if host == "" {
			return c.Next()
		}

		return c.Redirect(code, buildRedirectURL(c.Scheme(), host, path+"/", c.RawQuery()))
	}
}

// RemoveTrailingSlashRedirect strips a trailing slash from the request
// path. The root path "/" and paths without a trailing slash pass through
// to the next handler.
//
// The default status code is 301 (Moved Permanently). Note that 301
// allows browsers to change the request method (e.g., POST becomes GET).
// Use Config{Code: 308} to preserve the original method.
func RemoveTrailingSlashRedirect(config ...Config) celeris.HandlerFunc {
	code, skip := initRedirect(config)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		path := c.Path()
		if path == "/" || len(path) == 0 || path[len(path)-1] != '/' {
			return c.Next()
		}

		host := c.Host()
		if host == "" {
			return c.Next()
		}

		return c.Redirect(code, buildRedirectURL(c.Scheme(), host, path[:len(path)-1], c.RawQuery()))
	}
}

// HTTPSWWWRedirect redirects HTTP requests to HTTPS and non-www to www
// in a single redirect. Requests that are already on HTTPS with www pass
// through to the next handler.
//
// The default status code is 301 (Moved Permanently). Note that 301
// allows browsers to change the request method (e.g., POST becomes GET).
// Use Config{Code: 308} to preserve the original method.
func HTTPSWWWRedirect(config ...Config) celeris.HandlerFunc {
	code, skip := initRedirect(config)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		host := c.Host()
		if host == "" {
			return c.Next()
		}

		isHTTPS := c.Scheme() == "https"
		isWWW := len(host) > 4 && strings.EqualFold(host[:4], "www.")

		if isHTTPS && isWWW {
			return c.Next()
		}

		if !isWWW {
			host = "www." + host
		}

		return c.Redirect(code, buildRedirectURL("https", host, c.Path(), c.RawQuery()))
	}
}

// HTTPSNonWWWRedirect redirects HTTP requests to HTTPS and www to
// non-www in a single redirect. Requests that are already on HTTPS
// without www pass through to the next handler.
//
// The default status code is 301 (Moved Permanently). Note that 301
// allows browsers to change the request method (e.g., POST becomes GET).
// Use Config{Code: 308} to preserve the original method.
func HTTPSNonWWWRedirect(config ...Config) celeris.HandlerFunc {
	code, skip := initRedirect(config)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		host := c.Host()
		if host == "" {
			return c.Next()
		}

		isHTTPS := c.Scheme() == "https"
		isWWW := len(host) > 4 && strings.EqualFold(host[:4], "www.")

		if isHTTPS && !isWWW {
			return c.Next()
		}

		if isWWW {
			host = host[4:]
		}

		return c.Redirect(code, buildRedirectURL("https", host, c.Path(), c.RawQuery()))
	}
}

// TrailingSlashRewrite adds a trailing slash to the request path in-place.
// Unlike TrailingSlashRedirect, this does not send a redirect response --
// the request is processed with the modified path.
func TrailingSlashRewrite(config ...Config) celeris.HandlerFunc {
	skip := initSkip(config)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		path := c.Path()
		if path == "/" || (len(path) > 0 && path[len(path)-1] == '/') {
			return c.Next()
		}

		c.SetPath(path + "/")
		return c.Next()
	}
}

// RemoveTrailingSlashRewrite strips a trailing slash from the request
// path in-place. Unlike RemoveTrailingSlashRedirect, this does not send
// a redirect response -- the request is processed with the modified path.
func RemoveTrailingSlashRewrite(config ...Config) celeris.HandlerFunc {
	skip := initSkip(config)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		path := c.Path()
		if path == "/" || len(path) == 0 || path[len(path)-1] != '/' {
			return c.Next()
		}

		c.SetPath(path[:len(path)-1])
		return c.Next()
	}
}
