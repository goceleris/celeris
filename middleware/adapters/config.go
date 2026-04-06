package adapters

import "net/http"

// Option configures a [ReverseProxy].
type Option func(*proxyConfig)

type proxyConfig struct {
	transport     http.RoundTripper
	modifyRequest func(*http.Request)
	errorHandler  func(http.ResponseWriter, *http.Request, error)
}

// WithTransport sets the transport used by the reverse proxy.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *proxyConfig) { c.transport = rt }
}

// WithModifyRequest registers a function that mutates the outbound request
// before it is sent to the target.
func WithModifyRequest(f func(*http.Request)) Option {
	return func(c *proxyConfig) { c.modifyRequest = f }
}

// WithErrorHandler registers a function that handles proxy errors (e.g.,
// connection refused, timeout). If not set, the default
// httputil.ReverseProxy error handler is used.
func WithErrorHandler(f func(http.ResponseWriter, *http.Request, error)) Option {
	return func(c *proxyConfig) { c.errorHandler = f }
}
